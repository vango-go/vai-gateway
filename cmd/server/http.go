package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v84"
	stripewebhook "github.com/stripe/stripe-go/v84/webhook"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
	workos "github.com/vango-go/vango-workos"
)

type chatStreamInput struct {
	ConversationID string   `json:"conversation_id"`
	Message        string   `json:"message"`
	Model          string   `json:"model"`
	KeySource      string   `json:"key_source"`
	AttachmentIDs  []string `json:"attachment_ids"`
	Regenerate     bool     `json:"regenerate"`
	EditMessageID  string   `json:"edit_message_id"`
}

type uploadIntentInput struct {
	ConversationID string `json:"conversation_id"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	Size           int64  `json:"size"`
}

type uploadClaimInput struct {
	ConversationID string `json:"conversation_id"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	Size           int64  `json:"size"`
	IntentToken    string `json:"intent_token"`
}

type messageRequestEnvelope struct {
	Model string `json:"model"`
}

type runRequestEnvelope struct {
	Request struct {
		Model string `json:"model"`
	} `json:"request"`
}

func (s *betaServer) registerMux(mux *http.ServeMux) {
	csrfMw := s.app.Server().CSRFMiddleware()

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.Handle("/v1/", s.rawGatewayProxy())

	if s.workos != nil {
		s.workos.RegisterAuthHandlers(mux, csrfMw)
	}

	mux.Handle("/webhooks/stripe", http.HandlerFunc(s.handleStripeWebhook))
	mux.Handle("/", s.app)
}

func (s *betaServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *betaServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, "select 1"); err != nil {
		http.Error(w, "database not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *betaServer) handleNewConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	conv, err := s.services.CreateConversation(r.Context(), actor, "", s.cfg.DefaultModel, services.KeySourcePlatformHosted)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/chat/"+conv.ID, http.StatusSeeOther)
}

