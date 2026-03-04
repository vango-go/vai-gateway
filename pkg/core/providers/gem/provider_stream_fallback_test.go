package gem

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestStreamMessage_AutoFallbackAndCache(t *testing.T) {
	t.Parallel()

	var (
		mu             sync.Mutex
		streamArgFlags []bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		payload := string(bodyBytes)
		hasStreamArgs := strings.Contains(payload, `"streamFunctionCallArguments":true`)

		mu.Lock()
		streamArgFlags = append(streamArgFlags, hasStreamArgs)
		mu.Unlock()

		if hasStreamArgs {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`+"\n\n")
	}))
	defer server.Close()

	provider := NewVertex("test-key", WithHTTPClient(server.Client()), WithBaseURL(server.URL+"/"))
	req := &types.MessageRequest{
		Model: "gemini-2.5-flash",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}}},
		},
		Tools: []types.Tool{
			types.NewFunctionTool("get_weather", "Get weather", &types.JSONSchema{
				Type:       "object",
				Properties: map[string]types.JSONSchema{"location": {Type: "string"}},
			}),
		},
	}

	stream, err := provider.StreamMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamMessage() error = %v", err)
	}
	events := collectEvents(t, stream)

	start, ok := findEvent[types.MessageStartEvent](events)
	if !ok {
		t.Fatalf("expected message_start event")
	}
	gemMeta, _ := start.Message.Metadata["gem"].(map[string]any)
	if gemMeta == nil {
		t.Fatalf("expected gem metadata on message_start")
	}
	rawWarnings, _ := gemMeta["warnings"].([]string)
	foundWarning := false
	for _, w := range rawWarnings {
		if strings.Contains(w, "stream_function_call_arguments disabled after Vertex INVALID_ARGUMENT") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("expected fallback warning, got %#v", rawWarnings)
	}

	mu.Lock()
	firstFlags := append([]bool(nil), streamArgFlags...)
	mu.Unlock()
	if len(firstFlags) != 2 || !firstFlags[0] || firstFlags[1] {
		t.Fatalf("unexpected request stream arg flags after first call: %#v", firstFlags)
	}
	if got := provider.streamArgsSupportForModel(req.Model); got != streamArgsSupportUnsupported {
		t.Fatalf("stream args support cache = %v, want unsupported", got)
	}

	stream2, err := provider.StreamMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("second StreamMessage() error = %v", err)
	}
	_ = collectEvents(t, stream2)

	mu.Lock()
	secondFlags := append([]bool(nil), streamArgFlags...)
	mu.Unlock()
	if len(secondFlags) != 3 || secondFlags[2] {
		t.Fatalf("expected cached unsupported model to skip streamed args, got flags %#v", secondFlags)
	}
}

func TestStreamMessage_ExplicitTrueIsStrict(t *testing.T) {
	t.Parallel()

	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)
	}))
	defer server.Close()

	provider := NewVertex("test-key", WithHTTPClient(server.Client()), WithBaseURL(server.URL+"/"))
	req := &types.MessageRequest{
		Model: "gemini-2.5-flash",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}}},
		},
		Tools: []types.Tool{
			types.NewFunctionTool("get_weather", "Get weather", &types.JSONSchema{
				Type:       "object",
				Properties: map[string]types.JSONSchema{"location": {Type: "string"}},
			}),
		},
		Extensions: map[string]any{
			"gem": map[string]any{"stream_function_call_arguments": true},
		},
	}

	_, err := provider.StreamMessage(context.Background(), req)
	if err == nil {
		t.Fatalf("expected strict stream args error")
	}
	var gemErr *Error
	if !errors.As(err, &gemErr) {
		t.Fatalf("expected *gem.Error, got %T", err)
	}
	if gemErr.Type != ErrInvalidRequest {
		t.Fatalf("gem error type = %s, want %s", gemErr.Type, ErrInvalidRequest)
	}
	if callCount != 1 {
		t.Fatalf("expected strict mode to avoid retry, callCount=%d", callCount)
	}
}
