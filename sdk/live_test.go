package vai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestLiveWebsocketURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr string
	}{
		{name: "http", baseURL: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/v1/live"},
		{name: "https path", baseURL: "https://api.example.com/proxy", want: "wss://api.example.com/proxy/v1/live"},
		{name: "unsupported", baseURL: "ftp://example.com", wantErr: "invalid gateway base URL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := liveWebsocketURL(tc.baseURL)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("liveWebsocketURL error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("url=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildLiveHeaders_IncludesGatewayAndProviderKeys(t *testing.T) {
	client := NewClient(
		WithBaseURL("https://api.example.com/proxy"),
		WithGatewayAPIKey("vai_sk_test"),
		WithProviderKey("openai", "sk-openai"),
		WithProviderKey("cartesia", "sk-cartesia"),
		WithProviderKey("tavily", "tvly"),
	)

	headers := client.buildLiveHeaders()
	if got := headers.Get("Authorization"); got != "Bearer vai_sk_test" {
		t.Fatalf("authorization=%q", got)
	}
	if got := headers.Get("X-Provider-Key-OpenAI"); got != "sk-openai" {
		t.Fatalf("openai header=%q", got)
	}
	if got := headers.Get("X-Provider-Key-Cartesia"); got != "sk-cartesia" {
		t.Fatalf("cartesia header=%q", got)
	}
	if got := headers.Get("X-Provider-Key-Tavily"); got != "tvly" {
		t.Fatalf("tavily header=%q", got)
	}
}

func TestLiveServiceConnect_SendsStartFrameAndStartupEvent(t *testing.T) {
	type observed struct {
		auth       string
		openAIKey  string
		vaiVersion string
		start      types.LiveStartFrame
	}
	obsCh := make(chan observed, 1)

	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		obsCh <- observed{
			auth:       r.Header.Get("Authorization"),
			openAIKey:  r.Header.Get("X-Provider-Key-OpenAI"),
			vaiVersion: r.Header.Get(vaiVersionHeader),
			start:      start,
		}
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    900,
		}); err != nil {
			t.Errorf("write session_started: %v", err)
		}
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL),
		WithGatewayAPIKey("vai_sk_test"),
		WithProviderKey("openai", "sk-openai"),
	)

	session, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer session.Close()

	select {
	case obs := <-obsCh:
		if obs.auth != "Bearer vai_sk_test" {
			t.Fatalf("authorization=%q", obs.auth)
		}
		if obs.openAIKey != "sk-openai" {
			t.Fatalf("openai header=%q", obs.openAIKey)
		}
		if obs.vaiVersion != vaiVersionValue {
			t.Fatalf("version=%q", obs.vaiVersion)
		}
		if obs.start.Type != "start" {
			t.Fatalf("start.Type=%q", obs.start.Type)
		}
		if obs.start.RunRequest.Request.Model != "openai/gpt-5" {
			t.Fatalf("model=%q", obs.start.RunRequest.Request.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for start frame")
	}

	select {
	case event := <-session.Events():
		started, ok := event.(LiveSessionStartedEvent)
		if !ok {
			t.Fatalf("event=%T, want LiveSessionStartedEvent", event)
		}
		if started.OutputSampleRateHz != 16000 {
			t.Fatalf("output sample rate=%d", started.OutputSampleRateHz)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for startup event")
	}
}

func TestLiveServiceConnect_StartupError(t *testing.T) {
	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		_ = conn.WriteJSON(types.LiveErrorEvent{
			Type:    "error",
			Fatal:   true,
			Message: "bad request",
			Code:    "invalid_request",
		})
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	_, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("expected startup error, got %v", err)
	}
}

func TestLiveSession_HistorySnapshotUpdatesFromTurnComplete(t *testing.T) {
	serverReady := make(chan struct{})
	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    900,
		}); err != nil {
			t.Errorf("write session_started: %v", err)
			return
		}
		close(serverReady)
		time.Sleep(20 * time.Millisecond)
		_ = conn.WriteJSON(types.LiveTurnCompleteEvent{
			Type:       "turn_complete",
			TurnID:     "turn_1",
			StopReason: "end_turn",
			History: []types.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			},
		})
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	session, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer session.Close()

	<-serverReady
	<-session.Events() // session_started

	select {
	case event := <-session.Events():
		complete, ok := event.(LiveTurnCompleteEvent)
		if !ok {
			t.Fatalf("event=%T, want LiveTurnCompleteEvent", event)
		}
		if complete.TurnID != "turn_1" {
			t.Fatalf("turn_id=%q", complete.TurnID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn_complete")
	}

	history := session.HistorySnapshot()
	if len(history) != 2 || history[1].TextContent() != "hi" {
		t.Fatalf("history=%#v", history)
	}
}

func TestLiveSession_SendHelpers(t *testing.T) {
	type observed struct {
		textFrames []types.LiveClientFrame
		binaries   [][]byte
		mu         sync.Mutex
	}
	obs := &observed{}
	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    900,
		}); err != nil {
			t.Errorf("write session_started: %v", err)
			return
		}
		for i := 0; i < 7; i++ {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read frame: %v", err)
				return
			}
			obs.mu.Lock()
			if mt == websocket.BinaryMessage {
				obs.binaries = append(obs.binaries, append([]byte(nil), data...))
			} else {
				frame, err := types.UnmarshalLiveClientFrame(data)
				if err != nil {
					obs.mu.Unlock()
					t.Errorf("unmarshal frame: %v", err)
					return
				}
				obs.textFrames = append(obs.textFrames, frame)
			}
			obs.mu.Unlock()
		}
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	session, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer session.Close()

	<-session.Events()
	if err := session.SendPlaybackMark("turn_1", 250); err != nil {
		t.Fatalf("SendPlaybackMark error: %v", err)
	}
	if err := session.SendPlaybackState("turn_1", LivePlaybackStateFinished); err != nil {
		t.Fatalf("SendPlaybackState error: %v", err)
	}
	if err := session.SendAudio([]byte{0x01, 0x02}); err != nil {
		t.Fatalf("SendAudio error: %v", err)
	}
	if err := session.SendToolResult("exec_1", []ContentBlock{Text("ok")}, false, nil); err != nil {
		t.Fatalf("SendToolResult error: %v", err)
	}
	if err := session.AppendInputBlocks([]ContentBlock{Text("draft")}); err != nil {
		t.Fatalf("AppendInputBlocks error: %v", err)
	}
	if err := session.CommitText("send"); err != nil {
		t.Fatalf("CommitText error: %v", err)
	}
	if err := session.ClearPendingInput(); err != nil {
		t.Fatalf("ClearPendingInput error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.binaries) != 1 || len(obs.binaries[0]) != 2 {
		t.Fatalf("binaries=%v", obs.binaries)
	}
	if len(obs.textFrames) < 6 {
		t.Fatalf("textFrames=%d, want >= 6", len(obs.textFrames))
	}
}

