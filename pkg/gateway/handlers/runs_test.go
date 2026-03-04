package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
)

type fakeRunProvider struct {
	createResp   *types.MessageResponse
	streamEvents []types.StreamEvent
	stream       core.EventStream
}

func (p *fakeRunProvider) Name() string { return "anthropic" }
func (p *fakeRunProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}
func (p *fakeRunProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	if p.createResp != nil {
		return p.createResp, nil
	}
	return &types.MessageResponse{Type: "message", Role: "assistant", Model: req.Model, StopReason: types.StopReasonEndTurn, Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}}}, nil
}
func (p *fakeRunProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	if p.stream != nil {
		return p.stream, nil
	}
	return &fakeEventStream{events: p.streamEvents}, nil
}

type delayedEOFStream struct {
	delay time.Duration
	once  bool
}

func (s *delayedEOFStream) Next() (types.StreamEvent, error) {
	if s.once {
		return nil, io.EOF
	}
	s.once = true
	time.Sleep(s.delay)
	return nil, io.EOF
}

func (s *delayedEOFStream) Close() error { return nil }

type timeoutRunProvider struct{}

func (p *timeoutRunProvider) Name() string { return "anthropic" }
func (p *timeoutRunProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}
func (p *timeoutRunProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *timeoutRunProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return &delayedEOFStream{delay: time.Second}, nil
}

func baseRunsConfig() config.Config {
	return config.Config{
		MaxBodyBytes:              1 << 20,
		ModelAllowlist:            map[string]struct{}{},
		SSEPingInterval:           10 * time.Millisecond,
		SSEMaxStreamDuration:      time.Minute,
		StreamIdleTimeout:         time.Second,
		TavilyBaseURL:             "https://api.tavily.com",
		ExaBaseURL:                "https://api.exa.ai",
		FirecrawlBaseURL:          "https://api.firecrawl.dev",
		LimitMaxConcurrentStreams: 4,
	}
}

