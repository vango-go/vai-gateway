package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
)

type fakeFactory struct {
	p core.Provider
}

func (f fakeFactory) New(providerName, apiKey string) (core.Provider, error) {
	return f.p, nil
}

type fakeProvider struct {
	streamEvents []types.StreamEvent
}

func (p *fakeProvider) Name() string { return "anthropic" }

func (p *fakeProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}

func (p *fakeProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return &types.MessageResponse{
		ID:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "anthropic/" + req.Model,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}},
		StopReason: types.StopReasonEndTurn,
	}, nil
}

func (p *fakeProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return &fakeEventStream{events: p.streamEvents}, nil
}

type fakeEventStream struct {
	events []types.StreamEvent
	i      int
	closed bool
}

func (s *fakeEventStream) Next() (types.StreamEvent, error) {
	if s.i >= len(s.events) {
		return nil, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *fakeEventStream) Close() error {
	s.closed = true
	return nil
}

func TestMessagesHandler_NonStream(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	if got := rr.Header().Get("X-Model"); got != "anthropic/test" {
		t.Fatalf("X-Model=%q, expected anthropic/test", got)
	}
	if !strings.Contains(rr.Body.String(), `"model":"anthropic/test"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestMessagesHandler_MethodNotAllowed_IsCanonicalJSON(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"type":"invalid_request_error"`) {
		t.Fatalf("unexpected body: %q", body)
	}
	if !strings.Contains(body, `"code":"method_not_allowed"`) {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestMessagesHandler_Stream_DrainingRejected(t *testing.T) {
	lc := &lifecycle.Lifecycle{}
	lc.SetDraining(true)

	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
		Lifecycle: lc,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 529 {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"overloaded_error"`) {
		t.Fatalf("unexpected body: %q", rr.Body.String())
	}
}

func TestMessagesHandler_MissingUpstreamHeader(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"messages":[{"role":"user","content":"hello"}]
	}`)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"provider_key_missing"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestMessagesHandler_RejectsLegacyVoiceInput(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"messages":[{"role":"user","content":"hello"}],
		"voice":{"input":{"model":"ink-whisper"}}
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"voice.input"`) {
		t.Fatalf("expected voice.input param, body=%s", rr.Body.String())
	}
}

func TestMessagesHandler_AudioSTTRequiresCartesiaHeader(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"messages":[{"role":"user","content":[{"type":"audio_stt","source":{"type":"base64","media_type":"audio/wav","data":"AAAA"}}]}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Cartesia"`) {
		t.Fatalf("expected cartesia header param, body=%s", rr.Body.String())
	}
}

func TestMessagesHandler_AudioSTTUnsupportedModelProviderRejected(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stt_model":"elevenlabs/whisper-v3",
		"messages":[{"role":"user","content":[{"type":"audio_stt","source":{"type":"base64","media_type":"audio/wav","data":"AAAA"}}]}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
	req.Header.Set("X-Provider-Key-Cartesia", "sk-cartesia")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"stt_model"`) {
		t.Fatalf("expected stt_model param, body=%s", rr.Body.String())
	}
}

func TestMessagesHandler_CompatibilityError_OpenAIVideo(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"openai/gpt-4o",
		"messages":[{"role":"user","content":[{"type":"video","source":{"type":"base64","media_type":"video/mp4","data":"Zm9v"}}]}]
	}`)))
	req.Header.Set("X-Provider-Key-OpenAI", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var envelope map[string]map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	errObj := envelope["error"]
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type=%v, want invalid_request_error", errObj["type"])
	}
	if _, ok := errObj["param"]; ok {
		t.Fatalf("top-level param should be omitted when compat_issues present: %#v", errObj["param"])
	}
	issues, ok := errObj["compat_issues"].([]any)
	if !ok || len(issues) == 0 {
		t.Fatalf("compat_issues missing: %#v", errObj["compat_issues"])
	}
	first, ok := issues[0].(map[string]any)
	if !ok {
		t.Fatalf("compat_issues[0] wrong type: %T", issues[0])
	}
	if first["code"] != "unsupported_content_block" {
		t.Fatalf("compat issue code=%v, want unsupported_content_block", first["code"])
	}
}

func TestMessagesHandler_CompatibilityError_OpenAIToolType(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"openai/gpt-4o",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"web_search"}]
	}`)))
	req.Header.Set("X-Provider-Key-OpenAI", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"unsupported_tool_type"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestMessagesHandler_CompatibilityError_UnsupportedThinking(t *testing.T) {
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"openai/gpt-4o",
		"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"}]}]
	}`)))
	req.Header.Set("X-Provider-Key-OpenAI", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"unsupported_thinking"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestMessagesHandler_StreamSSE(t *testing.T) {
	fp := &fakeProvider{
		streamEvents: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
			types.MessageStopEvent{Type: "message_stop"},
		},
	}

	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEMaxStreamDuration: time.Minute,
			SSEPingInterval:      time.Second,
		},
		Upstreams: fakeFactory{p: fp},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
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
	body := rr.Body.String()
	if !strings.Contains(body, "event: message_start\n") {
		t.Fatalf("missing message_start event: %q", body)
	}
	if !strings.Contains(body, `"type":"message_start"`) {
		t.Fatalf("missing message_start payload: %q", body)
	}
	if !strings.Contains(body, "event: message_stop\n") {
		t.Fatalf("missing message_stop event: %q", body)
	}
}