func TestLiveSession_ProcessCallbacks(t *testing.T) {
	session := &LiveSession{
		events:    make(chan LiveEvent, 10),
		procTools: make(chan liveProcessToolEvent, 2),
		done:      make(chan struct{}),
	}

	audioPayload := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})
	session.events <- LiveSessionStartedEvent{Type: "session_started"}
	session.events <- LiveAssistantTextDeltaEvent{Type: "assistant_text_delta", Text: "hello"}
	session.events <- LiveAudioChunkEvent{Type: "audio_chunk", Format: "pcm_s16le", Audio: audioPayload}
	session.events <- LiveAudioUnavailableEvent{Type: "audio_unavailable", Reason: "tts_failed", Message: "boom"}
	session.events <- LiveInputStateEvent{Type: "input_state", Content: []ContentBlock{Text("draft")}}
	session.events <- LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_1", AudioBytes: 42}
	session.events <- LiveTurnCompleteEvent{Type: "turn_complete", TurnID: "turn_1", StopReason: "end_turn", History: []Message{{Role: "assistant", Content: "done"}}}
	session.events <- LiveTurnCancelledEvent{Type: "turn_cancelled", TurnID: "turn_2", Reason: "cancelled"}
	session.events <- LiveAudioResetEvent{Type: "audio_reset", TurnID: "turn_3", Reason: "barge_in"}
	close(session.events)

	session.procTools <- liveProcessToolEvent{start: &ToolCallStartEvent{ID: "exec_1", Name: "local_tool", Input: map[string]any{"value": "x"}}}
	session.procTools <- liveProcessToolEvent{result: &ToolResultEvent{ID: "exec_1", Name: "local_tool", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}}}}
	close(session.procTools)
	close(session.done)

	var got struct {
		text          string
		audio         []byte
		unavailable   string
		inputState    string
		started       bool
		userTurnID    string
		turnComplete  string
		turnCancelled string
		audioReset    string
		toolStarted   string
		toolResult    string
	}

	err := session.Process(LiveCallbacks{
		StreamCallbacks: StreamCallbacks{
			OnTextDelta: func(text string) { got.text += text },
			OnAudioChunk: func(data []byte, format string) {
				got.audio = append([]byte(nil), data...)
			},
			OnAudioUnavailable: func(reason, message string) {
				got.unavailable = reason + ":" + message
			},
			OnToolCallStart: func(id, name string, input map[string]any) {
				got.toolStarted = id + ":" + name
			},
			OnToolResult: func(id, name string, content []types.ContentBlock, err error) {
				got.toolResult = id + ":" + name + ":" + content[0].(types.TextBlock).Text
			},
		},
		OnSessionStarted: func(LiveSessionStartedEvent) { got.started = true },
		OnInputState: func(content []ContentBlock) {
			if len(content) > 0 {
				got.inputState = content[0].(types.TextBlock).Text
			}
		},
		OnUserTurnCommitted: func(turnID string, audioBytes int) {
			got.userTurnID = turnID
		},
		OnTurnComplete: func(turnID string, stopReason ServerRunStopReason, history []Message) {
			got.turnComplete = turnID + ":" + string(stopReason) + ":" + history[0].TextContent()
		},
		OnTurnCancelled: func(turnID, reason string) { got.turnCancelled = turnID + ":" + reason },
		OnAudioReset:    func(turnID, reason string) { got.audioReset = turnID + ":" + reason },
	})
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}

	if !got.started {
		t.Fatal("expected OnSessionStarted")
	}
	if got.text != "hello" {
		t.Fatalf("text=%q", got.text)
	}
	if string(got.audio) != string([]byte{0x01, 0x02}) {
		t.Fatalf("audio=%v", got.audio)
	}
	if got.unavailable != "tts_failed:boom" {
		t.Fatalf("unavailable=%q", got.unavailable)
	}
	if got.inputState != "draft" {
		t.Fatalf("inputState=%q", got.inputState)
	}
	if got.userTurnID != "turn_1" {
		t.Fatalf("userTurnID=%q", got.userTurnID)
	}
	if got.turnComplete != "turn_1:end_turn:done" {
		t.Fatalf("turnComplete=%q", got.turnComplete)
	}
	if got.turnCancelled != "turn_2:cancelled" {
		t.Fatalf("turnCancelled=%q", got.turnCancelled)
	}
	if got.audioReset != "turn_3:barge_in" {
		t.Fatalf("audioReset=%q", got.audioReset)
	}
	if got.toolStarted != "exec_1:local_tool" {
		t.Fatalf("toolStarted=%q", got.toolStarted)
	}
	if got.toolResult != "exec_1:local_tool:ok" {
		t.Fatalf("toolResult=%q", got.toolResult)
	}
}

