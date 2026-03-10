package vai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type Transport string

const (
	chainSDKIdempotencyHeader = "Idempotency-Key"
	chainSDKWSSubprotocol     = "vai.chain.v1"
)

const (
	TransportAuto      Transport = "auto"
	TransportWebSocket Transport = "websocket"
	TransportSSE       Transport = "sse"
	TransportHTTP      Transport = "http"
)

type ChainRequest struct {
	ExternalSessionID string
	Model             string
	System            any
	Messages          []Message
	Tools             []Tool
	GatewayTools      []string
	GatewayToolConfig map[string]any
	ToolChoice        *ToolChoice
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	TopK              *int
	StopSequences     []string
	STTModel          string
	TTSModel          string
	OutputFormat      *OutputFormat
	Output            *OutputConfig
	Voice             *VoiceConfig
	Extensions        map[string]any
	Metadata          map[string]any
	ProviderMetadata  map[string]any
	Transport         Transport
}

type ChainAttachRequest struct {
	ChainID            string
	ResumeToken        string
	AfterEventID       int64
	RequireExactReplay bool
	Takeover           bool
	Transport          Transport
}

type ChainUpdateRequest struct {
	Model             string
	System            any
	Tools             []Tool
	GatewayTools      []string
	GatewayToolConfig map[string]any
	ToolChoice        *ToolChoice
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	TopK              *int
	StopSequences     []string
	STTModel          string
	TTSModel          string
	OutputFormat      *OutputFormat
	Output            *OutputConfig
	Voice             *VoiceConfig
	Extensions        map[string]any
	Metadata          map[string]any
	ProviderMetadata  map[string]any
}

type ChainRunRequest struct {
	Input             []ContentBlock
	Model             string
	System            any
	Tools             []Tool
	GatewayTools      []string
	GatewayToolConfig map[string]any
	ToolChoice        *ToolChoice
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	TopK              *int
	StopSequences     []string
	STTModel          string
	TTSModel          string
	OutputFormat      *OutputFormat
	Output            *OutputConfig
	Voice             *VoiceConfig
	Extensions        map[string]any
	Metadata          map[string]any
	ProviderMetadata  map[string]any
}

type ChainStreamEvent any

type ChainsService struct {
	client *Client
}

type Chain struct {
	client    *Client
	transport Transport

	id                string
	sessionID         string
	externalSessionID string
	resumeToken       string
	defaults          types.ChainDefaults
	endpoint          string

	conn        *websocket.Conn
	writeMu     sync.Mutex
	reconnectMu sync.Mutex
	readerDone  chan struct{}

	errMu sync.RWMutex
	err   error

	mu           sync.Mutex
	lastEventID  int64
	current      *ChainStream
	updateWait   chan chainUpdateResult
	baseHandlers map[string]ToolHandler
	baseTimeout  time.Duration
	closed       bool

	closeOnce sync.Once
}

type chainUpdateResult struct {
	event *types.ChainUpdatedEvent
	err   error
}

type ChainStream struct {
	chain        *Chain
	runID        string
	events       chan ChainStreamEvent
	done         chan struct{}
	result       *types.RunResult
	err          error
	stopOnce     sync.Once
	closeOnce    sync.Once
	toolHandlers map[string]ToolHandler
	toolTimeout  time.Duration
	cancelMu     sync.Mutex
	cancel       func() error
	cancelQueued bool
}

func (s *ChainsService) Connect(ctx context.Context, req *ChainRequest, opts ...RunOption) (*Chain, error) {
	if s == nil || s.client == nil {
		return nil, core.NewInvalidRequestError("client is not initialized")
	}
	if !s.client.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}
	cfg := defaultRunConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	transport := resolveChainTransport(req.Transport)
	switch transport {
	case TransportWebSocket:
		return s.connectWebSocket(ctx, req, &cfg)
	case TransportHTTP, TransportSSE:
		return s.connectHTTP(ctx, req, &cfg, transport)
	default:
		return nil, core.NewInvalidRequestError("unsupported chain transport")
	}
}

func (s *ChainsService) Attach(ctx context.Context, req *ChainAttachRequest, opts ...RunOption) (*Chain, error) {
	if s == nil || s.client == nil {
		return nil, core.NewInvalidRequestError("client is not initialized")
	}
	if !s.client.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}
	cfg := defaultRunConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	transport := resolveChainTransport(req.Transport)
	switch transport {
	case TransportWebSocket:
		return s.attachWebSocket(ctx, req, &cfg)
	case TransportHTTP, TransportSSE:
		return &Chain{
			client:       s.client,
			transport:    transport,
			id:           strings.TrimSpace(req.ChainID),
			resumeToken:  strings.TrimSpace(req.ResumeToken),
			readerDone:   closedChan(),
			baseHandlers: cloneMap(cfg.toolHandlers),
			baseTimeout:  cfg.toolTimeout,
		}, nil
	default:
		return nil, core.NewInvalidRequestError("unsupported chain transport")
	}
}