type blockingEventStream struct {
	closed chan struct{}
}

func newBlockingEventStream() *blockingEventStream {
	return &blockingEventStream{closed: make(chan struct{})}
}

func (s *blockingEventStream) Next() (types.StreamEvent, error) {
	<-s.closed
	return nil, io.EOF
}

func (s *blockingEventStream) Close() error {
	select {
	case <-s.closed:
		// already closed
	default:
		close(s.closed)
	}
	return nil
}

type blockingProvider struct {
	es *blockingEventStream
}

func (p *blockingProvider) Name() string { return "anthropic" }

func (p *blockingProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}

func (p *blockingProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return nil, core.NewInvalidRequestError("not supported")
}

func (p *blockingProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return p.es, nil
}

type delayedEvent struct {
	delay time.Duration
	ev    types.StreamEvent
	err   error
}

type delayedEventStream struct {
	events []delayedEvent
	i      int
	closed chan struct{}
}

func newDelayedEventStream(events []delayedEvent) *delayedEventStream {
	return &delayedEventStream{
		events: events,
		closed: make(chan struct{}),
	}
}

func (s *delayedEventStream) Next() (types.StreamEvent, error) {
	if s.i >= len(s.events) {
		return nil, io.EOF
	}
	next := s.events[s.i]
	s.i++

	timer := time.NewTimer(next.delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return next.ev, next.err
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *delayedEventStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

type delayedProvider struct {
	es core.EventStream
}

func (p *delayedProvider) Name() string { return "anthropic" }

func (p *delayedProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}

func (p *delayedProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return nil, core.NewInvalidRequestError("not supported")
}

func (p *delayedProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return p.es, nil
}

type streamErrProvider struct {
	err error
}

func (p *streamErrProvider) Name() string { return "anthropic" }

func (p *streamErrProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}

func (p *streamErrProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return nil, p.err
}

func (p *streamErrProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return nil, p.err
}

type fakeStreamingTTSProvider struct {
	failOnSend    bool
	chunksPerSend int
	chunksOnFinal int
	chunkPayload  []byte
}

func (p *fakeStreamingTTSProvider) Name() string { return "fake-tts" }

func (p *fakeStreamingTTSProvider) Synthesize(ctx context.Context, text string, opts tts.SynthesizeOptions) (*tts.Synthesis, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *fakeStreamingTTSProvider) SynthesizeStream(ctx context.Context, text string, opts tts.SynthesizeOptions) (*tts.SynthesisStream, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *fakeStreamingTTSProvider) NewStreamingContext(ctx context.Context, opts tts.StreamingContextOptions) (*tts.StreamingContext, error) {
	sc := tts.NewStreamingContext()
	var finished sync.Once
	chunkPayload := p.chunkPayload
	if len(chunkPayload) == 0 {
		chunkPayload = []byte{0x01, 0x02, 0x03}
	}
	chunksPerSend := p.chunksPerSend
	if chunksPerSend <= 0 {
		chunksPerSend = 1
	}
	sc.SendFunc = func(text string, isFinal bool) error {
		if p.failOnSend && strings.TrimSpace(text) != "" {
			err := fmt.Errorf("forced tts failure")
			sc.SetError(err)
			finished.Do(func() { sc.FinishAudio() })
			return err
		}
		if strings.TrimSpace(text) != "" {
			for i := 0; i < chunksPerSend; i++ {
				if !sc.PushAudio(chunkPayload) {
					return nil
				}
			}
		}
		if isFinal {
			for i := 0; i < p.chunksOnFinal; i++ {
				if !sc.PushAudio(chunkPayload) {
					break
				}
			}
			// Simulate Cartesia behavior: after the final chunk, the
			// provider generates remaining audio and closes the stream.
			finished.Do(func() { sc.FinishAudio() })
		}
		return nil
	}
	sc.CloseFunc = func() error {
		finished.Do(func() { sc.FinishAudio() })
		return nil
	}
	return sc, nil
}

func newVoiceStreamingRequest() *types.MessageRequest {
	return &types.MessageRequest{
		Model: "test",
		Voice: &types.VoiceConfig{
			Output: &types.VoiceOutputConfig{
				Voice:      "test-voice",
				Format:     types.VoiceFormatPCM,
				SampleRate: 24000,
			},
		},
	}
}

func TestMessagesHandler_Stream_PingAndMaxDurationTimeout(t *testing.T) {
	es := newBlockingEventStream()
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEPingInterval:      10 * time.Millisecond,
			SSEMaxStreamDuration: 80 * time.Millisecond,
		},
		Upstreams: fakeFactory{p: &blockingProvider{es: es}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: ping\n") {
		t.Fatalf("missing ping event: %q", body)
	}
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("missing error event: %q", body)
	}
	if !strings.Contains(body, "request timeout") {
		t.Fatalf("missing timeout message: %q", body)
	}
}