func TestLiveSession_InputHelpersRejectEmpty(t *testing.T) {
	session := &LiveSession{}

	if err := session.AppendInputBlocks(nil); err == nil {
		t.Fatal("AppendInputBlocks(nil) error=nil, want invalid request")
	}
	if err := session.CommitInputBlocks(nil); err == nil {
		t.Fatal("CommitInputBlocks(nil) error=nil, want invalid request")
	}
	if err := session.AppendText(""); err == nil {
		t.Fatal("AppendText(\"\") error=nil, want invalid request")
	}
	if err := session.CommitText("   "); err == nil {
		t.Fatal("CommitText(blank) error=nil, want invalid request")
	}
}

func TestLiveServiceConnect_AutoToolExecution(t *testing.T) {
	resultCh := make(chan types.LiveToolResultFrame, 1)
	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    900,
		}); err != nil {
			t.Errorf("write session_started: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveToolCallEvent{
			Type:        "tool_call",
			ExecutionID: "exec_1",
			Name:        "local_tool",
			Input:       map[string]any{"value": "x"},
		}); err != nil {
			t.Errorf("write tool_call: %v", err)
			return
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read tool_result: %v", err)
			return
		}
		frame, err := types.UnmarshalLiveClientFrame(payload)
		if err != nil {
			t.Errorf("unmarshal tool_result: %v", err)
			return
		}
		result, ok := frame.(types.LiveToolResultFrame)
		if !ok {
			t.Errorf("frame=%T, want LiveToolResultFrame", frame)
			return
		}
		resultCh <- result
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	session, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, &LiveConnectOptions{
		ToolHandlers: map[string]ToolHandler{
			"local_tool": func(ctx context.Context, input json.RawMessage) (any, error) {
				return "from tool", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer session.Close()

	<-session.Events()
	select {
	case result := <-resultCh:
		if result.ExecutionID != "exec_1" {
			t.Fatalf("execution_id=%q", result.ExecutionID)
		}
		if result.IsError {
			t.Fatalf("expected success result: %#v", result)
		}
		if len(result.Content) != 1 || result.Content[0].(types.TextBlock).Text != "from tool" {
			t.Fatalf("content=%#v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool_result")
	}
}

func TestLiveServiceConnect_UnknownToolAutoError(t *testing.T) {
	resultCh := make(chan types.LiveToolResultFrame, 1)
	server := newLiveTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.LiveStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read start frame: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    900,
		}); err != nil {
			t.Errorf("write session_started: %v", err)
			return
		}
		if err := conn.WriteJSON(types.LiveToolCallEvent{
			Type:        "tool_call",
			ExecutionID: "exec_1",
			Name:        "missing_tool",
			Input:       map[string]any{"value": "x"},
		}); err != nil {
			t.Errorf("write tool_call: %v", err)
			return
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read tool_result: %v", err)
			return
		}
		frame, err := types.UnmarshalLiveClientFrame(payload)
		if err != nil {
			t.Errorf("unmarshal tool_result: %v", err)
			return
		}
		resultCh <- frame.(types.LiveToolResultFrame)
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	session, err := client.Live.Connect(context.Background(), &LiveConnectRequest{
		Request: MessageRequest{
			Model:    "openai/gpt-5",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer session.Close()

	<-session.Events()
	select {
	case result := <-resultCh:
		if !result.IsError {
			t.Fatalf("expected error result: %#v", result)
		}
		if len(result.Content) != 1 || !strings.Contains(result.Content[0].(types.TextBlock).Text, "unknown tool") {
			t.Fatalf("content=%#v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool_result")
	}
}

func newLiveTestServer(t *testing.T, handler func(conn *websocket.Conn, r *http.Request)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		handler(conn, r)
	}))
	return server
}
