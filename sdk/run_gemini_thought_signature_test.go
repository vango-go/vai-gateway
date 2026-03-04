package vai

import (
	"context"
	"encoding/base64"
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
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
)

func TestRun_GeminiThoughtSignaturesPreservedAcrossToolRecursion(t *testing.T) {
	serverState, srv := newGeminiThoughtSignatureServer(t)
	defer srv.Close()

	svc := newMessagesServiceForGeminiProviderTest(srv)

	tool := MakeTool("do_something", "Repeatable tool", func(ctx context.Context, input struct{}) (string, error) {
		return "ok", nil
	})

	result, err := svc.Run(context.Background(), &MessageRequest{
		Model: "gem-dev/gemini-3-flash-preview",
		Messages: []Message{
			{Role: "user", Content: Text("Keep calling do_something many times.")},
		},
		ToolChoice: ToolChoiceTool("do_something"),
		MaxTokens:  512,
	}, WithTools(tool), WithMaxToolCalls(3))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result == nil {
		t.Fatalf("Run() result is nil")
	}
	if result.StopReason != RunStopMaxToolCalls {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopMaxToolCalls)
	}
	if result.ToolCallCount != 3 {
		t.Fatalf("tool call count = %d, want 3", result.ToolCallCount)
	}
	if got := serverState.createCallCount(); got < 4 {
		t.Fatalf("create call count = %d, want at least 4", got)
	}
}

func TestRunStream_GeminiThoughtSignaturesPreservedAcrossToolRecursion(t *testing.T) {
	serverState, srv := newGeminiThoughtSignatureServer(t)
	defer srv.Close()

	svc := newMessagesServiceForGeminiProviderTest(srv)

	tool := MakeTool("do_something", "Repeatable tool", func(ctx context.Context, input struct{}) (string, error) {
		return "ok", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model: "gem-dev/gemini-3-flash-preview",
		Messages: []Message{
			{Role: "user", Content: Text("Keep calling do_something many times.")},
		},
		ToolChoice: ToolChoiceTool("do_something"),
		MaxTokens:  512,
	}, WithTools(tool), WithMaxToolCalls(3))
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	for range stream.Events() {
	}

	if runErr := stream.Err(); runErr != nil {
		t.Fatalf("stream.Err() = %v, want nil", runErr)
	}

	result := stream.Result()
	if result == nil {
		t.Fatalf("RunStream() result is nil")
	}
	if result.StopReason != RunStopMaxToolCalls {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopMaxToolCalls)
	}
	if result.ToolCallCount != 3 {
		t.Fatalf("tool call count = %d, want 3", result.ToolCallCount)
	}
	if got := serverState.streamCallCount(); got < 4 {
		t.Fatalf("stream call count = %d, want at least 4", got)
	}
}

type geminiThoughtSignatureServerState struct {
	t *testing.T

	mu          sync.Mutex
	createCalls int
	streamCalls int
}

func (s *geminiThoughtSignatureServerState) createCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCalls
}

func (s *geminiThoughtSignatureServerState) streamCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamCalls
}

func newGeminiThoughtSignatureServer(t *testing.T) (*geminiThoughtSignatureServerState, *httptest.Server) {
	t.Helper()

	state := &geminiThoughtSignatureServerState{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("failed to read generateContent request body: %v", err)
			}

			if err := validateGeminiThoughtSignatureRequest(body); err != nil {
				writeGeminiInvalidArgument(w, err)
				return
			}

			state.mu.Lock()
			state.createCalls++
			call := state.createCalls
			state.mu.Unlock()

			writeGeminiFunctionCallJSON(w, fmt.Sprintf("sig-%d", call))
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("failed to read streamGenerateContent request body: %v", err)
			}

			if err := validateGeminiThoughtSignatureRequest(body); err != nil {
				writeGeminiInvalidArgument(w, err)
				return
			}

			state.mu.Lock()
			state.streamCalls++
			call := state.streamCalls
			state.mu.Unlock()

			writeGeminiFunctionCallSSE(w, fmt.Sprintf("sig-%d", call))
		default:
			http.NotFound(w, r)
		}
	})

	return state, httptest.NewServer(mux)
}

func newMessagesServiceForGeminiProviderTest(srv *httptest.Server) *MessagesService {
	engine := core.NewEngine(nil)
	provider := gem.NewDeveloper("test-key", gem.WithBaseURL(srv.URL), gem.WithHTTPClient(srv.Client()))
	engine.RegisterProvider(newGemAdapter(provider))
	client := &Client{core: engine}
	return &MessagesService{client: client}
}

func validateGeminiThoughtSignatureRequest(body []byte) error {
	var req struct {
		Contents []struct {
			Parts []struct {
				ThoughtSignature string `json:"thoughtSignature,omitempty"`
				FunctionCall     *struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args,omitempty"`
				} `json:"functionCall,omitempty"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("unmarshal request: %w", err)
	}

	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall == nil {
				continue
			}
			if part.ThoughtSignature == "" {
				return fmt.Errorf("missing thought_signature for function call %q", part.FunctionCall.Name)
			}
			if _, ok := part.FunctionCall.Args["__thought_signature"]; ok {
				return fmt.Errorf("thought signature leaked into function args for %q", part.FunctionCall.Name)
			}
		}
	}
	return nil
}

func writeGeminiInvalidArgument(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    400,
			"message": err.Error(),
			"status":  "INVALID_ARGUMENT",
		},
	})
}

func writeGeminiFunctionCallJSON(w http.ResponseWriter, signature string) {
	encodedSig := base64.StdEncoding.EncodeToString([]byte(signature))
	resp := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{
							"functionCall": map[string]any{
								"name": "do_something",
							},
							"thoughtSignature": encodedSig,
						},
					},
				},
				"finishReason": "FUNCTION_CALL",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     1,
			"candidatesTokenCount": 1,
			"totalTokenCount":      2,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeGeminiFunctionCallSSE(w http.ResponseWriter, signature string) {
	encodedSig := base64.StdEncoding.EncodeToString([]byte(signature))
	chunk := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{
							"functionCall": map[string]any{
								"name": "do_something",
							},
							"thoughtSignature": encodedSig,
						},
					},
				},
				"finishReason": "FUNCTION_CALL",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     1,
			"candidatesTokenCount": 1,
			"totalTokenCount":      2,
		},
	}
	data, _ := json.Marshal(chunk)

	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
