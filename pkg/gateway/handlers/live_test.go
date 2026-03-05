package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
)

type fakeLiveSTTSession struct {
	transcripts chan stt.TranscriptDelta
}

type failingLiveSTTSession struct{}

type fakeLiveTTSSession struct {
	audioCh       chan []byte
	flushResultCh chan error
	flushCalledCh chan struct{}
	closeCalledCh chan struct{}
	closeCalls    atomic.Int32
}

func newFakeLiveSTTSession() *fakeLiveSTTSession {
	return &fakeLiveSTTSession{transcripts: make(chan stt.TranscriptDelta)}
}

func (s *fakeLiveSTTSession) SendAudio(data []byte) error {
	return nil
}

func (s *fakeLiveSTTSession) Transcripts() <-chan stt.TranscriptDelta {
	return s.transcripts
}

func (s *fakeLiveSTTSession) Close() error {
	select {
	case <-s.transcripts:
	default:
		close(s.transcripts)
	}
	return nil
}

func (f failingLiveSTTSession) SendAudio(data []byte) error { return errors.New("broken pipe") }

func (f failingLiveSTTSession) Transcripts() <-chan stt.TranscriptDelta {
	ch := make(chan stt.TranscriptDelta)
	close(ch)
	return ch
}

func (f failingLiveSTTSession) Close() error { return nil }

func (f *fakeLiveTTSSession) OnTextDelta(text string) error { return nil }

func (f *fakeLiveTTSSession) Flush() error {
	if f.flushCalledCh != nil {
		select {
		case f.flushCalledCh <- struct{}{}:
		default:
		}
	}
	if f.flushResultCh != nil {
		return <-f.flushResultCh
	}
	return nil
}

func (f *fakeLiveTTSSession) Close() error {
	f.closeCalls.Add(1)
	if f.closeCalledCh != nil {
		select {
		case f.closeCalledCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeLiveTTSSession) Audio() <-chan []byte {
	if f.audioCh == nil {
		ch := make(chan []byte)
		close(ch)
		return ch
	}
	return f.audioCh
}

func (f *fakeLiveTTSSession) Err() error { return nil }

func dialLiveTestConn(t *testing.T, serverURL string, headers http.Header) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/live"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial live websocket: %v", err)
	}
	return conn
}

func readLiveEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("unmarshal event json: %v", err)
	}
	return event
}