func TestMessagesHandler_Stream_UpstreamIdleTimeout(t *testing.T) {
	es := newBlockingEventStream()
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEPingInterval:      10 * time.Millisecond,
			SSEMaxStreamDuration: 5 * time.Second,
			StreamIdleTimeout:    60 * time.Millisecond,
		},
		Upstreams: fakeFactory{p: &blockingProvider{es: es}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: ping\n") {
		t.Fatalf("missing ping event: %q", body)
	}
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("missing error event: %q", body)
	}
	if !strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("missing api_error type: %q", body)
	}
	if !strings.Contains(body, "upstream stream idle timeout") {
		t.Fatalf("missing idle-timeout message: %q", body)
	}

	select {
	case <-es.closed:
	default:
		t.Fatalf("expected upstream stream to be closed on idle timeout")
	}
}

func TestMessagesHandler_Stream_UpstreamActivityResetsIdleTimer(t *testing.T) {
	es := newDelayedEventStream([]delayedEvent{
		{
			delay: 20 * time.Millisecond,
			ev: types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
		},
		{
			delay: 20 * time.Millisecond,
			ev:    types.MessageStopEvent{Type: "message_stop"},
		},
	})

	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEPingInterval:      time.Second,
			SSEMaxStreamDuration: 2 * time.Second,
			StreamIdleTimeout:    80 * time.Millisecond,
		},
		Upstreams: fakeFactory{p: &delayedProvider{es: es}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: message_stop\n") {
		t.Fatalf("expected normal stream completion, got %q", body)
	}
	if strings.Contains(body, "upstream stream idle timeout") {
		t.Fatalf("unexpected idle-timeout error for active upstream stream: %q", body)
	}
}