func TestRunsHandler_MethodNotAllowed(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"method_not_allowed"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_MissingProviderHeader(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"provider_key_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ServerToolMissingProvider_Fails(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_search"]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"tool_provider_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ServerToolInferProviderFromHeader_Succeeds(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_search"]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunsHandler_BuiltinsAliasStillWorks(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"builtins":["vai_web_search"]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunsHandler_ServerToolMissingKey_Fails401(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_search"],
		"server_tool_config":{"vai_web_search":{"provider":"tavily"}}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"provider_key_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Tavily"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ServerToolAmbiguousInference_Fails(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_search"]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	req.Header.Set("X-Provider-Key-Exa", "exa-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"tool_provider_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ServerToolExaMissingKey_Fails401(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_search"],
		"server_tool_config":{"vai_web_search":{"provider":"exa"}}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Exa"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ServerFetchInferProviderFromHeader_Succeeds(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_fetch"]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunsHandler_ServerFetchTavilyMissingKey_Fails401(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"server_tools":["vai_web_fetch"],
		"server_tool_config":{"vai_web_fetch":{"provider":"tavily"}}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Tavily"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_ModelAllowlistDenied(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.ModelAllowlist = map[string]struct{}{"anthropic/allowed": {}}
	h := RunsHandler{Config: cfg, Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunsHandler_CompatibilityErrorsIncludeCompatIssues(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{
			"model":"openai/gpt-4o",
			"messages":[{"role":"user","content":[{"type":"video","source":{"type":"base64","media_type":"video/mp4","data":"Zm9v"}}]}]
		}
	}`)))
	req.Header.Set("X-Provider-Key-OpenAI", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"compat_issues"`) || !strings.Contains(body, `"unsupported_content_block"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestRunsHandler_BlockingSuccess(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"result"`) || !strings.Contains(rr.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRunsHandler_LegacyGeminiProviderRejected(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"gemini-oauth/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Gemini", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider has been removed") {
		t.Fatalf("expected migration error, got: %s", rr.Body.String())
	}
}

func TestRunsHandler_BlockingRunTimeoutReturnsResult(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &timeoutRunProvider{}}, Stream: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"run":{"timeout_ms":1000}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"result"`) || !strings.Contains(body, `"stop_reason":"timeout"`) {
		t.Fatalf("body=%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("expected result envelope, got error body=%s", body)
	}
}

func TestRunsHandler_StreamSSE(t *testing.T) {
	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn

	h := RunsHandler{
		Config: baseRunsConfig(),
		Upstreams: fakeFactory{p: &fakeRunProvider{streamEvents: []types.StreamEvent{
			types.MessageStartEvent{Type: "message_start", Message: types.MessageResponse{Type: "message", Role: "assistant", Model: "test"}},
			types.ContentBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: types.TextBlock{Type: "text", Text: ""}},
			types.ContentBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: types.TextDelta{Type: "text_delta", Text: "ok"}},
			delta,
		}}},
		Stream: true,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control=%q", got)
	}
	if got := rr.Header().Get("Connection"); got != "keep-alive" {
		t.Fatalf("connection=%q", got)
	}
	if got := rr.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("x-accel-buffering=%q", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: run_start") || !strings.Contains(body, "event: step_start") || !strings.Contains(body, "event: stream_event") || !strings.Contains(body, "event: run_complete") {
		t.Fatalf("body=%s", body)
	}
}

func TestRunsHandler_Stream_EmitsPing(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.SSEPingInterval = 5 * time.Millisecond
	cfg.StreamIdleTimeout = 250 * time.Millisecond
	h := RunsHandler{
		Config:    cfg,
		Upstreams: fakeFactory{p: &fakeRunProvider{stream: &delayedEOFStream{delay: 40 * time.Millisecond}}},
		Stream:    true,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "event: ping") {
		t.Fatalf("expected ping in body: %s", rr.Body.String())
	}
}

func TestRunsHandler_Stream_IdleTimeout(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.SSEPingInterval = 5 * time.Millisecond
	cfg.StreamIdleTimeout = 15 * time.Millisecond
	h := RunsHandler{
		Config:    cfg,
		Upstreams: fakeFactory{p: &fakeRunProvider{stream: &delayedEOFStream{delay: 150 * time.Millisecond}}},
		Stream:    true,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream stream idle timeout") {
		t.Fatalf("body=%s", body)
	}
}

func TestRunsHandler_Stream_RunTimeoutEmitsCancelledRunComplete(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.SSEPingInterval = 5 * time.Millisecond
	cfg.StreamIdleTimeout = 5 * time.Second
	h := RunsHandler{
		Config:    cfg,
		Upstreams: fakeFactory{p: &fakeRunProvider{stream: &delayedEOFStream{delay: 2500 * time.Millisecond}}},
		Stream:    true,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"run":{"timeout_ms":1000}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: run_complete") || !strings.Contains(body, `"stop_reason":"cancelled"`) {
		t.Fatalf("body=%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected error event for run timeout: %s", body)
	}
}

func TestRunsHandler_Stream_UsesMinOfRunTimeoutAndSSEMaxDuration(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.SSEPingInterval = 5 * time.Millisecond
	cfg.SSEMaxStreamDuration = 80 * time.Millisecond
	cfg.StreamIdleTimeout = 5 * time.Second
	h := RunsHandler{
		Config:    cfg,
		Upstreams: fakeFactory{p: &fakeRunProvider{stream: &delayedEOFStream{delay: 2 * time.Second}}},
		Stream:    true,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]},
		"run":{"timeout_ms":300000}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("stream should have been capped by SSE max duration, elapsed=%v", elapsed)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: run_complete") || !strings.Contains(body, `"stop_reason":"cancelled"`) {
		t.Fatalf("body=%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected error event when duration limit reached: %s", body)
	}
}

func TestRunsHandler_Stream_DrainingRejected(t *testing.T) {
	lc := &lifecycle.Lifecycle{}
	lc.SetDraining(true)
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{streamEvents: []types.StreamEvent{}}}, Stream: true, Lifecycle: lc}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 529 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunsHandler_Stream_ClientDisconnectSilent(t *testing.T) {
	h := RunsHandler{Config: baseRunsConfig(), Upstreams: fakeFactory{p: &fakeRunProvider{streamEvents: []types.StreamEvent{}}}, Stream: true}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs:stream", bytes.NewReader([]byte(`{
		"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}
	}`))).WithContext(ctx)
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != 0 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

var _ = io.EOF