func TestLiveHandler_NonWebSocketUpgradeRejected(t *testing.T) {
	h := LiveHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/live", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ws_upgrade_required") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestLiveHandler_BinaryFirstFrameReturnsFatalError(t *testing.T) {
	h := LiveHandler{
		Config: config.Config{
			WSMaxSessionDuration:      time.Minute,
			WSMaxSessionsPerPrincipal: 2,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/live", h)
	server := httptest.NewServer(mux)
	defer server.Close()

	headers := http.Header{}
	conn := dialLiveTestConn(t, server.URL, headers)
	defer conn.Close()

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("write binary start frame: %v", err)
	}

	event := readLiveEvent(t, conn)
	if got, _ := event["type"].(string); got != "error" {
		t.Fatalf("type=%v, want error", event["type"])
	}
	if got, _ := event["fatal"].(bool); !got {
		t.Fatalf("fatal=%v, want true", event["fatal"])
	}
}

func TestLiveHandler_StartRequiresCartesiaHeader(t *testing.T) {
	h := LiveHandler{
		Config: config.Config{
			WSMaxSessionDuration:      time.Minute,
			WSMaxSessionsPerPrincipal: 2,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/live", h)
	server := httptest.NewServer(mux)
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Provider-Key-Anthropic", "sk-test")
	conn := dialLiveTestConn(t, server.URL, headers)
	defer conn.Close()

	start := map[string]any{
		"type": "start",
		"run_request": map[string]any{
			"request": map[string]any{
				"model": "anthropic/test",
				"messages": []map[string]any{{
					"role":    "user",
					"content": "hello",
				}},
				"voice": map[string]any{
					"output": map[string]any{
						"voice":       "a167e0f3-df7e-4d52-a9c3-f949145efdab",
						"format":      "pcm",
						"sample_rate": 24000,
					},
				},
			},
			"run": map[string]any{
				"max_turns":       1,
				"max_tool_calls":  1,
				"timeout_ms":      1000,
				"parallel_tools":  true,
				"tool_timeout_ms": 1000,
			},
		},
	}
	if err := conn.WriteJSON(start); err != nil {
		t.Fatalf("write start frame: %v", err)
	}

	event := readLiveEvent(t, conn)
	if got, _ := event["type"].(string); got != "error" {
		t.Fatalf("type=%v, want error", event["type"])
	}
	if got, _ := event["code"].(string); got != "provider_key_missing" {
		t.Fatalf("code=%v, want provider_key_missing", event["code"])
	}
}

func TestLiveHandler_EmptyMessagesAndZeroRunFieldsStillStartSession(t *testing.T) {
	oldNewVoicePipeline := newLiveVoicePipelineFunc
	oldNewSTTSession := newLiveSTTSessionFunc
	defer func() {
		newLiveVoicePipelineFunc = oldNewVoicePipeline
		newLiveSTTSessionFunc = oldNewSTTSession
	}()

	gotSTTModel := ""
	newLiveVoicePipelineFunc = func(cartesiaKey string, httpClient *http.Client) *voice.Pipeline {
		return &voice.Pipeline{}
	}
	newLiveSTTSessionFunc = func(ctx context.Context, pipeline *voice.Pipeline, model string) (liveSTTSession, error) {
		gotSTTModel = model
		return newFakeLiveSTTSession(), nil
	}

	h := LiveHandler{
		Config: config.Config{
			WSMaxSessionDuration:      time.Minute,
			WSMaxSessionsPerPrincipal: 2,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/live", h)
	server := httptest.NewServer(mux)
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Provider-Key-Anthropic", "sk-test")
	headers.Set("X-Provider-Key-Cartesia", "sk-cartesia")
	conn := dialLiveTestConn(t, server.URL, headers)
	defer conn.Close()

	start := map[string]any{
		"type": "start",
		"run_request": map[string]any{
			"request": map[string]any{
				"model":    "anthropic/test",
				"messages": []any{},
				"voice": map[string]any{
					"output": map[string]any{
						"voice":       "a167e0f3-df7e-4d52-a9c3-f949145efdab",
						"format":      "pcm",
						"sample_rate": 24000,
					},
				},
			},
			"run": map[string]any{
				"max_turns":       0,
				"max_tool_calls":  0,
				"timeout_ms":      0,
				"parallel_tools":  true,
				"tool_timeout_ms": 0,
			},
		},
	}
	if err := conn.WriteJSON(start); err != nil {
		t.Fatalf("write start frame: %v", err)
	}

	event := readLiveEvent(t, conn)
	if got, _ := event["type"].(string); got != "session_started" {
		t.Fatalf("type=%v, want session_started", event["type"])
	}
	if got, _ := event["input_sample_rate_hz"].(float64); int(got) != 16000 {
		t.Fatalf("input_sample_rate_hz=%v, want 16000", event["input_sample_rate_hz"])
	}
	if got, _ := event["output_sample_rate_hz"].(float64); int(got) != 24000 {
		t.Fatalf("output_sample_rate_hz=%v, want 24000", event["output_sample_rate_hz"])
	}
	if gotSTTModel != "ink-whisper" {
		t.Fatalf("stt model passed to streaming session=%q, want %q", gotSTTModel, "ink-whisper")
	}
}

func TestNormalizeLiveRunRequestForStrict_SeedsMessagesAndDropsZeroLimits(t *testing.T) {
	raw := json.RawMessage(`{
		"request": {
			"model": "anthropic/test",
			"messages": []
		},
		"run": {
			"max_turns": 0,
			"max_tool_calls": 0,
			"timeout_ms": 0,
			"parallel_tools": true,
			"tool_timeout_ms": 0
		}
	}`)

	normalized, seeded := normalizeLiveRunRequestForStrict(raw)
	if !seeded {
		t.Fatal("seeded=false, want true")
	}

	req, err := types.UnmarshalRunRequestStrict(normalized)
	if err != nil {
		t.Fatalf("UnmarshalRunRequestStrict(normalized) error: %v", err)
	}
	if len(req.Request.Messages) != 1 {
		t.Fatalf("len(messages)=%d, want 1 seeded message", len(req.Request.Messages))
	}
	if req.Run.MaxTurns <= 0 {
		t.Fatalf("run.max_turns=%d, want >0 default", req.Run.MaxTurns)
	}
	if req.Run.MaxToolCalls <= 0 {
		t.Fatalf("run.max_tool_calls=%d, want >0 default", req.Run.MaxToolCalls)
	}
	if req.Run.TimeoutMS <= 0 {
		t.Fatalf("run.timeout_ms=%d, want >0 default", req.Run.TimeoutMS)
	}
	if req.Run.ToolTimeoutMS <= 0 {
		t.Fatalf("run.tool_timeout_ms=%d, want >0 default", req.Run.ToolTimeoutMS)
	}
}

func TestLiveHandler_MissingOutputSampleRateDefaultsTo16K(t *testing.T) {
	oldNewVoicePipeline := newLiveVoicePipelineFunc
	oldNewSTTSession := newLiveSTTSessionFunc
	defer func() {
		newLiveVoicePipelineFunc = oldNewVoicePipeline
		newLiveSTTSessionFunc = oldNewSTTSession
	}()

	newLiveVoicePipelineFunc = func(cartesiaKey string, httpClient *http.Client) *voice.Pipeline {
		return &voice.Pipeline{}
	}
	newLiveSTTSessionFunc = func(ctx context.Context, pipeline *voice.Pipeline, model string) (liveSTTSession, error) {
		return newFakeLiveSTTSession(), nil
	}

	h := LiveHandler{
		Config: config.Config{
			WSMaxSessionDuration:      time.Minute,
			WSMaxSessionsPerPrincipal: 2,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/live", h)
	server := httptest.NewServer(mux)
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Provider-Key-Anthropic", "sk-test")
	headers.Set("X-Provider-Key-Cartesia", "sk-cartesia")
	conn := dialLiveTestConn(t, server.URL, headers)
	defer conn.Close()

	start := map[string]any{
		"type": "start",
		"run_request": map[string]any{
			"request": map[string]any{
				"model": "anthropic/test",
				"messages": []map[string]any{{
					"role":    "user",
					"content": "hello",
				}},
				"voice": map[string]any{
					"output": map[string]any{
						"voice":  "a167e0f3-df7e-4d52-a9c3-f949145efdab",
						"format": "pcm",
					},
				},
			},
			"run": map[string]any{
				"max_turns":       1,
				"max_tool_calls":  1,
				"timeout_ms":      1000,
				"parallel_tools":  true,
				"tool_timeout_ms": 1000,
			},
		},
	}
	if err := conn.WriteJSON(start); err != nil {
		t.Fatalf("write start frame: %v", err)
	}

	event := readLiveEvent(t, conn)
	if got, _ := event["type"].(string); got != "session_started" {
		t.Fatalf("type=%v, want session_started", event["type"])
	}
	if got, _ := event["output_sample_rate_hz"].(float64); int(got) != 16000 {
		t.Fatalf("output_sample_rate_hz=%v, want 16000", event["output_sample_rate_hz"])
	}
}

func TestLiveHandler_UnsupportedOutputSampleRateRejected(t *testing.T) {
	h := LiveHandler{
		Config: config.Config{
			WSMaxSessionDuration:      time.Minute,
			WSMaxSessionsPerPrincipal: 2,
		},
		Upstreams: fakeFactory{p: &fakeProvider{}},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/live", h)
	server := httptest.NewServer(mux)
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Provider-Key-Anthropic", "sk-test")
	headers.Set("X-Provider-Key-Cartesia", "sk-cartesia")
	conn := dialLiveTestConn(t, server.URL, headers)
	defer conn.Close()

	start := map[string]any{
		"type": "start",
		"run_request": map[string]any{
			"request": map[string]any{
				"model": "anthropic/test",
				"messages": []map[string]any{{
					"role":    "user",
					"content": "hello",
				}},
				"voice": map[string]any{
					"output": map[string]any{
						"voice":       "a167e0f3-df7e-4d52-a9c3-f949145efdab",
						"format":      "pcm",
						"sample_rate": 12345,
					},
				},
			},
			"run": map[string]any{
				"max_turns":       1,
				"max_tool_calls":  1,
				"timeout_ms":      1000,
				"parallel_tools":  true,
				"tool_timeout_ms": 1000,
			},
		},
	}
	if err := conn.WriteJSON(start); err != nil {
		t.Fatalf("write start frame: %v", err)
	}

	event := readLiveEvent(t, conn)
	if got, _ := event["type"].(string); got != "error" {
		t.Fatalf("type=%v, want error", event["type"])
	}
	if got, _ := event["fatal"].(bool); !got {
		t.Fatalf("fatal=%v, want true", event["fatal"])
	}
	if got, _ := event["code"].(string); got != "run_validation_failed" {
		t.Fatalf("code=%v, want run_validation_failed", event["code"])
	}
	if msg, _ := event["message"].(string); !strings.Contains(msg, "unsupported live output sample_rate") {
		t.Fatalf("message=%q, want unsupported sample-rate hint", msg)
	}
}

func TestLiveTalkTurnState_PreservesWhitespaceTextDelta(t *testing.T) {
	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 1),
	}
	state := newLiveTalkTurnState(session)

	err := state.handleStreamEvent(types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: 0,
		Delta: types.TextDelta{Type: "text_delta", Text: " "},
	})
	if err != nil {
		t.Fatalf("handleStreamEvent error: %v", err)
	}

	select {
	case raw := <-session.sendCh:
		ev, ok := raw.(types.LiveAssistantTextDeltaEvent)
		if !ok {
			t.Fatalf("unexpected event type %T", raw)
		}
		if ev.Text != " " {
			t.Fatalf("text=%q, want single-space delta", ev.Text)
		}
	default:
		t.Fatal("expected assistant_text_delta event for whitespace token")
	}
}

func TestLiveSession_STTFailureCancelsSessionAndDoesNotSpam(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	session := &liveSession{
		ctx:        ctx,
		cancel:     cancel,
		sendCh:     make(chan any, 8),
		sttSession: failingLiveSTTSession{},
	}

	session.handleAudioChunk([]byte{0x01})
	session.handleAudioChunk([]byte{0x02})

	select {
	case raw := <-session.sendCh:
		ev, ok := raw.(types.LiveErrorEvent)
		if !ok {
			t.Fatalf("unexpected event type %T", raw)
		}
		if !ev.Fatal {
			t.Fatalf("fatal=%v, want true", ev.Fatal)
		}
		if ev.Code != "stt_unavailable" {
			t.Fatalf("code=%q, want stt_unavailable", ev.Code)
		}
	default:
		t.Fatal("expected stt_unavailable error event")
	}

	select {
	case raw := <-session.sendCh:
		t.Fatalf("unexpected extra event after first stt failure: %#v", raw)
	default:
	}
}

func TestLiveTalkTurnStateFinish_WaitsForAudioDrainBeforeClose(t *testing.T) {
	ttsDone := make(chan struct{})
	tts := &fakeLiveTTSSession{
		flushResultCh: make(chan error, 1),
		flushCalledCh: make(chan struct{}, 1),
		closeCalledCh: make(chan struct{}, 1),
	}
	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 4),
	}
	state := &liveTalkTurnState{
		session: session,
		tts:     tts,
		ttsDone: ttsDone,
	}

	finishDone := make(chan struct{})
	go func() {
		state.finish()
		close(finishDone)
	}()

	select {
	case <-tts.flushCalledCh:
	case <-time.After(time.Second):
		t.Fatal("flush was not called")
	}
	tts.flushResultCh <- nil

	select {
	case <-tts.closeCalledCh:
		t.Fatal("close called before audio drain completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(ttsDone)
	select {
	case <-finishDone:
	case <-time.After(time.Second):
		t.Fatal("finish did not return after ttsDone")
	}
	if got := tts.closeCalls.Load(); got != 1 {
		t.Fatalf("closeCalls=%d, want 1", got)
	}
}

func TestLiveTalkTurnStateFinish_FlushFailureEmitsAudioUnavailable(t *testing.T) {
	ttsDone := make(chan struct{})
	close(ttsDone)

	tts := &fakeLiveTTSSession{
		flushResultCh: make(chan error, 1),
		closeCalledCh: make(chan struct{}, 1),
	}
	tts.flushResultCh <- errors.New("flush boom")

	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 4),
	}
	state := &liveTalkTurnState{
		session: session,
		tts:     tts,
		ttsDone: ttsDone,
	}

	state.finish()

	if got := tts.closeCalls.Load(); got != 1 {
		t.Fatalf("closeCalls=%d, want 1", got)
	}
	select {
	case raw := <-session.sendCh:
		ev, ok := raw.(types.LiveAudioUnavailableEvent)
		if !ok {
			t.Fatalf("unexpected event type %T", raw)
		}
		if ev.Reason != "tts_failed" {
			t.Fatalf("reason=%q, want tts_failed", ev.Reason)
		}
		if !strings.Contains(ev.Message, "flush boom") {
			t.Fatalf("message=%q, want flush error details", ev.Message)
		}
	default:
		t.Fatal("expected audio_unavailable event on flush failure")
	}
}

func TestLiveTalkTurnStateFinish_DrainTimeoutEmitsAudioUnavailable(t *testing.T) {
	oldTimeout := liveTTSDrainTimeout
	liveTTSDrainTimeout = 30 * time.Millisecond
	defer func() { liveTTSDrainTimeout = oldTimeout }()

	tts := &fakeLiveTTSSession{
		flushResultCh: make(chan error, 1),
		closeCalledCh: make(chan struct{}, 1),
	}
	tts.flushResultCh <- nil

	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 4),
	}
	state := &liveTalkTurnState{
		session: session,
		tts:     tts,
		ttsDone: make(chan struct{}),
	}

	start := time.Now()
	state.finish()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("finish took too long after drain timeout: %s", elapsed)
	}
	if got := tts.closeCalls.Load(); got != 1 {
		t.Fatalf("closeCalls=%d, want 1", got)
	}

	select {
	case raw := <-session.sendCh:
		ev, ok := raw.(types.LiveAudioUnavailableEvent)
		if !ok {
			t.Fatalf("unexpected event type %T", raw)
		}
		if ev.Reason != "tts_failed" {
			t.Fatalf("reason=%q, want tts_failed", ev.Reason)
		}
		if !strings.Contains(ev.Message, "timed out waiting for TTS audio drain") {
			t.Fatalf("message=%q, want drain-timeout hint", ev.Message)
		}
	default:
		t.Fatal("expected audio_unavailable event on drain timeout")
	}
}

