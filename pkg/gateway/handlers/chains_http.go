package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	assetsvc "github.com/vango-go/vai-lite/pkg/gateway/assets"
	chainrt "github.com/vango-go/vai-lite/pkg/gateway/chains"
	"github.com/vango-go/vai-lite/pkg/gateway/compat"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
	"github.com/vango-go/vai-lite/pkg/gateway/limits"
	"github.com/vango-go/vai-lite/pkg/gateway/mw"
	"github.com/vango-go/vai-lite/pkg/gateway/principal"
	"github.com/vango-go/vai-lite/pkg/gateway/ratelimit"
	"github.com/vango-go/vai-lite/pkg/gateway/sse"
)

const idempotencyKeyHeader = "Idempotency-Key"

type ChainsHandler struct {
	Config     config.Config
	Upstreams  ProviderFactory
	HTTPClient *http.Client
	Logger     *slog.Logger
	Limiter    *ratelimit.Limiter
	Lifecycle  *lifecycle.Lifecycle
	Chains     *chainrt.Manager
	Assets     *assetsvc.Service
}

func (h ChainsHandler) getConfig() config.Config          { return h.Config }
func (h ChainsHandler) getHTTPClient() *http.Client       { return h.HTTPClient }
func (h ChainsHandler) getUpstreams() ProviderFactory     { return h.Upstreams }
func (h ChainsHandler) getChainManager() *chainrt.Manager { return h.Chains }
func (h ChainsHandler) getAssets() *assetsvc.Service      { return h.Assets }

func (h ChainsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/chains":
		h.serveChainsRoot(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/chains/"):
		h.serveChainPath(w, r)
	default:
		NotFoundHandler{}.ServeHTTP(w, r)
	}
}

func (h ChainsHandler) serveChainsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.createChain(w, r)
	case http.MethodGet:
		h.listChains(w, r)
	default:
		writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
	}
}

func (h ChainsHandler) serveChainPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/chains/")
	path = strings.Trim(path, "/")
	if path == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	if strings.HasSuffix(path, ":fork") {
		chainID := strings.TrimSpace(strings.TrimSuffix(path, ":fork"))
		if chainID == "" {
			NotFoundHandler{}.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
			return
		}
		h.forkChain(w, r, chainID)
		return
	}
	parts := strings.Split(path, "/")
	chainID := strings.TrimSpace(parts[0])
	if chainID == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.getChain(w, r, chainID)
		case http.MethodPatch:
			h.patchChain(w, r, chainID)
		default:
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
		}
		return
	}
	switch parts[1] {
	case "context":
		if r.Method != http.MethodGet {
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
			return
		}
		h.getChainContext(w, r, chainID)
	case "runs":
		switch r.Method {
		case http.MethodGet:
			h.listChainRuns(w, r, chainID)
		case http.MethodPost:
			h.runChainBlocking(w, r, chainID)
		default:
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
		}
	case "runs:stream":
		if r.Method != http.MethodPost {
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
			return
		}
		h.runChainStream(w, r, chainID)
	default:
		NotFoundHandler{}.ServeHTTP(w, r)
	}
}

func (h ChainsHandler) createChain(w http.ResponseWriter, r *http.Request) {
	if h.Chains == nil {
		writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestError("chain runtime is not configured"), http.StatusServiceUnavailable)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required"))
		return
	}
	body, ok := readBoundedBody(w, r, h.Config.MaxBodyBytes)
	if !ok {
		return
	}
	payload, err := types.UnmarshalChainStartPayloadStrict(body)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	if err := h.validateChainMessageRequest(payload.Defaults, payload.History); err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	env := chainEnv(h, pr, r.Header, types.AttachmentModeStatefulHTTP, "http", false, nil)
	ctx := withHandlerTimeout(r.Context(), h.Config.HandlerTimeout)
	event, _, err := h.Chains.StartChain(ctx, pr, env, *payload, idempotencyKey)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, event)
}

