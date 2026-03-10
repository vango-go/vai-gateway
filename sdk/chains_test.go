package vai

import (
	"context"
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

func TestChainWebsocketURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr string
	}{
		{name: "http", baseURL: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/v1/chains/ws"},
		{name: "https path", baseURL: "https://api.example.com/proxy", want: "wss://api.example.com/proxy/v1/chains/ws"},
		{name: "unsupported", baseURL: "ftp://example.com", wantErr: "invalid gateway base URL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chainWebsocketURL(tc.baseURL)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("chainWebsocketURL error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("url=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestChainsConnectAndRunStream(t *testing.T) {
	type observed struct {
		auth      string
		openAIKey string
		start     types.ChainStartFrame
		run       types.RunStartFrame
	}
	obsCh := make(chan observed, 1)

	server := newChainTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.ChainStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read chain.start: %v", err)
			return
		}
		if err := conn.WriteJSON(types.ChainStartedEvent{
			Type:         "chain.started",
			EventID:      1,
			ChainVersion: 1,
			ChainID:      "chain_1",
			SessionID:    "sess_1",
			ResumeToken:  "chain_rt_1",
			Defaults:     start.Defaults,
		}); err != nil {
			t.Errorf("write chain.started: %v", err)
			return
		}

		var run types.RunStartFrame
		if err := conn.ReadJSON(&run); err != nil {
			t.Errorf("read run.start: %v", err)
			return
		}
		obsCh <- observed{
			auth:      r.Header.Get("Authorization"),
			openAIKey: r.Header.Get("X-Provider-Key-OpenAI"),
			start:     start,
			run:       run,
		}

		if err := conn.WriteJSON(types.RunEnvelopeEvent{
			Type:         "run.event",
			EventID:      2,
			ChainVersion: 1,
			RunID:        "run_1",
			ChainID:      "chain_1",
			Event: types.RunStreamEventWrapper{
				Type: "stream_event",
				Event: types.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: 0,
					Delta: types.TextDelta{Type: "text_delta", Text: "ok"},
				},
			},
		}); err != nil {
			t.Errorf("write stream event: %v", err)
			return
		}
		if err := conn.WriteJSON(types.RunEnvelopeEvent{
			Type:         "run.event",
			EventID:      3,
			ChainVersion: 1,
			RunID:        "run_1",
			ChainID:      "chain_1",
			Event: types.RunCompleteEvent{
				Type: "run_complete",
				Result: &types.RunResult{
					Response: &types.MessageResponse{
						Type:       "message",
						Role:       "assistant",
						Model:      "openai/gpt-5",
						Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
						StopReason: types.StopReasonEndTurn,
					},
					StopReason: types.RunStopReasonEndTurn,
				},
			},
		}); err != nil {
			t.Errorf("write run complete: %v", err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL),
		WithGatewayAPIKey("vai_sk_test"),
		WithProviderKey("openai", "sk-openai"),
	)

	chain, err := client.Chains.Connect(context.Background(), &ChainRequest{
		Model:    "openai/gpt-5",
		System:   "be concise",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Metadata: map[string]any{
			"observability": map[string]any{"source": "platform_demo_chat"},
		},
		ProviderMetadata: map[string]any{
			"trace_id": "trace_chain_connect",
		},
	})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer chain.Close()

	stream, err := chain.RunStream(context.Background(), &ChainRunRequest{
		Input: ContentBlocks(Text("say hi")),
		Metadata: map[string]any{
			"observability": map[string]any{"selected_transport": "websocket"},
		},
		ProviderMetadata: map[string]any{
			"trace_id": "trace_chain_run",
		},
	})
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	text, err := stream.Process(StreamCallbacks{})
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if strings.TrimSpace(text) != "ok" {
		t.Fatalf("text=%q", text)
	}
	if chain.ID() != "chain_1" {
		t.Fatalf("chain id=%q", chain.ID())
	}

	select {
	case obs := <-obsCh:
		if obs.auth != "Bearer vai_sk_test" {
			t.Fatalf("authorization=%q", obs.auth)
		}
		if obs.openAIKey != "sk-openai" {
			t.Fatalf("openai header=%q", obs.openAIKey)
		}
		if obs.start.Defaults.Model != "openai/gpt-5" {
			t.Fatalf("start model=%q", obs.start.Defaults.Model)
		}
		if _, ok := obs.start.Metadata["observability"].(map[string]any); !ok {
			t.Fatalf("start metadata=%#v, want observability object in top-level metadata", obs.start.Metadata)
		}
		if got, _ := obs.start.Defaults.Metadata["trace_id"].(string); got != "trace_chain_connect" {
			t.Fatalf("start provider metadata=%#v", obs.start.Defaults.Metadata)
		}
		if _, exists := obs.start.Defaults.Metadata["observability"]; exists {
			t.Fatalf("provider metadata leaked internal observability object: %#v", obs.start.Defaults.Metadata)
		}
		if len(obs.run.Input) != 1 {
			t.Fatalf("len(run input)=%d, want 1", len(obs.run.Input))
		}
		if _, ok := obs.run.Metadata["observability"].(map[string]any); !ok {
			t.Fatalf("run metadata=%#v, want observability object in top-level metadata", obs.run.Metadata)
		}
		if obs.run.Overrides == nil {
			t.Fatal("expected run overrides")
		}
		if got, _ := obs.run.Overrides.Metadata["trace_id"].(string); got != "trace_chain_run" {
			t.Fatalf("run provider metadata=%#v", obs.run.Overrides.Metadata)
		}
		if _, exists := obs.run.Overrides.Metadata["observability"]; exists {
			t.Fatalf("run provider metadata leaked internal observability object: %#v", obs.run.Overrides.Metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for observed frames")
	}
}

func TestChainsFork_UsesHTTPEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotIDKey  string
		payload   types.ChainForkRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotIDKey = r.Header.Get(chainSDKIdempotencyHeader)
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ChainForkResponse{
			ChainID:           "chain_forked_1",
			SessionID:         "sess_1",
			ExternalSessionID: "chat_1",
			ParentChainID:     "chain_source_1",
			Defaults:          types.ChainDefaults{Model: "openai/gpt-5"},
			ResumeToken:       "chain_rt_fork_1",
		})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	resp, err := client.Chains.Fork(context.Background(), "chain_source_1", &types.ChainForkRequest{
		History: []types.Message{{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "edited"}}}},
	})
	if err != nil {
		t.Fatalf("Fork error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method=%q", gotMethod)
	}
	if gotPath != "/v1/chains/chain_source_1:fork" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotIDKey == "" {
		t.Fatal("expected idempotency header")
	}
	if len(payload.History) != 1 {
		t.Fatalf("len(history)=%d", len(payload.History))
	}
	if resp.ChainID != "chain_forked_1" {
		t.Fatalf("chain_id=%q", resp.ChainID)
	}
}