func TestLiveTalkTurnStateFinish_ProgressKeepsDrainAliveUntilDone(t *testing.T) {
	oldTimeout := liveTTSDrainTimeout
	liveTTSDrainTimeout = 30 * time.Millisecond
	defer func() { liveTTSDrainTimeout = oldTimeout }()

	ttsDone := make(chan struct{})
	tts := &fakeLiveTTSSession{
		flushResultCh: make(chan error, 1),
		closeCalledCh: make(chan struct{}, 1),
	}
	tts.flushResultCh <- nil

	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 4),
	}
	state := &liveTalkTurnState{
		session:       session,
		tts:           tts,
		ttsDone:       ttsDone,
		ttsProgressCh: make(chan struct{}, 1),
	}

	finishDone := make(chan struct{})
	go func() {
		state.finish()
		close(finishDone)
	}()

	progressStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-progressStop:
				return
			case <-ticker.C:
				state.signalTTSProgress()
			}
		}
	}()

	time.Sleep(90 * time.Millisecond)
	close(progressStop)
	close(ttsDone)

	select {
	case <-finishDone:
	case <-time.After(time.Second):
		t.Fatal("finish did not return after ttsDone")
	}

	if got := tts.closeCalls.Load(); got != 1 {
		t.Fatalf("closeCalls=%d, want 1", got)
	}
	select {
	case raw := <-session.sendCh:
		t.Fatalf("unexpected event while progress was active: %#v", raw)
	default:
	}
}