func (h ChainsHandler) patchChain(w http.ResponseWriter, r *http.Request, chainID string) {
	if h.Chains == nil {
		writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestError("chain runtime is not configured"), http.StatusServiceUnavailable)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required").WithChain(chainID))
		return
	}
	body, ok := readBoundedBody(w, r, h.Config.MaxBodyBytes)
	if !ok {
		return
	}
	payload, err := types.UnmarshalChainUpdatePayloadStrict(body)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	record, err := h.Chains.GetChainForMutation(r.Context(), pr, chainID)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	if err := h.validateChainMessageRequest(mergeChainDefaults(record.Defaults, payload.Defaults), nil); err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	env := chainEnv(h, pr, r.Header, types.AttachmentModeStatefulHTTP, "http", false, nil)
	event, err := h.Chains.UpdateChain(withHandlerTimeout(r.Context(), h.Config.HandlerTimeout), chainID, env, *payload, idempotencyKey)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, event)
}

func (h ChainsHandler) forkChain(w http.ResponseWriter, r *http.Request, chainID string) {
	if h.Chains == nil {
		writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestError("chain runtime is not configured"), http.StatusServiceUnavailable)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required").WithChain(chainID))
		return
	}
	body, ok := readBoundedBody(w, r, h.Config.MaxBodyBytes)
	if !ok {
		return
	}
	var payload types.ChainForkRequest
	if err := decodeStrictJSON(body, &payload); err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	preview, err := h.Chains.PreviewForkChain(r.Context(), pr, chainID, payload)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	if err := h.validateChainMessageRequest(preview.Defaults, preview.History); err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	resp, err := h.Chains.ForkChain(withHandlerTimeout(r.Context(), h.Config.HandlerTimeout), pr, chainID, payload, idempotencyKey)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, resp)
}

func (h ChainsHandler) getChain(w http.ResponseWriter, r *http.Request, chainID string) {
	pr := chainPrincipalFromRequest(r, h.Config)
	record, err := h.Chains.GetChainAuthorized(r.Context(), pr, chainID)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, record)
}

func (h ChainsHandler) getChainContext(w http.ResponseWriter, r *http.Request, chainID string) {
	pr := chainPrincipalFromRequest(r, h.Config)
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" || format == "messages" {
		resp, err := h.Chains.GetChainContextAuthorized(r.Context(), pr, chainID)
		if err != nil {
			h.writeChainError(w, requestID(r), err)
			return
		}
		resp.AtRunID = strings.TrimSpace(r.URL.Query().Get("at_run_id"))
		writeJSON(w, resp)
		return
	}
	if format == "timeline" {
		runs, err := h.Chains.ListRunsAuthorized(r.Context(), pr, chainID)
		if err != nil {
			h.writeChainError(w, requestID(r), err)
			return
		}
		items := make([]types.RunTimelineItem, 0)
		for i := range runs {
			timeline, timelineErr := h.Chains.GetRunTimelineAuthorized(r.Context(), pr, runs[i].ID)
			if timelineErr != nil {
				h.writeChainError(w, requestID(r), timelineErr)
				return
			}
			items = append(items, timeline.Items...)
		}
		writeJSON(w, &types.ChainContextResponse{
			ChainID:  chainID,
			AtRunID:  strings.TrimSpace(r.URL.Query().Get("at_run_id")),
			Timeline: items,
		})
		return
	}
	writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestErrorWithParam("unsupported context format", "format"), http.StatusBadRequest)
}

func (h ChainsHandler) listChainRuns(w http.ResponseWriter, r *http.Request, chainID string) {
	pr := chainPrincipalFromRequest(r, h.Config)
	runs, err := h.Chains.ListRunsAuthorized(r.Context(), pr, chainID)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, &types.ChainRunList{Items: runs})
}

func (h ChainsHandler) listChains(w http.ResponseWriter, r *http.Request) {
	if h.Chains == nil {
		writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestError("chain runtime is not configured"), http.StatusServiceUnavailable)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	unsessionedOnly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("unsessioned_only")), "true")
	chains, err := h.Chains.ListChains(r.Context(), pr, sessionID, unsessionedOnly)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, &types.ChainList{Items: chains})
}

func (h ChainsHandler) runChainBlocking(w http.ResponseWriter, r *http.Request, chainID string) {
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required").WithChain(chainID))
		return
	}
	payload, err := h.readRunPayload(w, r)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	if err := h.validateRunStartAuthorized(r.Context(), pr, chainID, payload); err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	env := chainEnv(h, pr, r.Header, types.AttachmentModeStatefulHTTP, "http", false, nil)
	ctx := withHandlerTimeout(r.Context(), h.Config.HandlerTimeout)
	run, result, err := h.Chains.RunBlocking(ctx, chainID, env, *payload, idempotencyKey)
	if err != nil {
		h.writeChainError(w, requestID(r), err)
		return
	}
	writeJSON(w, &types.ChainRunResultEnvelope{Run: run, Result: result})
}