func (s *ChainsService) Fork(ctx context.Context, chainID string, req *types.ChainForkRequest) (*types.ChainForkResponse, error) {
	if s == nil || s.client == nil {
		return nil, core.NewInvalidRequestError("client is not initialized")
	}
	if !s.client.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	chainID = strings.TrimSpace(chainID)
	if chainID == "" {
		return nil, core.NewInvalidRequestError("chain_id must not be empty")
	}
	if req == nil {
		req = &types.ChainForkRequest{}
	}
	headers := s.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("chain_fork"))
	resp, endpoint, err := s.client.postGatewayJSON(ctx, "/v1/chains/"+url.PathEscape(chainID)+":fork", req, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	var forked types.ChainForkResponse
	if err := json.NewDecoder(resp.Body).Decode(&forked); err != nil {
		return nil, core.NewAPIError("failed to decode chain fork response")
	}
	return &forked, nil
}

func (s *ChainsService) Regenerate(ctx context.Context, runID string, req *types.RunRegenerateRequest) (*types.ChainForkResponse, error) {
	if s == nil || s.client == nil {
		return nil, core.NewInvalidRequestError("client is not initialized")
	}
	if !s.client.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, core.NewInvalidRequestError("run_id must not be empty")
	}
	if req == nil {
		req = &types.RunRegenerateRequest{}
	}
	headers := s.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("run_regenerate"))
	resp, endpoint, err := s.client.postGatewayJSON(ctx, "/v1/runs/"+url.PathEscape(runID)+":regenerate", req, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	var forked types.ChainForkResponse
	if err := json.NewDecoder(resp.Body).Decode(&forked); err != nil {
		return nil, core.NewAPIError("failed to decode run regenerate response")
	}
	return &forked, nil
}

func (c *Chain) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

func (c *Chain) ResumeToken() string {
	if c == nil {
		return ""
	}
	return c.resumeToken
}

func (c *Chain) LastEventID() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID
}

func (c *Chain) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		if c.conn != nil {
			_ = c.sendWSFrame(types.ChainCloseFrame{Type: "chain.close", IdempotencyKey: newIdempotencyKey("chain_close")})
			c.writeMu.Lock()
			_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(5*time.Second))
			c.writeMu.Unlock()
			_ = c.conn.Close()
		}
	})
	if c.readerDone != nil {
		<-c.readerDone
	}
	return c.Err()
}

func (c *Chain) Err() error {
	if c == nil {
		return nil
	}
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.err
}

func (c *Chain) Update(ctx context.Context, req *ChainUpdateRequest) (*types.ChainUpdatedEvent, error) {
	if c == nil {
		return nil, core.NewInvalidRequestError("chain is not initialized")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}
	payload := types.ChainUpdatePayload{Defaults: chainDefaultsFromUpdate(req)}
	switch c.transport {
	case TransportWebSocket:
		waitCh := make(chan chainUpdateResult, 1)
		c.mu.Lock()
		if c.updateWait != nil {
			c.mu.Unlock()
			return nil, core.NewInvalidRequestError("another chain update is already in flight")
		}
		c.updateWait = waitCh
		c.mu.Unlock()
		frame := types.ChainUpdateFrame{
			Type:               "chain.update",
			IdempotencyKey:     newIdempotencyKey("chain_update"),
			ChainUpdatePayload: payload,
		}
		if err := c.sendWSFrame(frame); err != nil {
			c.mu.Lock()
			c.updateWait = nil
			c.mu.Unlock()
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-waitCh:
			return result.event, result.err
		}
	case TransportHTTP, TransportSSE:
		return c.updateHTTP(ctx, payload)
	default:
		return nil, core.NewInvalidRequestError("unsupported chain transport")
	}
}

func (c *Chain) Run(ctx context.Context, req *ChainRunRequest, opts ...RunOption) (*types.RunResult, error) {
	if c == nil {
		return nil, core.NewInvalidRequestError("chain is not initialized")
	}
	cfg := defaultRunConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	switch c.transport {
	case TransportWebSocket:
		stream, err := c.RunStream(ctx, req, opts...)
		if err != nil {
			return nil, err
		}
		_, err = stream.Process(StreamCallbacks{})
		if err != nil {
			return nil, err
		}
		return stream.Result(), stream.Err()
	case TransportHTTP, TransportSSE:
		return c.runHTTP(ctx, req, &cfg)
	default:
		return nil, core.NewInvalidRequestError("unsupported chain transport")
	}
}

func (c *Chain) RunStream(ctx context.Context, req *ChainRunRequest, opts ...RunOption) (*ChainStream, error) {
	if c == nil {
		return nil, core.NewInvalidRequestError("chain is not initialized")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}
	cfg := defaultRunConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	switch c.transport {
	case TransportWebSocket:
		return c.runStreamWS(req, &cfg)
	case TransportSSE:
		return c.runStreamSSE(ctx, req, &cfg)
	case TransportHTTP:
		return nil, core.NewInvalidRequestError("Chain.RunStream requires TransportSSE or TransportWebSocket")
	default:
		return nil, core.NewInvalidRequestError("unsupported chain transport")
	}
}

func (s *ChainStream) Events() <-chan ChainStreamEvent {
	if s == nil {
		ch := make(chan ChainStreamEvent)
		close(ch)
		return ch
	}
	return s.events
}