func TestMessagesHandler_Stream_ClientDisconnect_ClosesUpstream(t *testing.T) {
	es := newBlockingEventStream()
	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEPingInterval:      1 * time.Second,
			SSEMaxStreamDuration: 5 * time.Second,
		},
		Upstreams: fakeFactory{p: &blockingProvider{es: es}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))).WithContext(ctx)
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}()

	cancel()
	<-done

	select {
	case <-es.closed:
		// ok
	default:
		t.Fatalf("expected upstream stream to be closed on client disconnect")
	}
}

func TestMessagesHandler_Stream_ErrorEventIncludesCompatIssues(t *testing.T) {
	compatErr := &core.Error{
		Type:    core.ErrInvalidRequest,
		Message: "compat failure",
		CompatIssues: []core.CompatibilityIssue{
			{
				Severity: "error",
				Param:    "messages[0].content[0]",
				Code:     "unsupported_content_block",
				Message:  "video blocks are not supported by anthropic",
			},
		},
	}

	h := MessagesHandler{
		Config: config.Config{
			MaxBodyBytes:         1 << 20,
			ModelAllowlist:       map[string]struct{}{},
			SSEPingInterval:      time.Second,
			SSEMaxStreamDuration: time.Second,
		},
		Upstreams: fakeFactory{p: &streamErrProvider{err: compatErr}},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"anthropic/test",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("X-Provider-Key-Anthropic", "sk-test")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("missing stream error event: %q", body)
	}
	if !strings.Contains(body, `"compat_issues":[`) {
		t.Fatalf("missing compat_issues in stream error event: %q", body)
	}
	if !strings.Contains(body, `"unsupported_content_block"`) {
		t.Fatalf("missing compatibility issue code in stream error event: %q", body)
	}
}