func (h ChainsHandler) runChainStream(w http.ResponseWriter, r *http.Request, chainID string) {
	reqID := requestID(r)
	if h.Lifecycle != nil && h.Lifecycle.IsDraining() {
		writeCoreErrorJSON(w, reqID, &core.Error{Type: core.ErrOverloaded, Message: "gateway is draining", Code: "draining", RequestID: reqID}, 529)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required").WithChain(chainID))
		return
	}
	payload, err := h.readRunPayload(w, r)
	if err != nil {
		h.writeChainError(w, reqID, err)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	if err := h.validateRunStartAuthorized(r.Context(), pr, chainID, payload); err != nil {
		h.writeChainError(w, reqID, err)
		return
	}
	p := principal.Resolve(r, h.Config)
	if h.Limiter != nil && h.Config.LimitMaxConcurrentStreams > 0 {
		dec := h.Limiter.AcquireStream(p.Key, time.Now())
		if !dec.Allowed {
			if dec.RetryAfter > 0 {
				w.Header().Set("Retry-After", "1")
			}
			writeCoreErrorJSON(w, reqID, core.NewRateLimitError("too many concurrent streams", dec.RetryAfter), http.StatusTooManyRequests)
			return
		}
		if dec.Permit != nil {
			defer dec.Permit.Release()
		}
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	sw, err := sse.New(w)
	if err != nil {
		h.writeChainError(w, reqID, err)
		return
	}
	ctx := r.Context()
	if h.Config.SSEMaxStreamDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Config.SSEMaxStreamDuration)
		defer cancel()
	}
	if h.Config.SSEPingInterval > 0 {
		ticker := time.NewTicker(h.Config.SSEPingInterval)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = sw.Send("ping", types.RunPingEvent{Type: "ping"})
				}
			}
		}()
	}
	env := chainEnv(h, pr, r.Header, types.AttachmentModeStatefulSSE, "sse", false, func(event types.ChainServerEvent) error {
		return sw.Send(event.ChainServerEventType(), event)
	})
	run, result, err := h.Chains.RunBlocking(ctx, chainID, env, *payload, idempotencyKey)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		_ = sw.Send("chain.error", chainErrorEvent(chainID, runID(run), err))
		return
	}
	_ = run
	_ = result
}

func (h ChainsHandler) readRunPayload(w http.ResponseWriter, r *http.Request) (*types.RunStartPayload, error) {
	body, ok := readBoundedBody(w, r, h.Config.MaxBodyBytes)
	if !ok {
		return nil, core.NewInvalidRequestError("failed to read request body")
	}
	return types.UnmarshalChainRunPayloadStrict(body)
}

func (h ChainsHandler) validateRunStartAuthorized(ctx context.Context, pr chainrt.Principal, chainID string, payload *types.RunStartPayload) error {
	if payload == nil {
		return core.NewInvalidRequestError("payload must not be nil")
	}
	record, err := h.Chains.GetChainAuthorized(ctx, pr, chainID)
	if err != nil {
		return err
	}
	contextResp, err := h.Chains.GetChainContextAuthorized(ctx, pr, chainID)
	if err != nil {
		return err
	}
	effective := mergeChainDefaults(record.Defaults, overrideDefaults(payload.Overrides))
	history := append(cloneMessages(contextResp.Messages), cloneMessages(payload.Input)...)
	return h.validateChainMessageRequest(effective, history)
}