func (s *ChainStream) Result() *types.RunResult {
	if s == nil {
		return nil
	}
	<-s.done
	return s.result
}

func (s *ChainStream) Err() error {
	if s == nil {
		return nil
	}
	<-s.done
	return s.err
}

func (s *ChainStream) Close() error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		s.fail(context.Canceled)
	})
	<-s.done
	return s.err
}

func (s *ChainStream) Cancel() error {
	if s == nil {
		return nil
	}
	s.cancelMu.Lock()
	cancel := s.cancel
	if cancel == nil {
		s.cancelQueued = true
		s.cancelMu.Unlock()
		return nil
	}
	s.cancelMu.Unlock()
	return cancel()
}

func (s *ChainStream) Process(callbacks StreamCallbacks) (string, error) {
	if s == nil {
		return "", nil
	}
	var text strings.Builder
	toolMetaByIndex := make(map[int]toolStreamMeta)
	reportedErr := false
	for event := range s.events {
		switch e := event.(type) {
		case types.RunStreamEventWrapper:
			processCommonStreamEvent(e.Event, &text, callbacks, toolMetaByIndex)
		case types.AudioChunkEvent:
			if callbacks.OnAudioChunk != nil {
				data, err := decodeAudioChunk(e.Audio)
				if err != nil {
					return text.String(), err
				}
				callbacks.OnAudioChunk(data, e.Format)
			}
		case types.AudioUnavailableEvent:
			if callbacks.OnAudioUnavailable != nil {
				callbacks.OnAudioUnavailable(e.Reason, e.Message)
			}
		case types.RunStepStartEvent:
			if callbacks.OnStepStart != nil {
				callbacks.OnStepStart(e.Index)
			}
		case types.RunStepCompleteEvent:
			if callbacks.OnStepComplete != nil {
				callbacks.OnStepComplete(e.Index, &Response{MessageResponse: e.Response})
			}
		case types.RunToolCallStartEvent:
			if callbacks.OnToolCallStart != nil {
				callbacks.OnToolCallStart(e.ID, e.Name, e.Input)
			}
		case types.RunToolResultEvent:
			if callbacks.OnToolResult != nil {
				callbacks.OnToolResult(e.ID, e.Name, e.Content, typesErrorToCoreErrorOrNil(e.Error))
			}
		case ToolCallStartEvent:
			if callbacks.OnToolCallStart != nil {
				callbacks.OnToolCallStart(e.ID, e.Name, e.Input)
			}
		case ToolResultEvent:
			if callbacks.OnToolResult != nil {
				callbacks.OnToolResult(e.ID, e.Name, e.Content, e.Error)
			}
		case types.ChainErrorEvent:
			if callbacks.OnError != nil {
				callbacks.OnError(chainErrorToError(e))
			}
			reportedErr = true
		case types.RunErrorEvent:
			if callbacks.OnError != nil {
				callbacks.OnError(typesErrorToCoreError(e.Error))
			}
			reportedErr = true
		case types.RunCompleteEvent:
			// Terminal result is captured separately.
		}
	}
	if err := s.Err(); err != nil {
		if callbacks.OnError != nil && !reportedErr {
			callbacks.OnError(err)
		}
		return text.String(), err
	}
	return text.String(), nil
}

func (s *ChainsService) connectHTTP(ctx context.Context, req *ChainRequest, cfg *runConfig, transport Transport) (*Chain, error) {
	payload := types.ChainStartPayload{
		ExternalSessionID: strings.TrimSpace(req.ExternalSessionID),
		Defaults:          chainDefaultsFromRequest(req, cfg),
		History:           cloneMessages(req.Messages),
		Metadata:          cloneMap(req.Metadata),
	}
	headers := s.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("chain_start"))
	resp, endpoint, err := s.client.postGatewayJSON(ctx, "/v1/chains", &payload, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}
	eventAny, err := types.UnmarshalChainServerEventStrict(body)
	if err != nil {
		return nil, core.NewAPIError("failed to decode chain create response")
	}
	event, ok := eventAny.(types.ChainStartedEvent)
	if !ok {
		return nil, core.NewAPIError("unexpected chain create response")
	}
	return &Chain{
		client:            s.client,
		transport:         transport,
		id:                event.ChainID,
		sessionID:         event.SessionID,
		externalSessionID: event.ExternalSessionID,
		resumeToken:       event.ResumeToken,
		defaults:          event.Defaults,
		readerDone:        closedChan(),
		baseHandlers:      cloneMap(cfg.toolHandlers),
		baseTimeout:       cfg.toolTimeout,
	}, nil
}