func TestMessagesHandler_Stream_MessageStartIncludesUserTranscript(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
			types.MessageStopEvent{Type: "message_stop"},
		},
	}

	h := MessagesHandler{
		Config: config.Config{
			SSEMaxStreamDuration: time.Second,
			StreamIdleTimeout:    time.Second,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	h.serveStream(rr, req, req.Context(), "req_test", provider, &types.MessageRequest{Model: "test"}, nil, "hello from stt")

	body := rr.Body.String()
	if !strings.Contains(body, "event: message_start\n") {
		t.Fatalf("missing message_start event: %q", body)
	}
	if !strings.Contains(body, `"user_transcript":"hello from stt"`) {
		t.Fatalf("missing user transcript metadata in message_start: %q", body)
	}
}

func TestMessagesHandler_Stream_VoiceAudioChunkFormatAndFinalMarker(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
			types.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: types.TextBlock{Type: "text", Text: ""},
			},
			types.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: types.TextDelta{Type: "text_delta", Text: "Hello."},
			},
			types.ContentBlockStopEvent{Type: "content_block_stop", Index: 0},
			types.MessageStopEvent{Type: "message_stop"},
		},
	}

	voicePipeline := voice.NewPipelineWithProviders(nil, &fakeStreamingTTSProvider{})
	h := MessagesHandler{
		Config: config.Config{
			SSEMaxStreamDuration: time.Second,
			StreamIdleTimeout:    time.Second,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	h.serveStream(rr, req, req.Context(), "req_test", provider, newVoiceStreamingRequest(), voicePipeline, "")

	body := rr.Body.String()
	if !strings.Contains(body, "event: audio_chunk\n") {
		t.Fatalf("missing audio_chunk events: %q", body)
	}
	if !strings.Contains(body, `"format":"pcm_s16le"`) {
		t.Fatalf("expected pcm_s16le format in audio_chunk event: %q", body)
	}
	if !strings.Contains(body, `"is_final":true`) {
		t.Fatalf("missing final audio chunk marker: %q", body)
	}
	if !strings.Contains(body, "event: message_stop\n") {
		t.Fatalf("missing message_stop event: %q", body)
	}
}

func TestMessagesHandler_Stream_VoiceLongBurst_NoBackpressureTruncation(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
			types.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: types.TextBlock{Type: "text", Text: ""},
			},
			types.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: types.TextDelta{Type: "text_delta", Text: "Long response body."},
			},
			types.ContentBlockStopEvent{Type: "content_block_stop", Index: 0},
			types.MessageStopEvent{Type: "message_stop"},
		},
	}

	voicePipeline := voice.NewPipelineWithProviders(nil, &fakeStreamingTTSProvider{
		chunksOnFinal: 220,
		chunkPayload:  []byte{0x09, 0x08, 0x07, 0x06},
	})
	h := MessagesHandler{
		Config: config.Config{
			SSEMaxStreamDuration: 2 * time.Second,
			StreamIdleTimeout:    2 * time.Second,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	h.serveStream(rr, req, req.Context(), "req_test", provider, newVoiceStreamingRequest(), voicePipeline, "")

	body := rr.Body.String()
	if strings.Count(body, "event: audio_chunk\n") < 150 {
		t.Fatalf("expected many audio chunks, got %d; body=%q", strings.Count(body, "event: audio_chunk\n"), body)
	}
	if strings.Count(body, `"is_final":true`) != 1 {
		t.Fatalf("expected exactly one final marker, got %d; body=%q", strings.Count(body, `"is_final":true`), body)
	}
	if strings.Contains(body, `"reason":"backpressure"`) {
		t.Fatalf("unexpected backpressure audio_unavailable event: %q", body)
	}
	if !strings.Contains(body, "event: message_stop\n") {
		t.Fatalf("missing message_stop event: %q", body)
	}
}

func TestMessagesHandler_Stream_TTSFailureEmitsAudioUnavailableAndContinues(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					ID:    "msg_1",
					Type:  "message",
					Role:  "assistant",
					Model: "anthropic/test",
				},
			},
			types.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        0,
				ContentBlock: types.TextBlock{Type: "text", Text: ""},
			},
			types.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: types.TextDelta{Type: "text_delta", Text: "Hello."},
			},
			types.ContentBlockStopEvent{Type: "content_block_stop", Index: 0},
			types.MessageStopEvent{Type: "message_stop"},
		},
	}

	voicePipeline := voice.NewPipelineWithProviders(nil, &fakeStreamingTTSProvider{failOnSend: true})
	h := MessagesHandler{
		Config: config.Config{
			SSEMaxStreamDuration: time.Second,
			StreamIdleTimeout:    time.Second,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	h.serveStream(rr, req, req.Context(), "req_test", provider, newVoiceStreamingRequest(), voicePipeline, "")

	body := rr.Body.String()
	if !strings.Contains(body, "event: audio_unavailable\n") {
		t.Fatalf("missing audio_unavailable event: %q", body)
	}
	if !strings.Contains(body, `"reason":"tts_failed"`) {
		t.Fatalf("missing tts_failed reason in audio_unavailable event: %q", body)
	}
	if !strings.Contains(body, "TTS synthesis failed: forced tts failure") {
		t.Fatalf("missing deterministic tts failure message: %q", body)
	}
	if !strings.Contains(body, "event: message_stop\n") {
		t.Fatalf("text stream did not complete after TTS failure: %q", body)
	}
	if strings.Contains(body, "event: error\n") {
		t.Fatalf("tts failure should not emit terminal error event: %q", body)
	}

	unavailableIdx := strings.Index(body, "event: audio_unavailable\n")
	if unavailableIdx == -1 {
		t.Fatalf("missing audio_unavailable event: %q", body)
	}
	if strings.Contains(body[unavailableIdx:], "event: audio_chunk\n") {
		t.Fatalf("audio_chunk emitted after audio_unavailable: %q", body)
	}
	if strings.Contains(body, `"is_final":true`) {
		t.Fatalf("final audio marker should not be emitted after audio_unavailable: %q", body)
	}
}