func (h ChainsHandler) validateChainMessageRequest(defaults types.ChainDefaults, history []types.Message) error {
	if strings.TrimSpace(defaults.Model) == "" {
		return nil
	}
	req := messageRequestForValidation(defaults, history)
	if err := limits.ValidateMessageRequest(req, h.Config); err != nil {
		return err
	}
	if len(h.Config.ModelAllowlist) > 0 {
		if _, ok := h.Config.ModelAllowlist[req.Model]; !ok {
			return &core.Error{Type: core.ErrPermission, Message: "model is not allowlisted", Param: "model"}
		}
	}
	providerName, modelName, err := core.ParseModelString(req.Model)
	if err != nil {
		return err
	}
	if providerName == "gemini" || providerName == "gemini-oauth" {
		return &core.Error{Type: core.ErrInvalidRequest, Message: "provider has been removed; use gem-vert/<model> or gem-dev/<model>", Param: "model"}
	}
	if compatIssues := compat.ValidateMessageRequest(req, providerName, providerName+"/"+modelName); len(compatIssues) > 0 {
		return &core.Error{Type: core.ErrInvalidRequest, Message: "request is incompatible with the selected provider/model", CompatIssues: compatIssues}
	}
	return nil
}

func (h ChainsHandler) writeChainError(w http.ResponseWriter, reqID string, err error) {
	if err == nil {
		return
	}
	if canonical, ok := err.(*types.CanonicalError); ok {
		writeCanonicalErrorJSON(w, canonical)
		return
	}
	if errors.Is(err, chainrt.ErrNotFound) {
		writeCoreErrorJSON(w, reqID, &core.Error{Type: core.ErrNotFound, Message: "resource not found", RequestID: reqID}, http.StatusNotFound)
		return
	}
	if h.Logger != nil {
		h.Logger.Error("chain handler error", "request_id", reqID, "error", err)
	}
	coreErr, status := coreErrorFrom(err, reqID)
	writeCoreErrorJSON(w, reqID, coreErr, status)
}

type SessionsHandler struct {
	Config config.Config
	Chains *chainrt.Manager
}

func (h SessionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v1/sessions" {
		pr := chainPrincipalFromRequest(r, h.Config)
		sessions, err := h.Chains.ListSessions(r.Context(), pr)
		if err != nil {
			if canonical, ok := err.(*types.CanonicalError); ok {
				writeCanonicalErrorJSON(w, canonical)
				return
			}
			coreErr, status := coreErrorFrom(err, requestID(r))
			writeCoreErrorJSON(w, requestID(r), coreErr, status)
			return
		}
		writeJSON(w, &types.SessionList{Items: sessions})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	if path == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	parts := strings.Split(path, "/")
	sessionID := strings.TrimSpace(parts[0])
	if sessionID == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	if len(parts) == 1 {
		session, err := h.Chains.GetSessionAuthorized(r.Context(), pr, sessionID)
		if err != nil {
			if errors.Is(err, chainrt.ErrNotFound) {
				writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrNotFound, Message: "resource not found", RequestID: requestID(r)}, http.StatusNotFound)
				return
			}
			if canonical, ok := err.(*types.CanonicalError); ok {
				writeCanonicalErrorJSON(w, canonical)
				return
			}
			coreErr, status := coreErrorFrom(err, requestID(r))
			writeCoreErrorJSON(w, requestID(r), coreErr, status)
			return
		}
		writeJSON(w, session)
		return
	}
	if len(parts) == 2 && parts[1] == "chains" {
		chains, err := h.Chains.ListSessionChainsAuthorized(r.Context(), pr, sessionID)
		if err != nil {
			if errors.Is(err, chainrt.ErrNotFound) {
				writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrNotFound, Message: "resource not found", RequestID: requestID(r)}, http.StatusNotFound)
				return
			}
			if canonical, ok := err.(*types.CanonicalError); ok {
				writeCanonicalErrorJSON(w, canonical)
				return
			}
			coreErr, status := coreErrorFrom(err, requestID(r))
			writeCoreErrorJSON(w, requestID(r), coreErr, status)
			return
		}
		writeJSON(w, &types.SessionChainList{Items: chains})
		return
	}
	NotFoundHandler{}.ServeHTTP(w, r)
}

type ChainRunsReadHandler struct {
	Config config.Config
	Chains *chainrt.Manager
}