func (s *ChainsService) connectWebSocket(ctx context.Context, req *ChainRequest, cfg *runConfig) (*Chain, error) {
	endpoint, err := chainWebsocketURL(s.client.baseURL)
	if err != nil {
		return nil, err
	}
	headers := s.client.buildChainHeaders()
	headers.Set("Sec-WebSocket-Protocol", chainSDKWSSubprotocol)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		return nil, &TransportError{Op: "CONNECT", URL: endpoint, Err: err}
	}
	frame := types.ChainStartFrame{
		Type:           "chain.start",
		IdempotencyKey: newIdempotencyKey("chain_start"),
		ChainStartPayload: types.ChainStartPayload{
			ExternalSessionID: strings.TrimSpace(req.ExternalSessionID),
			Defaults:          chainDefaultsFromRequest(req, cfg),
			History:           cloneMessages(req.Messages),
			Metadata:          cloneMap(req.Metadata),
		},
	}
	if err := conn.WriteJSON(frame); err != nil {
		_ = conn.Close()
		return nil, &TransportError{Op: "WRITE", URL: endpoint, Err: err}
	}
	started, err := readExpectedChainEvent[types.ChainStartedEvent](conn, "chain.started")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	chain := &Chain{
		client:            s.client,
		transport:         TransportWebSocket,
		id:                started.ChainID,
		sessionID:         started.SessionID,
		externalSessionID: started.ExternalSessionID,
		resumeToken:       started.ResumeToken,
		endpoint:          endpoint,
		defaults:          started.Defaults,
		conn:              conn,
		readerDone:        make(chan struct{}),
		baseHandlers:      cloneMap(cfg.toolHandlers),
		baseTimeout:       cfg.toolTimeout,
	}
	go chain.readerLoop(endpoint)
	return chain, nil
}

func (s *ChainsService) attachWebSocket(ctx context.Context, req *ChainAttachRequest, cfg *runConfig) (*Chain, error) {
	endpoint, err := chainWebsocketURL(s.client.baseURL)
	if err != nil {
		return nil, err
	}
	headers := s.client.buildChainHeaders()
	headers.Set("Sec-WebSocket-Protocol", chainSDKWSSubprotocol)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		return nil, &TransportError{Op: "CONNECT", URL: endpoint, Err: err}
	}
	frame := types.ChainAttachFrame{
		Type:               "chain.attach",
		IdempotencyKey:     newIdempotencyKey("chain_attach"),
		ChainID:            strings.TrimSpace(req.ChainID),
		ResumeToken:        strings.TrimSpace(req.ResumeToken),
		AfterEventID:       req.AfterEventID,
		RequireExactReplay: req.RequireExactReplay,
		Takeover:           req.Takeover,
	}
	if err := conn.WriteJSON(frame); err != nil {
		_ = conn.Close()
		return nil, &TransportError{Op: "WRITE", URL: endpoint, Err: err}
	}
	attached, err := readExpectedChainEvent[types.ChainAttachedEvent](conn, "chain.attached")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	chain := &Chain{
		client:       s.client,
		transport:    TransportWebSocket,
		id:           attached.ChainID,
		sessionID:    attached.SessionID,
		resumeToken:  attached.ResumeToken,
		endpoint:     endpoint,
		conn:         conn,
		readerDone:   make(chan struct{}),
		baseHandlers: cloneMap(cfg.toolHandlers),
		baseTimeout:  cfg.toolTimeout,
	}
	go chain.readerLoop(endpoint)
	return chain, nil
}

func (c *Chain) runHTTP(ctx context.Context, req *ChainRunRequest, cfg *runConfig) (*types.RunResult, error) {
	payload := chainRunPayload(req, cfg)
	headers := c.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("run_start"))
	endpointPath := "/v1/chains/" + url.PathEscape(c.id) + "/runs"
	resp, endpoint, err := c.client.postGatewayJSON(ctx, endpointPath, &payload, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}
	var envelope types.ChainRunResultEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, core.NewAPIError("failed to decode chain run response")
	}
	return envelope.Result, nil
}

func (c *Chain) updateHTTP(ctx context.Context, payload types.ChainUpdatePayload) (*types.ChainUpdatedEvent, error) {
	endpoint, err := c.client.gatewayEndpoint("/v1/chains/" + url.PathEscape(c.id))
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, core.NewInvalidRequestError("failed to marshal chain update request")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &TransportError{Op: http.MethodPatch, URL: endpoint, Err: err}
	}
	headers := c.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("chain_update"))
	headers.Set("Content-Type", "application/json")
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	resp, err := c.client.httpClient.Do(httpReq)
	if err != nil {
		return nil, &TransportError{Op: http.MethodPatch, URL: endpoint, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPatch)
	}
	var event types.ChainUpdatedEvent
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		return nil, core.NewAPIError("failed to decode chain update response")
	}
	c.defaults = event.Defaults
	return &event, nil
}

func (c *Chain) runStreamWS(req *ChainRunRequest, cfg *runConfig) (*ChainStream, error) {
	if c.conn == nil {
		return nil, core.NewInvalidRequestError("websocket chain transport is not connected")
	}
	stream := newChainStream(c, mergeToolHandlers(c.baseHandlers, cfg.toolHandlers), chainToolTimeout(c.baseTimeout, cfg.toolTimeout))
	c.mu.Lock()
	if c.current != nil {
		c.mu.Unlock()
		return nil, core.NewInvalidRequestError("another chain run is already in flight")
	}
	c.current = stream
	c.mu.Unlock()
	frame := types.RunStartFrame{
		Type:            "run.start",
		IdempotencyKey:  newIdempotencyKey("run_start"),
		RunStartPayload: chainRunPayload(req, cfg),
	}
	if err := c.sendWSFrame(frame); err != nil {
		c.clearCurrentStream(stream)
		stream.fail(err)
		return nil, err
	}
	stream.cancelMu.Lock()
	stream.cancel = func() error {
		runID := strings.TrimSpace(stream.runID)
		if runID == "" {
			stream.cancelQueued = true
			return nil
		}
		return c.sendWSFrame(types.RunCancelFrame{
			Type:           "run.cancel",
			IdempotencyKey: newIdempotencyKey("run_cancel"),
			RunID:          runID,
		})
	}
	stream.cancelMu.Unlock()
	return stream, nil
}