func (s *betaServer) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	created, err := s.services.CreateAPIKey(r.Context(), actor, r.FormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><title>vai-lite gateway | API key created</title><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body class="beta-app"><main class="token-page"><h1>API key created</h1><p>Copy this key now. It will not be shown again.</p><pre class="token-block">%s</pre><a class="btn btn-primary" href="/settings/developers">Back to developers</a></main></body></html>`, created.Token)
}

func (s *betaServer) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := s.services.RevokeAPIKey(r.Context(), actor, r.FormValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings/developers", http.StatusSeeOther)
}

func (s *betaServer) handleStoreProviderSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.services.StoreProviderSecret(r.Context(), actor, r.FormValue("provider"), r.FormValue("secret_value")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func (s *betaServer) handleDeleteProviderSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := s.services.DeleteProviderSecret(r.Context(), actor, r.FormValue("provider")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func (s *betaServer) handleBillingTopup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	amountCents, err := parseBillingTopupAmount(r)
	if err != nil {
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}
	intent, err := s.services.CreateTopupCheckout(r.Context(), actor, amountCents)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, intent.CheckoutURL, http.StatusSeeOther)
}

func (s *betaServer) handleUploadIntent(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	var in uploadIntentInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	intent, err := s.services.CreateImageUploadIntent(r.Context(), actor, in.Filename, in.ContentType, in.Size)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, intent)
}

func (s *betaServer) handleUploadClaim(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	var in uploadClaimInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	attachment, err := s.services.ClaimImageAttachment(r.Context(), actor, in.ConversationID, "", in.Filename, in.ContentType, in.Size, in.IntentToken)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	attachmentURL := ""
	if s.services.BlobStore != nil {
		if signed, signErr := s.services.BlobStore.PresignGet(r.Context(), attachment.BlobRef.Key, 30*time.Minute); signErr == nil {
			attachmentURL = signed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          attachment.ID,
		"filename":    attachment.Filename,
		"contentType": attachment.ContentType,
		"sizeBytes":   attachment.SizeBytes,
		"url":         attachmentURL,
	})
}

func (s *betaServer) handleChatStream(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActorHTTP(w, r)
	if !ok {
		return
	}
	var in chatStreamInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		writeJSONError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}

	org, err := s.services.Org(r.Context(), actor.OrgID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	requestedMode := services.KeySource(strings.TrimSpace(in.KeySource))
	resolvedHeaders, keySource, err := s.services.ResolveExecutionHeaders(r.Context(), actor.OrgID, r.Header, requestedMode, services.AccessCredentialSessionAuth)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if keySource == services.KeySourcePlatformHosted {
		if err := s.services.ReservePlatformHostedUsage(r.Context(), actor.OrgID, firstNonEmpty(strings.TrimSpace(in.Model), org.DefaultModel)); err != nil {
			writeJSONError(w, http.StatusPaymentRequired, err.Error())
			return
		}
	}

	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = org.DefaultModel
	}
	if err := s.services.UpdateConversationSettings(r.Context(), actor, in.ConversationID, model, keySource); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if in.EditMessageID != "" {
		if err := s.services.ReviseUserMessage(r.Context(), actor, in.ConversationID, in.EditMessageID, in.Message); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if in.Regenerate {
		detail, err := s.services.Conversation(r.Context(), actor.OrgID, in.ConversationID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		lastUserID := lastUserMessageID(detail.Messages)
		if lastUserID == "" {
			writeJSONError(w, http.StatusBadRequest, "conversation has no user message to regenerate from")
			return
		}
		if err := s.services.TruncateConversationAfter(r.Context(), actor, in.ConversationID, lastUserID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if _, err := s.services.AddUserMessage(r.Context(), actor, in.ConversationID, in.Message, keySource, in.AttachmentIDs); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	detail, err := s.services.Conversation(r.Context(), actor.OrgID, in.ConversationID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runReq, err := s.buildConversationRunRequest(r.Context(), detail, resolvedHeaders)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runReq.Request.Model = model

	body, err := json.Marshal(runReq)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/runs:stream", bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header = cloneForForward(resolvedHeaders)
	req.Header.Set("Content-Type", "application/json")

	writer := newCaptureWriter(w)
	s.gateway.ServeHTTP(writer, req)
	if writer.StatusCode() >= http.StatusBadRequest {
		if gatewayErr := extractGatewayErrorMessage(writer.Body()); gatewayErr != "" {
			s.logger.Warn("chat stream gateway rejected request", "status", writer.StatusCode(), "error", gatewayErr)
		}
		return
	}
	result, parseErr := extractRunResult(writer.Body())
	if parseErr != nil {
		s.logger.Error("stream parse failed", "error", parseErr)
		return
	}
	if result == nil || result.Response == nil {
		return
	}

	if _, err := s.services.AddAssistantMessage(r.Context(), actor, in.ConversationID, result.Response.TextContent(), keySource, result.Usage, result.Steps); err != nil {
		s.logger.Error("persist assistant message failed", "error", err)
	}
	if err := s.services.RecordUsage(r.Context(), actor.OrgID, in.ConversationID, "", "chat_stream", model, keySource, services.AccessCredentialSessionAuth, result.Usage, map[string]any{
		"via": "chat_island",
	}); err != nil {
		s.logger.Error("record usage failed", "error", err)
	}
}

func (s *betaServer) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	if secret == "" {
		s.logger.Warn("stripe webhook secret missing")
		http.Error(w, "stripe webhook secret not configured", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.logger.Warn("stripe webhook body read failed", "error", err)
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	event, err := stripewebhook.ConstructEventWithOptions(
		body,
		r.Header.Get("Stripe-Signature"),
		secret,
		stripewebhook.ConstructEventOptions{
			IgnoreAPIVersionMismatch: true,
		},
	)
	if err != nil {
		s.logger.Warn("stripe webhook signature verification failed",
			"error", err,
			"secret_suffix", secretSuffix(secret),
			"secret_len", len(secret),
			"signature_present", r.Header.Get("Stripe-Signature") != "",
			"body_len", len(body),
		)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	if event.Type != "checkout.session.completed" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	s.logger.Info("stripe checkout completed received",
		"event_id", event.ID,
		"session_id", session.ID,
		"metadata_topup_id", session.Metadata["topup_id"],
		"metadata_org_id", session.Metadata["org_id"],
		"amount_total", session.AmountTotal,
	)
	amountCents := int64(0)
	if session.AmountTotal > 0 {
		amountCents = session.AmountTotal
	}
	if err := s.services.ApplyStripeCheckoutCompleted(
		r.Context(),
		event.ID,
		session.ID,
		session.Metadata["topup_id"],
		session.Metadata["org_id"],
		amountCents,
	); err != nil {
		s.logger.Error("stripe fulfillment failed", "error", err)
		http.Error(w, "fulfillment failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *betaServer) rawGatewayProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := authTokenFromRequest(r)
		if token == "" {
			writeGatewayError(w, http.StatusUnauthorized, "missing gateway API key")
			return
		}
		validated, err := s.services.ValidateGatewayAPIKey(r.Context(), token)
		if err != nil {
			writeGatewayError(w, http.StatusUnauthorized, "invalid gateway API key")
			return
		}

		var body []byte
		singleRequestLogged := isSingleRequestLoggedEndpoint(r.URL.Path)
		managedRunObserved := isManagedRunObservedEndpoint(r.URL.Path)
		observedRequest := singleRequestLogged || managedRunObserved
		if r.Method == http.MethodPost && observedRequest {
			body, err = io.ReadAll(r.Body)
			if err != nil {
				writeGatewayError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		if r.URL.Path == "/v1/models" {
			req := r.Clone(r.Context())
			req.Header = cloneForForward(r.Header)
			s.gateway.ServeHTTP(w, req)
			return
		}

		var observed *observedGatewayRequest
		var prepared *services.PreparedGatewayObservation
		var requestID string
		var startedAt time.Time
		keySource := inferredGatewayKeySource(r.Header)
		if observedRequest {
			startedAt = time.Now().UTC()
			requestID = ensureGatewayRequestID(r.Header)
			observed, err = parseObservedGatewayRequest(r, body, requestID)
			if err != nil {
				s.logger.Warn("gateway observability parse failed", "request_id", requestID, "path", r.URL.Path, "error", err)
				observed = fallbackObservedGatewayRequest(r, body, requestID)
			}
			if observed == nil {
				observed = fallbackObservedGatewayRequest(r, body, requestID)
			}
			if observed == nil {
				writeGatewayError(w, http.StatusInternalServerError, "failed to initialize gateway observability")
				return
			}
			prepared, err = s.services.PrepareGatewayObservation(r.Context(), services.GatewayObservationPrepareInput{
				RequestID:               observed.RequestID,
				OrgID:                   validated.Organization.ID,
				GatewayAPIKeyID:         validated.APIKeyID,
				SessionID:               observed.SessionID,
				EndpointFamily:          observed.EndpointFamily,
				InputContextFingerprint: observed.InputContextFingerprint,
			})
			if err != nil {
				s.logger.Error("prepare gateway observation failed", "request_id", observed.RequestID, "error", err)
				writeGatewayError(w, http.StatusInternalServerError, "failed to prepare observability")
				return
			}
			setGatewayObservationHeaders(w.Header(), prepared)
		}

		resolvedHeaders, keySource, err := s.services.ResolveExecutionHeaders(r.Context(), validated.Organization.ID, r.Header, "", services.AccessCredentialGatewayAPIKey)
		if err != nil {
			writeGatewayError(w, http.StatusBadRequest, err.Error())
			s.recordObservedGatewayOutcome(r.Context(), validated, prepared, observed, &observedGatewayCompletion{
				StatusCode:      http.StatusBadRequest,
				ResponseBody:    mustJSONText(map[string]any{"error": map[string]any{"message": strings.TrimSpace(err.Error())}}),
				ResponseSummary: mustJSONText(map[string]any{"status_code": http.StatusBadRequest, "error": strings.TrimSpace(err.Error())}),
				ErrorSummary:    strings.TrimSpace(err.Error()),
				ErrorJSON:       mustJSONText(map[string]any{"message": strings.TrimSpace(err.Error())}),
			}, keySource, startedAt, singleRequestLogged, managedRunObserved)
			return
		}
		if keySource == services.KeySourcePlatformHosted && observedRequest {
			model := extractGatewayModel(r.URL.Path, body)
			if err := s.services.ReservePlatformHostedUsage(r.Context(), validated.Organization.ID, model); err != nil {
				writeGatewayError(w, http.StatusPaymentRequired, err.Error())
				s.recordObservedGatewayOutcome(r.Context(), validated, prepared, observed, &observedGatewayCompletion{
					StatusCode:      http.StatusPaymentRequired,
					ResponseBody:    mustJSONText(map[string]any{"error": map[string]any{"message": strings.TrimSpace(err.Error())}}),
					ResponseSummary: mustJSONText(map[string]any{"status_code": http.StatusPaymentRequired, "error": strings.TrimSpace(err.Error())}),
					ErrorSummary:    strings.TrimSpace(err.Error()),
					ErrorJSON:       mustJSONText(map[string]any{"message": strings.TrimSpace(err.Error())}),
				}, keySource, startedAt, singleRequestLogged, managedRunObserved)
				return
			}
		}

		req := r.Clone(r.Context())
		req.Header = cloneForForward(resolvedHeaders)
		if observedRequest {
			req.Header.Set(headerRequestID, requestID)
			if prepared != nil && strings.TrimSpace(prepared.SessionID) != "" {
				req.Header.Set(headerSessionID, prepared.SessionID)
			}
		}
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}

		if !observedRequest {
			s.gateway.ServeHTTP(w, req)
			return
		}

		writer := newCaptureWriter(w)
		setGatewayObservationHeaders(writer.Header(), prepared)
		s.gateway.ServeHTTP(writer, req)

		completion, completionErr := completeObservedGatewayRequest(observed, writer.StatusCode(), writer.Body())
		if completionErr != nil {
			s.logger.Error("complete gateway observation failed", "request_id", requestID, "error", completionErr)
			completion = &observedGatewayCompletion{
				StatusCode:      writer.StatusCode(),
				ResponseBody:    strings.TrimSpace(string(writer.Body())),
				ResponseSummary: mustJSONText(map[string]any{"status_code": writer.StatusCode()}),
				ErrorSummary:    "failed to materialize gateway response",
				ErrorJSON:       mustJSONText(map[string]any{"message": completionErr.Error()}),
			}
		}
		s.recordObservedGatewayOutcome(r.Context(), validated, prepared, observed, completion, keySource, startedAt, singleRequestLogged, managedRunObserved)
	})
}

func isSingleRequestLoggedEndpoint(path string) bool {
	switch path {
	case "/v1/messages":
		return true
	default:
		return false
	}
}

func isManagedRunObservedEndpoint(path string) bool {
	switch path {
	case "/v1/runs", "/v1/runs:stream":
		return true
	default:
		return false
	}
}

func inferredGatewayKeySource(headers http.Header) services.KeySource {
	if gatewayRequestHasProviderKeyHeader(headers) {
		return services.KeySourceCustomerBYOKExternal
	}
	return services.KeySourcePlatformHosted
}

func gatewayRequestHasProviderKeyHeader(headers http.Header) bool {
	for key, values := range headers {
		if !strings.HasPrefix(http.CanonicalHeaderKey(key), "X-Provider-Key-") {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func setGatewayObservationHeaders(headers http.Header, prepared *services.PreparedGatewayObservation) {
	if headers == nil || prepared == nil {
		return
	}
	headers.Set(headerRequestID, prepared.RequestID)
	if strings.TrimSpace(prepared.SessionID) != "" {
		headers.Set(headerSessionID, prepared.SessionID)
	}
	headers.Set(headerChainID, prepared.ChainID)
	if strings.TrimSpace(prepared.ParentRequestID) != "" {
		headers.Set(headerParentRequestID, prepared.ParentRequestID)
	}
}

func (s *betaServer) recordObservedGatewayOutcome(
	ctx context.Context,
	validated *services.ValidatedGatewayAPIKey,
	prepared *services.PreparedGatewayObservation,
	observed *observedGatewayRequest,
	completion *observedGatewayCompletion,
	keySource services.KeySource,
	startedAt time.Time,
	singleRequestLogged bool,
	managedRunObserved bool,
) {
	if singleRequestLogged {
		s.recordGatewayObservation(ctx, validated, prepared, observed, completion, keySource, startedAt)
		return
	}
	if managedRunObserved {
		s.recordManagedGatewayRunObservation(ctx, validated, prepared, observed, completion, keySource, startedAt)
	}
}

func (s *betaServer) recordGatewayObservation(
	ctx context.Context,
	validated *services.ValidatedGatewayAPIKey,
	prepared *services.PreparedGatewayObservation,
	observed *observedGatewayRequest,
	completion *observedGatewayCompletion,
	keySource services.KeySource,
	startedAt time.Time,
) {
	if validated == nil || prepared == nil || observed == nil || completion == nil {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	completedAt := time.Now().UTC()
	if completion.DurationMS == 0 {
		completion.DurationMS = completedAt.Sub(startedAt).Milliseconds()
	}
	if err := s.services.RecordGatewayObservation(ctx, services.GatewayObservationRecordInput{
		RequestID:                prepared.RequestID,
		OrgID:                    validated.Organization.ID,
		GatewayAPIKeyID:          validated.APIKeyID,
		SessionID:                prepared.SessionID,
		ChainID:                  prepared.ChainID,
		ParentRequestID:          prepared.ParentRequestID,
		EndpointKind:             observed.EndpointKind,
		EndpointFamily:           observed.EndpointFamily,
		Method:                   observed.Method,
		Path:                     observed.Path,
		Provider:                 observed.Provider,
		Model:                    observed.Model,
		KeySource:                keySource,
		AccessCredential:         services.AccessCredentialGatewayAPIKey,
		StatusCode:               completion.StatusCode,
		DurationMS:               completion.DurationMS,
		InputContextFingerprint:  observed.InputContextFingerprint,
		OutputContextFingerprint: completion.OutputContextFingerprint,
		RequestSummary:           observed.RequestSummary,
		ResponseSummary:          completion.ResponseSummary,
		RequestBody:              observed.RequestBody,
		ResponseBody:             completion.ResponseBody,
		ErrorSummary:             completion.ErrorSummary,
		ErrorJSON:                completion.ErrorJSON,
		StartedAt:                startedAt,
		CompletedAt:              completedAt,
		RunTrace:                 completion.RunTrace,
	}); err != nil {
		s.logger.Error("record gateway observation failed", "request_id", prepared.RequestID, "error", err)
		return
	}
	s.recordObservedGatewayUsage(ctx, validated.Organization.ID, observed, completion, keySource)
}

func (s *betaServer) recordManagedGatewayRunObservation(
	ctx context.Context,
	validated *services.ValidatedGatewayAPIKey,
	prepared *services.PreparedGatewayObservation,
	observed *observedGatewayRequest,
	completion *observedGatewayCompletion,
	keySource services.KeySource,
	startedAt time.Time,
) {
	if validated == nil || prepared == nil || observed == nil || completion == nil {
		return
	}
	completedAt := time.Now().UTC()
	if completion.DurationMS == 0 {
		completion.DurationMS = completedAt.Sub(startedAt).Milliseconds()
	}
	if err := s.services.ProjectGatewayRunObservation(ctx, services.GatewayManagedRunProjectionInput{
		RequestID:                prepared.RequestID,
		OrgID:                    validated.Organization.ID,
		ChainID:                  prepared.ChainID,
		ExternalSessionID:        prepared.SessionID,
		ParentRequestID:          prepared.ParentRequestID,
		GatewayAPIKeyID:          validated.APIKeyID,
		GatewayAPIKeyName:        validated.APIKeyName,
		GatewayAPIKeyPrefix:      validated.TokenPrefix,
		EndpointKind:             observed.EndpointKind,
		EndpointFamily:           observed.EndpointFamily,
		Provider:                 observed.Provider,
		Model:                    observed.Model,
		KeySource:                keySource,
		AccessCredential:         services.AccessCredentialGatewayAPIKey,
		System:                   observed.System,
		Messages:                 observed.Messages,
		RunConfig:                observed.RunConfig,
		RunResult:                completion.RunResult,
		ErrorSummary:             completion.ErrorSummary,
		InputContextFingerprint:  observed.InputContextFingerprint,
		OutputContextFingerprint: completion.OutputContextFingerprint,
		StartedAt:                startedAt,
		CompletedAt:              completedAt,
	}); err != nil {
		s.logger.Error("project managed gateway run failed", "request_id", prepared.RequestID, "error", err)
		return
	}
	s.recordObservedGatewayUsage(ctx, validated.Organization.ID, observed, completion, keySource)
}

func (s *betaServer) recordObservedGatewayUsage(ctx context.Context, orgID string, observed *observedGatewayRequest, completion *observedGatewayCompletion, keySource services.KeySource) {
	if observed == nil || completion == nil || completion.StatusCode >= http.StatusBadRequest || completion.Usage == nil {
		return
	}
	var meta map[string]any
	switch observed.EndpointFamily {
	case endpointKindMessages:
		var req types.MessageRequest
		if err := json.Unmarshal([]byte(observed.RequestBody), &req); err == nil {
			meta = pricingMetadataForGatewayRequest(&req)
		}
	case endpointKindRuns:
		var req types.RunRequest
		if err := json.Unmarshal([]byte(observed.RequestBody), &req); err == nil {
			meta = pricingMetadataForGatewayRequest(&req.Request)
		}
	}
	if meta == nil {
		meta = map[string]any{"via": "raw_gateway"}
	}
	if err := s.services.RecordUsage(ctx, orgID, "", "", observed.RequestKind, observed.Model, keySource, services.AccessCredentialGatewayAPIKey, *completion.Usage, meta); err != nil {
		s.logger.Error("record raw gateway usage failed", "request_id", observed.RequestID, "error", err)
	}
}

func extractGatewayModel(path string, body []byte) string {
	switch path {
	case "/v1/messages":
		var req messageRequestEnvelope
		_ = json.Unmarshal(body, &req)
		return strings.TrimSpace(req.Model)
	case "/v1/runs", "/v1/runs:stream":
		var req runRequestEnvelope
		_ = json.Unmarshal(body, &req)
		return strings.TrimSpace(req.Request.Model)
	default:
		return ""
	}
}

func pricingMetadataForGatewayRequest(req *types.MessageRequest) map[string]any {
	meta := map[string]any{
		"via": "raw_gateway",
	}
	if req == nil {
		return meta
	}
	if requestHasAudioInput(req) {
		meta["input_modality"] = string(services.PricingInputModalityAudio)
	}
	if requestRequestsImageOutput(req) {
		meta["output_modality"] = string(services.PricingOutputModalityImage)
	}
	return meta
}

func requestHasAudioInput(req *types.MessageRequest) bool {
	if req == nil {
		return false
	}
	if types.RequestHasAudioSTT(req) {
		return true
	}
	switch system := req.System.(type) {
	case []types.ContentBlock:
		if contentBlocksHaveAudioInput(system) {
			return true
		}
	}
	for _, msg := range req.Messages {
		if contentBlocksHaveAudioInput(msg.ContentBlocks()) {
			return true
		}
	}
	return false
}

func contentBlocksHaveAudioInput(blocks []types.ContentBlock) bool {
	for _, block := range blocks {
		switch b := block.(type) {
		case types.AudioBlock, *types.AudioBlock, types.AudioSTTBlock, *types.AudioSTTBlock:
			return true
		case types.ToolResultBlock:
			if contentBlocksHaveAudioInput(b.Content) {
				return true
			}
		case *types.ToolResultBlock:
			if b != nil && contentBlocksHaveAudioInput(b.Content) {
				return true
			}
		}
	}
	return false
}

func requestRequestsImageOutput(req *types.MessageRequest) bool {
	if req == nil {
		return false
	}
	if req.Output != nil {
		for _, modality := range req.Output.Modalities {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				return true
			}
		}
	}
	model := strings.ToLower(strings.TrimSpace(req.Model))
	return strings.Contains(model, "-image") || strings.Contains(model, "image-preview")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (s *betaServer) requireActorHTTP(w http.ResponseWriter, r *http.Request) (services.UserIdentity, bool) {
	identity, ok := workos.IdentityFromContext(r.Context())
	if !ok {
		target := "/"
		if r.URL != nil {
			target = r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
		}
		if s.workos != nil {
			http.Redirect(w, r, "/auth/signin?return_to="+url.QueryEscape(target), http.StatusSeeOther)
			return services.UserIdentity{}, false
		}
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return services.UserIdentity{}, false
	}
	actor, err := s.services.EnsureIdentity(r.Context(), identity)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return services.UserIdentity{}, false
	}
	return actor, true
}

func (s *betaServer) buildConversationRunRequest(ctx context.Context, detail *services.ConversationDetail, resolvedHeaders http.Header) (*types.RunRequest, error) {
	messages := make([]types.Message, 0, len(detail.Messages))
	for _, msg := range detail.Messages {
		blocks := make([]types.ContentBlock, 0, len(msg.Attachments)+1)
		if strings.TrimSpace(msg.BodyText) != "" {
			blocks = append(blocks, types.TextBlock{Type: "text", Text: msg.BodyText})
		}
		for _, att := range msg.Attachments {
			signedURL := ""
			if s.services.BlobStore != nil {
				url, err := s.services.BlobStore.PresignGet(ctx, att.BlobRef.Key, 30*time.Minute)
				if err != nil {
					return nil, err
				}
				signedURL = url
			}
			if signedURL != "" {
				blocks = append(blocks, types.ImageBlock{
					Type: "image",
					Source: types.ImageSource{
						Type: "url",
						URL:  signedURL,
					},
				})
			}
		}

		switch {
		case len(blocks) == 0:
			messages = append(messages, types.Message{Role: msg.Role, Content: msg.BodyText})
		case len(blocks) == 1 && len(msg.Attachments) == 0:
			if text, ok := blocks[0].(types.TextBlock); ok {
				messages = append(messages, types.Message{Role: msg.Role, Content: text.Text})
				continue
			}
			messages = append(messages, types.Message{Role: msg.Role, Content: blocks})
		default:
			messages = append(messages, types.Message{Role: msg.Role, Content: blocks})
		}
	}

	serverToolsEnabled, serverToolConfig := conversationServerTools(resolvedHeaders)

	return &types.RunRequest{
		Request: types.MessageRequest{
			Model:     detail.Conversation.Model,
			Messages:  messages,
			MaxTokens: 8192,
		},
		Run: types.RunConfig{
			MaxTurns:      8,
			MaxToolCalls:  8,
			TimeoutMS:     5 * 60 * 1000,
			ParallelTools: true,
			ToolTimeoutMS: 30 * 1000,
		},
		ServerTools:      serverToolsEnabled,
		ServerToolConfig: serverToolConfig,
	}, nil
}

func conversationServerTools(headers http.Header) ([]string, map[string]any) {
	if headers == nil {
		return nil, nil
	}

	tools := make([]string, 0, 2)
	config := make(map[string]any, 2)

	if provider := preferredWebSearchProvider(headers); provider != "" {
		tools = append(tools, servertools.ToolWebSearch)
		config[servertools.ToolWebSearch] = map[string]any{
			"provider": provider,
		}
	}
	if provider := preferredWebFetchProvider(headers); provider != "" {
		tools = append(tools, servertools.ToolWebFetch)
		config[servertools.ToolWebFetch] = map[string]any{
			"provider": provider,
			"format":   "markdown",
		}
	}

	if len(tools) == 0 {
		return nil, nil
	}
	return tools, config
}

func preferredWebSearchProvider(headers http.Header) string {
	switch {
	case strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyTavily)) != "":
		return servertools.ProviderTavily
	case strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyExa)) != "":
		return servertools.ProviderExa
	default:
		return ""
	}
}

func preferredWebFetchProvider(headers http.Header) string {
	switch {
	case strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyFirecrawl)) != "":
		return servertools.ProviderFirecrawl
	case strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyTavily)) != "":
		return servertools.ProviderTavily
	default:
		return ""
	}
}

type captureWriter struct {
	w      http.ResponseWriter
	status int
	body   bytes.Buffer
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	return &captureWriter{w: w, status: http.StatusOK}
}

func (c *captureWriter) Header() http.Header {
	return c.w.Header()
}

func (c *captureWriter) WriteHeader(status int) {
	c.status = status
	c.w.WriteHeader(status)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	_, _ = c.body.Write(p)
	n, err := c.w.Write(p)
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (c *captureWriter) Flush() {
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (c *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := c.w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacker unavailable")
	}
	return h.Hijack()
}

func (c *captureWriter) Body() []byte {
	return c.body.Bytes()
}

func (c *captureWriter) StatusCode() int {
	return c.status
}

type sseEvent struct {
	Event string
	Data  string
}

type gatewayErrorEnvelope struct {
	Error any `json:"error"`
}

func extractRunResult(raw []byte) (*types.RunResult, error) {
	events := parseSSE(raw)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event != "run_complete" {
			continue
		}
		ev, err := types.UnmarshalRunStreamEvent([]byte(events[i].Data))
		if err != nil {
			return nil, err
		}
		complete, ok := ev.(types.RunCompleteEvent)
		if ok {
			return complete.Result, nil
		}
	}
	return nil, nil
}

func parseSSE(raw []byte) []sseEvent {
	chunks := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n")
	events := make([]sseEvent, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		var eventName string
		var dataLines []string
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if eventName != "" {
			events = append(events, sseEvent{Event: eventName, Data: strings.Join(dataLines, "\n")})
		}
	}
	return events
}

func extractGatewayErrorMessage(raw []byte) string {
	var env gatewayErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	switch value := env.Error.(type) {
	case map[string]any:
		if message, _ := value["message"].(string); strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	case string:
		return strings.TrimSpace(value)
	}
	return ""
}

func writeGatewayError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": strings.TrimSpace(message),
		},
	})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func cloneForForward(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for key, vals := range in {
		out[key] = append([]string(nil), vals...)
	}
	out.Del("X-VAI-Internal-Org-ID")
	out.Del("X-VAI-Internal-Principal-ID")
	out.Del("X-VAI-Internal-Principal-Type")
	return out
}

func authTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("X-API-Key")); token != "" {
		return token
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func parseInt64Value(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty value")
	}
	return strconv.ParseInt(raw, 10, 64)
}

func parseBillingTopupAmount(r *http.Request) (int64, error) {
	if raw := strings.TrimSpace(r.FormValue("amount_usd")); raw != "" {
		return parseUSDCents(raw)
	}
	return parseInt64Value(r.FormValue("amount_cents"))
}

func parseUSDCents(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "$")
	raw = strings.ReplaceAll(raw, ",", "")
	if raw == "" {
		return 0, errors.New("empty value")
	}
	if strings.HasPrefix(raw, "+") {
		raw = strings.TrimPrefix(raw, "+")
	}
	if strings.HasPrefix(raw, "-") {
		return 0, errors.New("negative value")
	}

	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return 0, errors.New("invalid money value")
	}

	dollarsPart := parts[0]
	if dollarsPart == "" {
		dollarsPart = "0"
	}
	for _, ch := range dollarsPart {
		if ch < '0' || ch > '9' {
			return 0, errors.New("invalid dollars")
		}
	}

	centsPart := "00"
	if len(parts) == 2 {
		switch len(parts[1]) {
		case 0:
			centsPart = "00"
		case 1:
			centsPart = parts[1] + "0"
		case 2:
			centsPart = parts[1]
		default:
			return 0, errors.New("too many decimal places")
		}
		for _, ch := range centsPart {
			if ch < '0' || ch > '9' {
				return 0, errors.New("invalid cents")
			}
		}
	}

	dollars, err := strconv.ParseInt(dollarsPart, 10, 64)
	if err != nil {
		return 0, err
	}
	cents, err := strconv.ParseInt(centsPart, 10, 64)
	if err != nil {
		return 0, err
	}
	return dollars*100 + cents, nil
}

func lastUserMessageID(messages []services.ConversationMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].ID
		}
	}
	return ""
}

func secretSuffix(secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) <= 6 {
		return secret
	}
	return secret[len(secret)-6:]
}