func (h ChainRunsReadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/runs/") {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")
	if path == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	if strings.HasSuffix(path, ":regenerate") {
		runID := strings.TrimSpace(strings.TrimSuffix(path, ":regenerate"))
		if runID == "" {
			NotFoundHandler{}.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
			return
		}
		h.regenerateRun(w, r, runID)
		return
	}
	if r.Method != http.MethodGet {
		writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrInvalidRequest, Message: "method not allowed", Code: "method_not_allowed", RequestID: requestID(r)}, http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(path, "/")
	runID := strings.TrimSpace(parts[0])
	if runID == "" {
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	var (
		value any
		err   error
	)
	switch {
	case len(parts) == 1:
		value, err = h.Chains.GetRunAuthorized(r.Context(), pr, runID)
	case len(parts) == 2 && parts[1] == "timeline":
		value, err = h.Chains.GetRunTimelineAuthorized(r.Context(), pr, runID)
	case len(parts) == 2 && parts[1] == "effective-request":
		value, err = h.Chains.GetEffectiveRequestAuthorized(r.Context(), pr, runID)
	default:
		NotFoundHandler{}.ServeHTTP(w, r)
		return
	}
	if err != nil {
		if errors.Is(err, chainrt.ErrNotFound) {
			writeCoreErrorJSON(w, requestID(r), &core.Error{Type: core.ErrNotFound, Message: "resource not found", RequestID: requestID(r)}, http.StatusNotFound)
			return
		}
		if canonical, ok := err.(*types.CanonicalError); ok {
			writeCanonicalErrorJSON(w, canonical)
			return
		}
		coreErr, status := coreErrorFrom(err, requestID(r))
		writeCoreErrorJSON(w, requestID(r), coreErr, status)
		return
	}
	writeJSON(w, value)
}

func (h ChainRunsReadHandler) regenerateRun(w http.ResponseWriter, r *http.Request, runID string) {
	if h.Chains == nil {
		writeCoreErrorJSON(w, requestID(r), core.NewInvalidRequestError("chain runtime is not configured"), http.StatusServiceUnavailable)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if idempotencyKey == "" {
		writeCanonicalErrorJSON(w, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "Idempotency-Key header is required").WithRun(runID))
		return
	}
	body, ok := readBoundedBody(w, r, 1<<20)
	if !ok {
		return
	}
	var payload types.RunRegenerateRequest
	if err := decodeStrictJSON(body, &payload); err != nil {
		if canonical, ok := err.(*types.CanonicalError); ok {
			writeCanonicalErrorJSON(w, canonical.WithRun(runID))
			return
		}
		coreErr, status := coreErrorFrom(err, requestID(r))
		writeCoreErrorJSON(w, requestID(r), coreErr, status)
		return
	}
	pr := chainPrincipalFromRequest(r, h.Config)
	preview, err := h.Chains.PreviewRegenerateRun(r.Context(), pr, runID, payload)
	if err != nil {
		if canonical, ok := err.(*types.CanonicalError); ok {
			writeCanonicalErrorJSON(w, canonical.WithRun(runID))
			return
		}
		coreErr, status := coreErrorFrom(err, requestID(r))
		writeCoreErrorJSON(w, requestID(r), coreErr, status)
		return
	}
	history := append(cloneMessages(preview.History), cloneMessages(preview.Input)...)
	chainsHandler := ChainsHandler{Config: h.Config}
	if err := chainsHandler.validateChainMessageRequest(preview.Defaults, history); err != nil {
		coreErr, status := coreErrorFrom(err, requestID(r))
		writeCoreErrorJSON(w, requestID(r), coreErr, status)
		return
	}
	resp, err := h.Chains.RegenerateRun(withHandlerTimeout(r.Context(), h.Config.HandlerTimeout), pr, runID, payload, idempotencyKey)
	if err != nil {
		if canonical, ok := err.(*types.CanonicalError); ok {
			writeCanonicalErrorJSON(w, canonical)
			return
		}
		coreErr, status := coreErrorFrom(err, requestID(r))
		writeCoreErrorJSON(w, requestID(r), coreErr, status)
		return
	}
	writeJSON(w, resp)
}

func requestID(r *http.Request) string {
	reqID, _ := mw.RequestIDFrom(r.Context())
	return reqID
}