func (c *Chain) runStreamSSE(ctx context.Context, req *ChainRunRequest, cfg *runConfig) (*ChainStream, error) {
	payload := chainRunPayload(req, cfg)
	runCtx, cancel := context.WithCancel(ctx)
	endpoint, err := c.client.gatewayEndpoint("/v1/chains/" + url.PathEscape(c.id) + "/runs:stream")
	if err != nil {
		cancel()
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		cancel()
		return nil, core.NewInvalidRequestError("failed to marshal chain run request")
	}
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}
	headers := c.client.buildChainHeaders()
	headers.Set(chainSDKIdempotencyHeader, newIdempotencyKey("run_start"))
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	resp, err := c.client.httpClient.Do(httpReq)
	if err != nil {
		cancel()
		return nil, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cancel()
		defer resp.Body.Close()
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	stream := newChainStream(nil, nil, 0)
	stream.cancelMu.Lock()
	stream.cancel = func() error {
		cancel()
		return nil
	}
	stream.cancelMu.Unlock()
	go stream.readSSE(resp.Body, endpoint)
	return stream, nil
}

func (c *Chain) readerLoop(endpoint string) {
	defer close(c.readerDone)
	for {
		conn := c.currentConn()
		if conn == nil {
			if err := c.reconnect(endpoint); err != nil {
				c.setErr(err)
				c.failCurrent(err)
				return
			}
			continue
		}
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if c.shouldReconnect() {
				if reconnectErr := c.reconnect(endpoint); reconnectErr == nil {
					continue
				}
			}
			c.setErr(&TransportError{Op: "READ", URL: endpoint, Err: err})
			c.failCurrent(err)
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		event, err := types.UnmarshalChainServerEventStrict(data)
		if err != nil {
			c.setErr(fmt.Errorf("decode chain event: %w", err))
			c.failCurrent(err)
			return
		}
		c.handleChainEvent(event)
	}
}

func (c *Chain) handleChainEvent(event types.ChainServerEvent) {
	switch e := event.(type) {
	case types.ChainStartedEvent:
		c.resumeToken = e.ResumeToken
		c.id = e.ChainID
		c.sessionID = e.SessionID
		c.externalSessionID = e.ExternalSessionID
		c.defaults = e.Defaults
		c.setLastEventID(e.EventID)
	case types.ChainAttachedEvent:
		c.resumeToken = e.ResumeToken
		c.id = e.ChainID
		c.sessionID = e.SessionID
		c.setLastEventID(e.EventID)
	case types.ChainUpdatedEvent:
		c.defaults = e.Defaults
		c.setLastEventID(e.EventID)
		c.mu.Lock()
		waitCh := c.updateWait
		c.updateWait = nil
		c.mu.Unlock()
		if waitCh != nil {
			waitCh <- chainUpdateResult{event: &e}
		}
	case types.RunEnvelopeEvent:
		c.setLastEventID(e.EventID)
		if stream := c.currentStream(); stream != nil {
			stream.handleRunEvent(e.Event)
		}
	case types.ClientToolCallEvent:
		c.setLastEventID(e.EventID)
		if stream := c.currentStream(); stream != nil {
			stream.handleClientToolCall(e)
		}
	case types.ChainErrorEvent:
		if stream := c.currentStream(); stream != nil {
			stream.sendEvent(e)
			stream.fail(chainErrorToError(e))
			return
		}
		c.mu.Lock()
		waitCh := c.updateWait
		c.updateWait = nil
		c.mu.Unlock()
		if waitCh != nil {
			waitCh <- chainUpdateResult{err: chainErrorToError(e)}
			return
		}
		c.setErr(chainErrorToError(e))
	}
}

func (c *Chain) sendWSFrame(frame any) error {
	if c.transport != TransportWebSocket {
		return core.NewInvalidRequestError("chain websocket is not connected")
	}
	if err := c.writeWSFrame(frame); err != nil {
		if !c.shouldReconnect() {
			return err
		}
		if reconnectErr := c.reconnect(c.endpoint); reconnectErr != nil {
			return err
		}
		if retryErr := c.writeWSFrame(frame); retryErr != nil {
			return retryErr
		}
	}
	return nil
}