func TestLiveTalkTurnStateForwardTTSAudio_EmitsSingleFinalChunk(t *testing.T) {
	audioCh := make(chan []byte, 4)
	tts := &fakeLiveTTSSession{audioCh: audioCh}
	session := &liveSession{
		ctx:    context.Background(),
		sendCh: make(chan any, 8),
		controllerCfg: &liveSessionConfig{
			OutputSampleRateHz: 24000,
		},
	}
	state := &liveTalkTurnState{
		session: session,
		tts:     tts,
		ttsDone: make(chan struct{}),
	}

	go state.forwardTTSAudio()
	audioCh <- []byte{0x01, 0x02}
	audioCh <- []byte{0x03, 0x04}
	close(audioCh)

	select {
	case <-state.ttsDone:
	case <-time.After(time.Second):
		t.Fatal("forwardTTSAudio did not finish")
	}

	events := make([]types.LiveAudioChunkEvent, 0, 2)
	for {
		select {
		case raw := <-session.sendCh:
			ev, ok := raw.(types.LiveAudioChunkEvent)
			if !ok {
				t.Fatalf("unexpected event type %T", raw)
			}
			events = append(events, ev)
		default:
			goto done
		}
	}
done:
	if len(events) != 2 {
		t.Fatalf("len(audio events)=%d, want 2", len(events))
	}
	finalCount := 0
	for i, ev := range events {
		if ev.IsFinal {
			finalCount++
			if i != len(events)-1 {
				t.Fatalf("is_final set on non-terminal chunk at index %d", i)
			}
		}
	}
	if finalCount != 1 {
		t.Fatalf("final chunk count=%d, want 1", finalCount)
	}
}