func readBoundedBody(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reqID, _ := mw.RequestIDFrom(r.Context())
		writeCoreErrorJSON(w, reqID, core.NewInvalidRequestError("failed to read request body"), http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

func withHandlerTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	newCtx, _ := context.WithTimeout(ctx, timeout)
	return newCtx
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func decodeStrictJSON(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return core.NewInvalidRequestError("request body is invalid JSON")
	}
	if dec.More() {
		return core.NewInvalidRequestError("request body must contain a single JSON object")
	}
	return nil
}

func messageRequestForValidation(defaults types.ChainDefaults, messages []types.Message) *types.MessageRequest {
	return &types.MessageRequest{
		Model:         defaults.Model,
		Messages:      cloneMessages(messages),
		System:        defaults.System,
		MaxTokens:     defaults.MaxTokens,
		Tools:         cloneJSON(defaults.Tools),
		ToolChoice:    cloneJSON(defaults.ToolChoice),
		Temperature:   cloneJSON(defaults.Temperature),
		TopP:          cloneJSON(defaults.TopP),
		TopK:          cloneJSON(defaults.TopK),
		StopSequences: append([]string(nil), defaults.StopSequences...),
		STTModel:      defaults.STTModel,
		TTSModel:      defaults.TTSModel,
		OutputFormat:  cloneJSON(defaults.OutputFormat),
		Output:        cloneJSON(defaults.Output),
		Voice:         cloneJSON(defaults.Voice),
		Extensions:    cloneJSON(defaults.Extensions),
		Metadata:      cloneJSON(defaults.Metadata),
	}
}

func mergeChainDefaults(current, patch types.ChainDefaults) types.ChainDefaults {
	if strings.TrimSpace(patch.Model) != "" {
		current.Model = patch.Model
	}
	if patch.System != nil {
		current.System = patch.System
	}
	if patch.Tools != nil {
		current.Tools = cloneJSON(patch.Tools)
	}
	if patch.GatewayTools != nil {
		current.GatewayTools = append([]string(nil), patch.GatewayTools...)
	}
	if patch.GatewayToolConfig != nil {
		current.GatewayToolConfig = cloneJSON(patch.GatewayToolConfig)
	}
	if patch.ToolChoice != nil {
		current.ToolChoice = cloneJSON(patch.ToolChoice)
	}
	if patch.MaxTokens != 0 {
		current.MaxTokens = patch.MaxTokens
	}
	if patch.Temperature != nil {
		current.Temperature = cloneJSON(patch.Temperature)
	}
	if patch.TopP != nil {
		current.TopP = cloneJSON(patch.TopP)
	}
	if patch.TopK != nil {
		current.TopK = cloneJSON(patch.TopK)
	}
	if patch.StopSequences != nil {
		current.StopSequences = append([]string(nil), patch.StopSequences...)
	}
	if patch.STTModel != "" {
		current.STTModel = patch.STTModel
	}
	if patch.TTSModel != "" {
		current.TTSModel = patch.TTSModel
	}
	if patch.OutputFormat != nil {
		current.OutputFormat = cloneJSON(patch.OutputFormat)
	}
	if patch.Output != nil {
		current.Output = cloneJSON(patch.Output)
	}
	if patch.Voice != nil {
		current.Voice = cloneJSON(patch.Voice)
	}
	if patch.Extensions != nil {
		current.Extensions = cloneJSON(patch.Extensions)
	}
	if patch.Metadata != nil {
		current.Metadata = cloneJSON(patch.Metadata)
	}
	return current
}

func overrideDefaults(overrides *types.RunOverrides) types.ChainDefaults {
	if overrides == nil {
		return types.ChainDefaults{}
	}
	return cloneJSON(*overrides)
}

func cloneJSON[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		return value
	}
	return out
}

func cloneMessages(in []types.Message) []types.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.Message, len(in))
	copy(out, in)
	return out
}

func chainErrorEvent(chainID, runID string, err error) types.ChainErrorEvent {
	canonical := &types.CanonicalError{
		Code:    types.ErrorCodeProtocolUnknownFrame,
		Message: "request failed",
		ChainID: chainID,
		RunID:   runID,
	}
	if typed, ok := err.(*types.CanonicalError); ok && typed != nil {
		canonical = typed
		if canonical.ChainID == "" {
			canonical = canonical.WithChain(chainID)
		}
		if runID != "" && canonical.RunID == "" {
			canonical = canonical.WithRun(runID)
		}
	}
	return types.ChainErrorEvent{
		Type:           "chain.error",
		ChainVersion:   0,
		CanonicalError: *canonical,
	}
}

func runID(run *types.ChainRunRecord) string {
	if run == nil {
		return ""
	}
	return run.ID
}