func (c *Chain) writeWSFrame(frame any) error {
	conn := c.currentConn()
	if conn == nil {
		return core.NewInvalidRequestError("chain websocket is not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := conn.WriteJSON(frame); err != nil {
		return &TransportError{Op: "WRITE", URL: c.endpoint, Err: err}
	}
	return nil
}

func (c *Chain) currentConn() *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *Chain) shouldReconnect() bool {
	if c == nil || c.transport != TransportWebSocket {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && strings.TrimSpace(c.id) != "" && strings.TrimSpace(c.resumeToken) != ""
}

func (c *Chain) reconnect(endpoint string) error {
	if c == nil || endpoint == "" {
		return core.NewInvalidRequestError("chain websocket is not configured")
	}
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	if !c.shouldReconnect() {
		return core.NewInvalidRequestError("chain websocket is closed")
	}

	headers := c.client.buildChainHeaders()
	headers.Set("Sec-WebSocket-Protocol", chainSDKWSSubprotocol)
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, headers)
	if err != nil {
		return &TransportError{Op: "CONNECT", URL: endpoint, Err: err}
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	attach := types.ChainAttachFrame{
		Type:               "chain.attach",
		IdempotencyKey:     newIdempotencyKey("chain_attach"),
		ChainID:            c.id,
		ResumeToken:        c.resumeToken,
		AfterEventID:       c.LastEventID(),
		RequireExactReplay: false,
		Takeover:           true,
	}
	c.writeMu.Lock()
	writeErr := conn.WriteJSON(attach)
	c.writeMu.Unlock()
	if writeErr != nil {
		_ = conn.Close()
		return &TransportError{Op: "WRITE", URL: endpoint, Err: writeErr}
	}
	mt, data, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return &TransportError{Op: "READ", URL: endpoint, Err: err}
	}
	if mt != websocket.TextMessage {
		_ = conn.Close()
		return core.NewAPIError("unexpected chain reconnect frame")
	}
	event, err := types.UnmarshalChainServerEventStrict(data)
	if err != nil {
		_ = conn.Close()
		return core.NewAPIError("failed to decode chain reconnect response")
	}
	attached, ok := event.(types.ChainAttachedEvent)
	if !ok {
		if chainErr, isChainErr := event.(types.ChainErrorEvent); isChainErr {
			_ = conn.Close()
			return chainErrorToError(chainErr)
		}
		_ = conn.Close()
		return core.NewAPIError("unexpected chain reconnect response")
	}

	c.mu.Lock()
	oldConn := c.conn
	c.conn = conn
	c.closed = false
	c.mu.Unlock()
	if oldConn != nil && oldConn != conn {
		_ = oldConn.Close()
	}
	c.handleChainEvent(attached)
	c.clearErr()
	return nil
}

func (c *Chain) currentStream() *ChainStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *Chain) clearCurrentStream(target *ChainStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == target {
		c.current = nil
	}
}

func (c *Chain) failCurrent(err error) {
	if stream := c.currentStream(); stream != nil {
		stream.fail(err)
	}
}

func (c *Chain) setErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *Chain) clearErr() {
	if c == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	c.err = nil
}

func (c *Chain) setLastEventID(eventID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastEventID = eventID
}

func newChainStream(chain *Chain, toolHandlers map[string]ToolHandler, toolTimeout time.Duration) *ChainStream {
	return &ChainStream{
		chain:        chain,
		events:       make(chan ChainStreamEvent, 128),
		done:         make(chan struct{}),
		toolHandlers: toolHandlers,
		toolTimeout:  toolTimeout,
	}
}

func (s *ChainStream) sendEvent(event ChainStreamEvent) {
	if s == nil {
		return
	}
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *ChainStream) handleRunEvent(event types.RunStreamEvent) {
	if s == nil || event == nil {
		return
	}
	switch e := event.(type) {
	case types.RunStartEvent:
		s.runID = strings.TrimSpace(e.RequestID)
		s.cancelMu.Lock()
		cancel := s.cancel
		queued := s.cancelQueued
		s.cancelQueued = false
		s.cancelMu.Unlock()
		if queued && cancel != nil {
			if err := cancel(); err != nil {
				s.sendEvent(types.RunErrorEvent{
					Type: "error",
					Error: types.Error{
						Type:    "transport_error",
						Message: err.Error(),
					},
				})
				s.fail(err)
				return
			}
		}
	case types.RunCompleteEvent:
		s.result = e.Result
		s.sendEvent(e)
		s.finish()
		return
	case types.RunErrorEvent:
		s.sendEvent(e)
		s.fail(typesErrorToCoreError(e.Error))
		return
	}
	s.sendEvent(event)
}

func (s *ChainStream) handleClientToolCall(event types.ClientToolCallEvent) {
	if s == nil {
		return
	}
	if strings.TrimSpace(s.runID) == "" {
		s.runID = strings.TrimSpace(event.RunID)
	}
	s.sendEvent(ToolCallStartEvent{ID: event.ExecutionID, Name: event.Name, Input: cloneMap(event.Input)})
	go func() {
		content, callErr, handlerErr := s.executeTool(event)
		if s.chain != nil {
			sendErr := s.chain.sendWSFrame(types.ClientToolResultFrame{
				Type:           "client_tool.result",
				IdempotencyKey: newIdempotencyKey("tool_result"),
				RunID:          event.RunID,
				ExecutionID:    event.ExecutionID,
				Content:        cloneContentBlocks(content),
				IsError:        callErr != nil,
			})
			if sendErr != nil {
				s.fail(sendErr)
				return
			}
		}
		s.sendEvent(ToolResultEvent{ID: event.ExecutionID, Name: event.Name, Content: cloneContentBlocks(content), Error: callErr})
		if handlerErr != nil {
			s.fail(handlerErr)
		}
	}()
}