func TestChainsRegenerate_UsesHTTPEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotIDKey  string
		payload   types.RunRegenerateRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotIDKey = r.Header.Get(chainSDKIdempotencyHeader)
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ChainForkResponse{
			ChainID:         "chain_regen_1",
			ParentChainID:   "chain_source_1",
			ForkedFromRunID: "run_source_1",
			Input: []types.Message{
				{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "say hi"}}},
			},
		})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	resp, err := client.Chains.Regenerate(context.Background(), "run_source_1", &types.RunRegenerateRequest{
		Defaults: types.ChainDefaults{Model: "openai/gpt-5"},
	})
	if err != nil {
		t.Fatalf("Regenerate error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method=%q", gotMethod)
	}
	if gotPath != "/v1/runs/run_source_1:regenerate" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotIDKey == "" {
		t.Fatal("expected idempotency header")
	}
	if payload.Defaults.Model != "openai/gpt-5" {
		t.Fatalf("model=%q", payload.Defaults.Model)
	}
	if resp.ForkedFromRunID != "run_source_1" {
		t.Fatalf("forked_from_run_id=%q", resp.ForkedFromRunID)
	}
	if len(resp.Input) != 1 {
		t.Fatalf("len(input)=%d", len(resp.Input))
	}
}

func TestChainsRunStream_SSEDoesNotLeakGatewayMetadataIntoProviderMetadata(t *testing.T) {
	t.Parallel()

	var (
		gotCreate types.ChainStartPayload
		gotRun    types.RunStartPayload
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chains":
			if r.Method != http.MethodPost {
				t.Fatalf("create method=%q", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&gotCreate); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(types.ChainStartedEvent{
				Type:         "chain.started",
				EventID:      1,
				ChainVersion: 1,
				ChainID:      "chain_sse_1",
				SessionID:    "sess_sse_1",
				ResumeToken:  "chain_rt_sse_1",
				Defaults:     gotCreate.Defaults,
			})
		case "/v1/chains/chain_sse_1/runs:stream":
			if r.Method != http.MethodPost {
				t.Fatalf("run method=%q", r.Method)
			}
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Fatalf("Accept=%q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&gotRun); err != nil {
				t.Fatalf("decode run payload: %v", err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSEJSON(t, w, "run.event", types.RunEnvelopeEvent{
				Type:         "run.event",
				EventID:      2,
				ChainVersion: 1,
				RunID:        "run_sse_1",
				ChainID:      "chain_sse_1",
				Event: types.RunCompleteEvent{
					Type: "run_complete",
					Result: &types.RunResult{
						Response: &types.MessageResponse{
							Type:       "message",
							Role:       "assistant",
							Model:      "oai-resp/gpt-5-mini",
							Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
							StopReason: types.StopReasonEndTurn,
						},
						StopReason: types.RunStopReasonEndTurn,
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	chain, err := client.Chains.Connect(context.Background(), &ChainRequest{
		Model: "oai-resp/gpt-5-mini",
		Metadata: map[string]any{
			"observability": map[string]any{"request_origin": "platform_demo_chat"},
		},
		ProviderMetadata: map[string]any{
			"trace_id": "chain_trace",
		},
		Transport: TransportSSE,
	})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}

	stream, err := chain.RunStream(context.Background(), &ChainRunRequest{
		Input: ContentBlocks(Text("hello")),
		Model: "oai-resp/gpt-5-mini",
		Metadata: map[string]any{
			"observability": map[string]any{"selected_transport": "sse"},
		},
		ProviderMetadata: map[string]any{
			"trace_id": "run_trace",
		},
	})
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}
	if _, err := stream.Process(StreamCallbacks{}); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	if _, ok := gotCreate.Metadata["observability"].(map[string]any); !ok {
		t.Fatalf("create metadata=%#v, want observability object", gotCreate.Metadata)
	}
	if got, _ := gotCreate.Defaults.Metadata["trace_id"].(string); got != "chain_trace" {
		t.Fatalf("create provider metadata=%#v", gotCreate.Defaults.Metadata)
	}
	if _, exists := gotCreate.Defaults.Metadata["observability"]; exists {
		t.Fatalf("create provider metadata leaked internal metadata: %#v", gotCreate.Defaults.Metadata)
	}
	if _, ok := gotRun.Metadata["observability"].(map[string]any); !ok {
		t.Fatalf("run metadata=%#v, want observability object", gotRun.Metadata)
	}
	if gotRun.Overrides == nil {
		t.Fatal("expected run overrides")
	}
	if got, _ := gotRun.Overrides.Metadata["trace_id"].(string); got != "run_trace" {
		t.Fatalf("run provider metadata=%#v", gotRun.Overrides.Metadata)
	}
	if _, exists := gotRun.Overrides.Metadata["observability"]; exists {
		t.Fatalf("run provider metadata leaked internal metadata: %#v", gotRun.Overrides.Metadata)
	}
}

func TestChainsRunStream_AutoReconnectsWithResumeToken(t *testing.T) {
	var (
		mu            sync.Mutex
		connCount     int
		attachSeen    bool
		attachedAfter int64
	)

	server := newChainTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		mu.Lock()
		connCount++
		currentConn := connCount
		mu.Unlock()

		switch currentConn {
		case 1:
			var start types.ChainStartFrame
			if err := conn.ReadJSON(&start); err != nil {
				t.Errorf("read chain.start: %v", err)
				return
			}
			if err := conn.WriteJSON(types.ChainStartedEvent{
				Type:         "chain.started",
				EventID:      1,
				ChainVersion: 1,
				ChainID:      "chain_reconnect_1",
				SessionID:    "sess_reconnect_1",
				ResumeToken:  "chain_rt_reconnect_1",
				Defaults:     start.Defaults,
			}); err != nil {
				t.Errorf("write chain.started: %v", err)
				return
			}

			var run types.RunStartFrame
			if err := conn.ReadJSON(&run); err != nil {
				t.Errorf("read run.start: %v", err)
				return
			}
			if err := conn.WriteJSON(types.RunEnvelopeEvent{
				Type:         "run.event",
				EventID:      2,
				ChainVersion: 1,
				RunID:        "run_reconnect_1",
				ChainID:      "chain_reconnect_1",
				Event: types.RunStreamEventWrapper{
					Type: "stream_event",
					Event: types.ContentBlockDeltaEvent{
						Type:  "content_block_delta",
						Index: 0,
						Delta: types.TextDelta{Type: "text_delta", Text: "o"},
					},
				},
			}); err != nil {
				t.Errorf("write reconnect delta: %v", err)
				return
			}
			_ = conn.Close()
		case 2:
			var attach types.ChainAttachFrame
			if err := conn.ReadJSON(&attach); err != nil {
				t.Errorf("read chain.attach: %v", err)
				return
			}
			mu.Lock()
			attachSeen = true
			attachedAfter = attach.AfterEventID
			mu.Unlock()
			if attach.ChainID != "chain_reconnect_1" {
				t.Errorf("attach chain_id=%q", attach.ChainID)
				return
			}
			if attach.ResumeToken != "chain_rt_reconnect_1" {
				t.Errorf("attach resume_token=%q", attach.ResumeToken)
				return
			}
			if !attach.Takeover {
				t.Errorf("expected takeover=true on reconnect")
				return
			}
			if err := conn.WriteJSON(types.ChainAttachedEvent{
				Type:         "chain.attached",
				EventID:      3,
				ChainVersion: 1,
				ChainID:      "chain_reconnect_1",
				SessionID:    "sess_reconnect_1",
				ResumeToken:  "chain_rt_reconnect_2",
				ReplayStatus: types.ReplayStatusNone,
			}); err != nil {
				t.Errorf("write chain.attached: %v", err)
				return
			}
			if err := conn.WriteJSON(types.RunEnvelopeEvent{
				Type:         "run.event",
				EventID:      4,
				ChainVersion: 1,
				RunID:        "run_reconnect_1",
				ChainID:      "chain_reconnect_1",
				Event: types.RunStreamEventWrapper{
					Type: "stream_event",
					Event: types.ContentBlockDeltaEvent{
						Type:  "content_block_delta",
						Index: 0,
						Delta: types.TextDelta{Type: "text_delta", Text: "k"},
					},
				},
			}); err != nil {
				t.Errorf("write post-reconnect delta: %v", err)
				return
			}
			if err := conn.WriteJSON(types.RunEnvelopeEvent{
				Type:         "run.event",
				EventID:      5,
				ChainVersion: 1,
				RunID:        "run_reconnect_1",
				ChainID:      "chain_reconnect_1",
				Event: types.RunCompleteEvent{
					Type: "run_complete",
					Result: &types.RunResult{
						Response: &types.MessageResponse{
							Type:       "message",
							Role:       "assistant",
							Model:      "openai/gpt-5",
							Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
							StopReason: types.StopReasonEndTurn,
						},
						StopReason: types.RunStopReasonEndTurn,
					},
				},
			}); err != nil {
				t.Errorf("write reconnect run_complete: %v", err)
				return
			}
			_, _, _ = conn.ReadMessage()
		default:
			t.Errorf("unexpected connection count %d", currentConn)
		}
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	chain, err := client.Chains.Connect(context.Background(), &ChainRequest{Model: "openai/gpt-5"})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer chain.Close()

	stream, err := chain.RunStream(context.Background(), &ChainRunRequest{
		Input: ContentBlocks(Text("resume me")),
	})
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}
	text, err := stream.Process(StreamCallbacks{})
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text=%q, want ok", text)
	}
	if chain.ResumeToken() != "chain_rt_reconnect_2" {
		t.Fatalf("resume token=%q, want rotated token", chain.ResumeToken())
	}

	mu.Lock()
	defer mu.Unlock()
	if !attachSeen {
		t.Fatal("expected reconnect attach to occur")
	}
	if attachedAfter != 2 {
		t.Fatalf("attach after_event_id=%d, want 2", attachedAfter)
	}
}

func TestChainsRunStreamCancel_SendsRunCancelFrame(t *testing.T) {
	cancelSeen := make(chan string, 1)
	server := newChainTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.ChainStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read chain.start: %v", err)
			return
		}
		if err := conn.WriteJSON(types.ChainStartedEvent{
			Type:         "chain.started",
			EventID:      1,
			ChainVersion: 1,
			ChainID:      "chain_cancel_1",
			SessionID:    "sess_cancel_1",
			ResumeToken:  "chain_rt_cancel_1",
			Defaults:     start.Defaults,
		}); err != nil {
			t.Errorf("write chain.started: %v", err)
			return
		}

		var run types.RunStartFrame
		if err := conn.ReadJSON(&run); err != nil {
			t.Errorf("read run.start: %v", err)
			return
		}
		if err := conn.WriteJSON(types.RunEnvelopeEvent{
			Type:         "run.event",
			EventID:      2,
			ChainVersion: 1,
			RunID:        "run_cancel_1",
			ChainID:      "chain_cancel_1",
			Event: types.RunStartEvent{
				Type:            "run_start",
				RequestID:       "run_cancel_1",
				Model:           "openai/gpt-5",
				ProtocolVersion: "1",
			},
		}); err != nil {
			t.Errorf("write run start: %v", err)
			return
		}

		var cancel types.RunCancelFrame
		if err := conn.ReadJSON(&cancel); err != nil {
			t.Errorf("read run.cancel: %v", err)
			return
		}
		cancelSeen <- cancel.RunID
		if err := conn.WriteJSON(types.RunEnvelopeEvent{
			Type:         "run.event",
			EventID:      3,
			ChainVersion: 1,
			RunID:        "run_cancel_1",
			ChainID:      "chain_cancel_1",
			Event: types.RunCompleteEvent{
				Type: "run_complete",
				Result: &types.RunResult{
					StopReason: types.RunStopReasonCancelled,
				},
			},
		}); err != nil {
			t.Errorf("write run complete: %v", err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	chain, err := client.Chains.Connect(context.Background(), &ChainRequest{Model: "openai/gpt-5"})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer chain.Close()

	stream, err := chain.RunStream(context.Background(), &ChainRunRequest{
		Input: ContentBlocks(Text("cancel me")),
	})
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}
	if err := stream.Cancel(); err != nil {
		t.Fatalf("Cancel error: %v", err)
	}
	if result := stream.Result(); result == nil || result.StopReason != types.RunStopReasonCancelled {
		t.Fatalf("cancel result=%+v, want stop_reason=cancelled", result)
	}

	select {
	case runID := <-cancelSeen:
		if runID != "run_cancel_1" {
			t.Fatalf("cancel run id=%q", runID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for run.cancel frame")
	}
}

func TestChainsRunStream_AutoExecutesClientToolCalls(t *testing.T) {
	type toolResult struct {
		Type        string            `json:"type"`
		ExecutionID string            `json:"execution_id"`
		Content     []json.RawMessage `json:"content"`
		IsError     bool              `json:"is_error"`
		Raw         string            `json:"-"`
	}
	toolResultCh := make(chan toolResult, 1)

	server := newChainTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		var start types.ChainStartFrame
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read chain.start: %v", err)
			return
		}
		if err := conn.WriteJSON(types.ChainStartedEvent{
			Type:         "chain.started",
			EventID:      1,
			ChainVersion: 1,
			ChainID:      "chain_2",
			SessionID:    "sess_2",
			ResumeToken:  "chain_rt_2",
			Defaults:     start.Defaults,
		}); err != nil {
			t.Errorf("write chain.started: %v", err)
			return
		}

		var run types.RunStartFrame
		if err := conn.ReadJSON(&run); err != nil {
			t.Errorf("read run.start: %v", err)
			return
		}
		if err := conn.WriteJSON(types.ClientToolCallEvent{
			Type:         "client_tool.call",
			EventID:      2,
			ChainVersion: 1,
			RunID:        "run_tool_1",
			ChainID:      "chain_2",
			ExecutionID:  "exec_1",
			Name:         "lookup",
			Input:        map[string]any{"city": "Denver"},
			DeadlineAt:   time.Now().Add(time.Second),
		}); err != nil {
			t.Errorf("write client_tool.call: %v", err)
			return
		}

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read client frame: %v", err)
				return
			}
			var result toolResult
			if err := json.Unmarshal(data, &result); err != nil {
				t.Errorf("decode client frame: %v body=%s", err, string(data))
				return
			}
			if result.Type == "client_tool.result" {
				result.Raw = string(data)
				toolResultCh <- result
				break
			}
		}

		if err := conn.WriteJSON(types.RunEnvelopeEvent{
			Type:         "run.event",
			EventID:      3,
			ChainVersion: 1,
			RunID:        "run_tool_1",
			ChainID:      "chain_2",
			Event: types.RunCompleteEvent{
				Type: "run_complete",
				Result: &types.RunResult{
					Response: &types.MessageResponse{
						Type:       "message",
						Role:       "assistant",
						Model:      "openai/gpt-5",
						Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
						StopReason: types.StopReasonEndTurn,
					},
					StopReason: types.RunStopReasonEndTurn,
				},
			},
		}); err != nil {
			t.Errorf("write run complete: %v", err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL))
	chain, err := client.Chains.Connect(context.Background(), &ChainRequest{
		Model: "openai/gpt-5",
	})
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer chain.Close()

	stream, err := chain.RunStream(context.Background(), &ChainRunRequest{
		Input: ContentBlocks(Text("tool test")),
	}, WithToolHandler("lookup", func(ctx context.Context, input json.RawMessage) (any, error) {
		return "72F and sunny", nil
	}))
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}
	if _, err := stream.Process(StreamCallbacks{}); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	select {
	case result := <-toolResultCh:
		if result.ExecutionID != "exec_1" {
			t.Fatalf("tool result=%+v raw=%s", result, result.Raw)
		}
		if result.IsError {
			t.Fatalf("expected successful tool result: %+v", result)
		}
		if len(result.Content) != 1 {
			t.Fatalf("len(content)=%d", len(result.Content))
		}
		var tb struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(result.Content[0], &tb); err != nil {
			t.Fatalf("decode tool result content: %v", err)
		}
		if tb.Text != "72F and sunny" {
			t.Fatalf("tool result text=%q", tb.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client tool result")
	}
}

func newChainTestServer(t *testing.T, handler func(conn *websocket.Conn, r *http.Request)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{chainSDKWSSubprotocol},
	}
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
