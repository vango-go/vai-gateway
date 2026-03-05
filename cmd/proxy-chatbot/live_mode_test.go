package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core/types"
	vai "github.com/vango-go/vai-lite/sdk"
)

func TestLiveWebsocketURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr string
	}{
		{
			name:    "http converts to ws",
			baseURL: "http://127.0.0.1:8080",
			want:    "ws://127.0.0.1:8080/v1/live",
		},
		{
			name:    "https converts to wss",
			baseURL: "https://api.example.com/proxy",
			want:    "wss://api.example.com/proxy/v1/live",
		},
		{
			name:    "unsupported scheme",
			baseURL: "ftp://example.com",
			wantErr: "unsupported base-url scheme",
		},
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

func TestBuildLiveWSHeaders_IncludesGatewayAndProviderKeys(t *testing.T) {
	cfg := chatConfig{
		GatewayAPIKey: "vai_sk_test",
		ProviderKeys: map[string]string{
			"openai":   "sk-openai",
			"cartesia": "sk-cartesia",
			"tavily":   "tvly",
		},
	}

	headers := buildLiveWSHeaders(cfg)
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

func TestPartitionLiveTools(t *testing.T) {
	talkTool := vai.MakeTool("talk_to_user", "talk", func(ctx context.Context, input struct {
		Content string `json:"content"`
	}) (string, error) {
		return "delivered", nil
	})
	localTool := vai.MakeTool("local_tool", "local", func(ctx context.Context, input struct {
		Value string `json:"value"`
	}) (string, error) {
		return input.Value, nil
	})

	requestTools, handlers, serverTools := partitionLiveTools([]vai.ToolWithHandler{
		vai.VAIWebSearch(vai.Tavily),
		vai.VAIWebFetch(vai.Tavily),
		talkTool,
		localTool,
	})

	if len(serverTools) != 2 {
		t.Fatalf("len(serverTools)=%d, want 2", len(serverTools))
	}
	if len(requestTools) != 1 {
		t.Fatalf("len(requestTools)=%d, want 1", len(requestTools))
	}
	if requestTools[0].Name != "local_tool" {
		t.Fatalf("requestTools[0].Name=%q, want local_tool", requestTools[0].Name)
	}
	if _, ok := handlers["local_tool"]; !ok {
		t.Fatalf("expected local_tool handler in map")
	}
	if _, ok := handlers["talk_to_user"]; ok {
		t.Fatalf("did not expect talk_to_user handler in map")
	}
}

func TestBuildLiveStartFrame_ConfiguresRunRequest(t *testing.T) {
	localTool := vai.MakeTool("local_tool", "local", func(ctx context.Context, input struct {
		Value string `json:"value"`
	}) (string, error) {
		return input.Value, nil
	})

	start, handlers := buildLiveStartFrame(chatConfig{
		MaxTokens:    321,
		SystemPrompt: "Be concise",
		VoiceID:      "voice-id",
	}, "oai-resp/gpt-5-mini", nil, 16000, []vai.ToolWithHandler{vai.VAIWebSearch(vai.Tavily), localTool})

	if start.Type != "start" {
		t.Fatalf("type=%q, want start", start.Type)
	}
	if start.RunRequest.Request.Model != "oai-resp/gpt-5-mini" {
		t.Fatalf("model=%q", start.RunRequest.Request.Model)
	}
	if start.RunRequest.Request.MaxTokens != 321 {
		t.Fatalf("max_tokens=%d, want 321", start.RunRequest.Request.MaxTokens)
	}
	if start.RunRequest.Request.Voice == nil || start.RunRequest.Request.Voice.Output == nil {
		t.Fatal("voice output not configured")
	}
	if start.RunRequest.Request.Voice.Output.SampleRate != 16000 {
		t.Fatalf("sample_rate=%d, want %d", start.RunRequest.Request.Voice.Output.SampleRate, 16000)
	}
	if len(start.RunRequest.ServerTools) != 1 || start.RunRequest.ServerTools[0] != "vai_web_search" {
		t.Fatalf("server_tools=%v, want [vai_web_search]", start.RunRequest.ServerTools)
	}
	if _, ok := start.RunRequest.ServerToolConfig["vai_web_search"]; !ok {
		t.Fatalf("missing server_tool_config for vai_web_search")
	}
	if len(start.RunRequest.Request.Tools) != 1 || start.RunRequest.Request.Tools[0].Name != "local_tool" {
		t.Fatalf("request.tools=%v", start.RunRequest.Request.Tools)
	}
	if _, ok := handlers["local_tool"]; !ok {
		t.Fatalf("missing local_tool handler")
	}
	if !strings.Contains(start.RunRequest.Request.System.(string), "talk_to_user") {
		t.Fatalf("system prompt missing talk_to_user instruction: %v", start.RunRequest.Request.System)
	}
}

func TestToolOutputToContentBlocks(t *testing.T) {
	blocks := toolOutputToContentBlocks("hello")
	if len(blocks) != 1 {
		t.Fatalf("len(blocks)=%d, want 1", len(blocks))
	}
	if tb, ok := blocks[0].(types.TextBlock); !ok || tb.Text != "hello" {
		t.Fatalf("unexpected block: %#v", blocks[0])
	}

	blocks = toolOutputToContentBlocks(map[string]any{"a": 1})
	if len(blocks) != 1 {
		t.Fatalf("len(blocks)=%d, want 1", len(blocks))
	}
	if tb, ok := blocks[0].(types.TextBlock); !ok || !strings.Contains(tb.Text, `"a":1`) {
		t.Fatalf("unexpected json block: %#v", blocks[0])
	}
}

func TestValidateModelForLive(t *testing.T) {
	cfg := chatConfig{ProviderKeys: map[string]string{"openai": "sk-openai"}}
	if err := validateModelForLive("oai-resp/gpt-5-mini", cfg); err != nil {
		t.Fatalf("expected oai-resp with openai key to pass, got %v", err)
	}

	cfg = chatConfig{ProviderKeys: map[string]string{}}
	err := validateModelForLive("oai-resp/gpt-5-mini", cfg)
	if err == nil || !strings.Contains(err.Error(), "missing provider key") {
		t.Fatalf("expected missing provider key error, got %v", err)
	}
}

func TestMaybeCloseFinishedLiveSession(t *testing.T) {
	session := &liveModeSession{
		done: make(chan struct{}),
		history: []vai.Message{{
			Role:    "assistant",
			Content: vai.Text("hello"),
		}},
		err: errors.New("boom"),
	}
	state := &chatRuntime{}
	var errOut bytes.Buffer
	close(session.done)

	active := session
	maybeCloseFinishedLiveSession(state, &active, &errOut)
	if active != nil {
		t.Fatalf("expected active session to be cleared")
	}
	if len(state.history) != 1 || state.history[0].TextContent() != "hello" {
		t.Fatalf("history not synced: %#v", state.history)
	}
	if !strings.Contains(errOut.String(), "live session ended: boom") {
		t.Fatalf("missing closed-session error output: %q", errOut.String())
	}
}

func TestLiveModeCommandHelpers(t *testing.T) {
	if !isLiveModeOnCommand(" /live ") {
		t.Fatal("expected /live to match")
	}
	if !isLiveModeOffCommand(" /LIVE OFF ") {
		t.Fatal("expected /live off to match")
	}
}

func TestRunChatbot_LiveModeToggleCommands(t *testing.T) {
	oldStartLiveMode := startLiveModeFunc
	t.Cleanup(func() { startLiveModeFunc = oldStartLiveMode })

	startCalls := 0
	startLiveModeFunc = func(ctx context.Context, cfg chatConfig, state *chatRuntime, tools []vai.ToolWithHandler, out io.Writer, errOut io.Writer) (*liveModeSession, error) {
		startCalls++
		return &liveModeSession{done: make(chan struct{})}, nil
	}

	cfg := chatConfig{
		BaseURL:      "http://127.0.0.1:8080",
		Model:        "oai-resp/gpt-5-mini",
		MaxTokens:    128,
		Timeout:      2 * time.Second,
		VoiceEnabled: true,
		VoiceID:      "voice-id",
		ProviderKeys: map[string]string{
			"openai":   "sk-openai",
			"tavily":   "tvly",
			"cartesia": "sk-cartesia",
		},
	}

	var out bytes.Buffer
	var errOut bytes.Buffer
	input := strings.NewReader("/live\n/live off\n/quit\n")

	if err := runChatbot(context.Background(), cfg, input, &out, &errOut); err != nil {
		t.Fatalf("runChatbot error: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("startCalls=%d, want 1", startCalls)
	}
	if !strings.Contains(out.String(), "Live mode active.") {
		t.Fatalf("missing live-mode activation output: %q", out.String())
	}
	if !strings.Contains(out.String(), "Live mode stopped.") {
		t.Fatalf("missing live-mode stop output: %q", out.String())
	}
	if !strings.Contains(out.String(), "bye") {
		t.Fatalf("missing exit output: %q", out.String())
	}
}

func TestLiveOutputRateCandidates(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   []int
	}{
		{
			name:   "auto",
			policy: "auto",
			want:   []int{16000, 8000},
		},
		{
			name:   "forced 16k",
			policy: "16000",
			want:   []int{16000},
		},
		{
			name:   "forced 8k",
			policy: "8000",
			want:   []int{8000},
		},
		{
			name:   "forced 24k",
			policy: "24000",
			want:   []int{24000},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveOutputRateCandidates(tc.policy)
			if len(got) != len(tc.want) {
				t.Fatalf("len(candidates)=%d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("candidates[%d]=%d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLiveRequestedOutputSampleRate(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   int
	}{
		{
			name:   "auto picks 16k",
			policy: "auto",
			want:   16000,
		},
		{
			name:   "forced 8k",
			policy: "8000",
			want:   8000,
		},
		{
			name:   "forced 24k",
			policy: "24000",
			want:   24000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveRequestedOutputSampleRate(tc.policy)
			if got != tc.want {
				t.Fatalf("requested rate=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadLiveSessionStarted_AcceptsNegotiatedOutputRate(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteJSON(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 16000,
			SilenceCommitMS:    600,
		}); err != nil {
			t.Fatalf("write session_started: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	started, err := readLiveSessionStarted(conn)
	if err != nil {
		t.Fatalf("readLiveSessionStarted error: %v", err)
	}
	if started.OutputSampleRateHz != 16000 {
		t.Fatalf("output sample_rate_hz=%d, want 16000", started.OutputSampleRateHz)
	}
}

func TestReadLiveSessionStarted_RejectsNonPositiveOutputRate(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()
		payload, _ := json.Marshal(types.LiveSessionStartedEvent{
			Type:               "session_started",
			InputFormat:        "pcm_s16le",
			InputSampleRateHz:  16000,
			OutputFormat:       "pcm_s16le",
			OutputSampleRateHz: 0,
			SilenceCommitMS:    600,
		})
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			t.Fatalf("write session_started: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	_, err = readLiveSessionStarted(conn)
	if err == nil || !strings.Contains(err.Error(), "unsupported live output sample rate") {
		t.Fatalf("expected output sample rate error, got %v", err)
	}
}