func (s *ChainStream) executeTool(event types.ClientToolCallEvent) ([]types.ContentBlock, error, error) {
	handler, ok := s.toolHandlers[event.Name]
	if !ok || handler == nil {
		err := fmt.Errorf("unknown tool %q", event.Name)
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: err.Error()}}, err, nil
	}
	inputJSON, err := json.Marshal(event.Input)
	if err != nil {
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: "invalid tool input"}}, err, nil
	}
	toolCtx := context.Background()
	cancel := func() {}
	if s.toolTimeout > 0 {
		toolCtx, cancel = context.WithTimeout(toolCtx, s.toolTimeout)
	}
	defer cancel()
	if s.chain != nil && s.chain.client != nil {
		toolCtx = contextWithToolExecutionClient(toolCtx, s.chain.client)
	}
	output, err := handler(toolCtx, inputJSON)
	if err != nil {
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: err.Error()}}, err, nil
	}
	return outputToContentBlocks(output), nil, nil
}

func (s *ChainStream) fail(err error) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.err = err
		close(s.events)
		close(s.done)
		if s.chain != nil {
			s.chain.clearCurrentStream(s)
		}
	})
}

func (s *ChainStream) finish() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.events)
		close(s.done)
		if s.chain != nil {
			s.chain.clearCurrentStream(s)
		}
	})
}

func (s *ChainStream) readSSE(body io.ReadCloser, endpoint string) {
	defer func() {
		_ = body.Close()
		if s.err != nil {
			s.fail(s.err)
			return
		}
		s.finish()
	}()
	parser := newSSEParser(body)
	for {
		frame, err := parser.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			s.err = &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
			return
		}
		if len(frame.Data) == 0 || frame.Event == "ping" {
			continue
		}
		event, err := types.UnmarshalChainServerEventStrict(frame.Data)
		if err != nil {
			s.err = core.NewAPIError("failed to decode chain stream event")
			return
		}
		switch e := event.(type) {
		case types.RunEnvelopeEvent:
			s.handleRunEvent(e.Event)
		case types.ChainErrorEvent:
			s.sendEvent(e)
			s.err = chainErrorToError(e)
			return
		}
	}
}

func (c *Client) buildChainHeaders() http.Header {
	headers := c.buildLiveHeaders()
	headers.Set(vaiVersionHeader, vaiVersionValue)
	return headers
}

func chainDefaultsFromRequest(req *ChainRequest, cfg *runConfig) types.ChainDefaults {
	return types.ChainDefaults{
		Model:             strings.TrimSpace(req.Model),
		System:            req.System,
		Tools:             mergeTools(req.Tools, cfg.extraTools),
		GatewayTools:      append([]string(nil), req.GatewayTools...),
		GatewayToolConfig: cloneMap(req.GatewayToolConfig),
		ToolChoice:        cloneJSON(req.ToolChoice),
		MaxTokens:         req.MaxTokens,
		Temperature:       cloneJSON(req.Temperature),
		TopP:              cloneJSON(req.TopP),
		TopK:              cloneJSON(req.TopK),
		StopSequences:     append([]string(nil), req.StopSequences...),
		STTModel:          strings.TrimSpace(req.STTModel),
		TTSModel:          strings.TrimSpace(req.TTSModel),
		OutputFormat:      cloneJSON(req.OutputFormat),
		Output:            cloneJSON(req.Output),
		Voice:             cloneJSON(req.Voice),
		Extensions:        cloneMap(req.Extensions),
		Metadata:          cloneMap(req.ProviderMetadata),
	}
}

func chainDefaultsFromUpdate(req *ChainUpdateRequest) types.ChainDefaults {
	return types.ChainDefaults{
		Model:             strings.TrimSpace(req.Model),
		System:            req.System,
		Tools:             cloneJSON(req.Tools),
		GatewayTools:      append([]string(nil), req.GatewayTools...),
		GatewayToolConfig: cloneMap(req.GatewayToolConfig),
		ToolChoice:        cloneJSON(req.ToolChoice),
		MaxTokens:         req.MaxTokens,
		Temperature:       cloneJSON(req.Temperature),
		TopP:              cloneJSON(req.TopP),
		TopK:              cloneJSON(req.TopK),
		StopSequences:     append([]string(nil), req.StopSequences...),
		STTModel:          strings.TrimSpace(req.STTModel),
		TTSModel:          strings.TrimSpace(req.TTSModel),
		OutputFormat:      cloneJSON(req.OutputFormat),
		Output:            cloneJSON(req.Output),
		Voice:             cloneJSON(req.Voice),
		Extensions:        cloneMap(req.Extensions),
		Metadata:          cloneMap(req.ProviderMetadata),
	}
}

func chainRunPayload(req *ChainRunRequest, cfg *runConfig) types.RunStartPayload {
	payload := types.RunStartPayload{
		Input: []types.Message{{
			Role:    "user",
			Content: cloneContentBlocks(req.Input),
		}},
		Metadata: cloneMap(req.Metadata),
	}
	overrides := types.RunOverrides{
		Model:             strings.TrimSpace(req.Model),
		System:            req.System,
		Tools:             mergeTools(req.Tools, cfg.extraTools),
		GatewayTools:      append([]string(nil), req.GatewayTools...),
		GatewayToolConfig: cloneMap(req.GatewayToolConfig),
		ToolChoice:        cloneJSON(req.ToolChoice),
		MaxTokens:         req.MaxTokens,
		Temperature:       cloneJSON(req.Temperature),
		TopP:              cloneJSON(req.TopP),
		TopK:              cloneJSON(req.TopK),
		StopSequences:     append([]string(nil), req.StopSequences...),
		STTModel:          strings.TrimSpace(req.STTModel),
		TTSModel:          strings.TrimSpace(req.TTSModel),
		OutputFormat:      cloneJSON(req.OutputFormat),
		Output:            cloneJSON(req.Output),
		Voice:             cloneJSON(req.Voice),
		Extensions:        cloneMap(req.Extensions),
		Metadata:          cloneMap(req.ProviderMetadata),
	}
	if hasRunOverrides(overrides) {
		payload.Overrides = &overrides
	}
	return payload
}

func hasRunOverrides(overrides types.RunOverrides) bool {
	return strings.TrimSpace(overrides.Model) != "" ||
		overrides.System != nil ||
		len(overrides.Tools) > 0 ||
		len(overrides.GatewayTools) > 0 ||
		len(overrides.GatewayToolConfig) > 0 ||
		overrides.ToolChoice != nil ||
		overrides.MaxTokens != 0 ||
		overrides.Temperature != nil ||
		overrides.TopP != nil ||
		overrides.TopK != nil ||
		len(overrides.StopSequences) > 0 ||
		overrides.STTModel != "" ||
		overrides.TTSModel != "" ||
		overrides.OutputFormat != nil ||
		overrides.Output != nil ||
		overrides.Voice != nil ||
		len(overrides.Extensions) > 0 ||
		len(overrides.Metadata) > 0
}

func resolveChainTransport(transport Transport) Transport {
	switch transport {
	case "", TransportAuto, TransportWebSocket:
		return TransportWebSocket
	case TransportHTTP:
		return TransportHTTP
	case TransportSSE:
		return TransportSSE
	default:
		return TransportWebSocket
	}
}

func mergeToolHandlers(base, extra map[string]ToolHandler) map[string]ToolHandler {
	out := cloneMap(base)
	if out == nil {
		out = make(map[string]ToolHandler)
	}
	for name, handler := range extra {
		out[name] = handler
	}
	return out
}

func chainToolTimeout(base, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return base
}

func chainWebsocketURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	u, err := url.Parse(baseURL)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return "", core.NewInvalidRequestError("invalid gateway base URL")
	}
	if u.User != nil {
		return "", core.NewInvalidRequestError("gateway base URL must not include credentials")
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", core.NewInvalidRequestError("invalid gateway base URL")
	}
	u.RawQuery = ""
	u.Fragment = ""
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "" || basePath == "/" {
		u.Path = "/v1/chains/ws"
	} else {
		u.Path = basePath + "/v1/chains/ws"
	}
	u.RawPath = ""
	return u.String(), nil
}

func readExpectedChainEvent[T any](conn *websocket.Conn, expectedType string) (T, error) {
	var zero T
	mt, data, err := conn.ReadMessage()
	if err != nil {
		return zero, err
	}
	if mt != websocket.TextMessage {
		return zero, core.NewAPIError("unexpected non-text chain event")
	}
	event, err := types.UnmarshalChainServerEventStrict(data)
	if err != nil {
		return zero, err
	}
	switch typed := event.(type) {
	case types.ChainErrorEvent:
		return zero, chainErrorToError(typed)
	}
	if event.ChainServerEventType() != expectedType {
		return zero, core.NewAPIError("unexpected chain handshake event")
	}
	value, ok := any(event).(T)
	if ok {
		return value, nil
	}
	ptrValue, ok := any(event).(*T)
	if ok && ptrValue != nil {
		return *ptrValue, nil
	}
	return zero, core.NewAPIError("failed to decode expected chain event")
}

func newIdempotencyKey(prefix string) string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func chainErrorToError(event types.ChainErrorEvent) error {
	msg := strings.TrimSpace(event.Message)
	if msg == "" {
		msg = "chain request failed"
	}
	if strings.TrimSpace(string(event.Code)) != "" {
		return fmt.Errorf("%s (%s)", msg, event.Code)
	}
	return errors.New(msg)
}

func typesErrorToCoreErrorOrNil(err *types.Error) error {
	if err == nil {
		return nil
	}
	return typesErrorToCoreError(*err)
}

func decodeAudioChunk(data string) ([]byte, error) {
	decoded, err := io.ReadAll(base64NewDecoder(data))
	if err != nil {
		return nil, fmt.Errorf("decode audio chunk: %w", err)
	}
	return decoded, nil
}

func base64NewDecoder(value string) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(value))
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

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
