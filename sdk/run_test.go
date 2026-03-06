package vai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestDefaultRunConfig(t *testing.T) {
	cfg := defaultRunConfig()

	if cfg.toolHandlers == nil {
		t.Error("toolHandlers should be initialized")
	}
	if !cfg.parallelTools {
		t.Error("parallelTools should be true by default")
	}
	if cfg.toolTimeout != 30*time.Second {
		t.Errorf("toolTimeout = %v, want 30s", cfg.toolTimeout)
	}
}

func TestRunOptions(t *testing.T) {
	cfg := defaultRunConfig()

	// Apply options
	opts := []RunOption{
		WithMaxToolCalls(5),
		WithMaxTurns(3),
		WithMaxTokensRun(1000),
		WithRunTimeout(10 * time.Second),
		WithParallelTools(false),
		WithToolTimeout(5 * time.Second),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.maxToolCalls != 5 {
		t.Errorf("maxToolCalls = %d, want 5", cfg.maxToolCalls)
	}
	if cfg.maxTurns != 3 {
		t.Errorf("maxTurns = %d, want 3", cfg.maxTurns)
	}
	if cfg.maxTokens != 1000 {
		t.Errorf("maxTokens = %d, want 1000", cfg.maxTokens)
	}
	if cfg.timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", cfg.timeout)
	}
	if cfg.parallelTools {
		t.Error("parallelTools should be false")
	}
	if cfg.toolTimeout != 5*time.Second {
		t.Errorf("toolTimeout = %v, want 5s", cfg.toolTimeout)
	}
}

func TestWithToolHandler(t *testing.T) {
	cfg := defaultRunConfig()

	handler := func(ctx context.Context, input json.RawMessage) (any, error) {
		return "test result", nil
	}

	WithToolHandler("test_tool", handler)(&cfg)

	if _, ok := cfg.toolHandlers["test_tool"]; !ok {
		t.Error("tool handler should be registered")
	}
}

func TestWithToolHandlers(t *testing.T) {
	cfg := defaultRunConfig()

	handlers := map[string]ToolHandler{
		"tool1": func(ctx context.Context, input json.RawMessage) (any, error) { return "1", nil },
		"tool2": func(ctx context.Context, input json.RawMessage) (any, error) { return "2", nil },
	}

	WithToolHandlers(handlers)(&cfg)

	if len(cfg.toolHandlers) != 2 {
		t.Errorf("len(toolHandlers) = %d, want 2", len(cfg.toolHandlers))
	}
}

func TestWithToolSet(t *testing.T) {
	type Input struct {
		Query string `json:"query"`
	}

	ts := NewToolSet()
	tool, handler := FuncAsTool("search", "Search", func(ctx context.Context, input Input) (any, error) {
		return "result for " + input.Query, nil
	})
	ts.Add(tool, handler)

	cfg := defaultRunConfig()
	WithToolSet(ts)(&cfg)

	if _, ok := cfg.toolHandlers["search"]; !ok {
		t.Error("tool handler from ToolSet should be registered")
	}
	if len(cfg.extraTools) != 1 {
		t.Fatalf("len(extraTools) = %d, want 1", len(cfg.extraTools))
	}
	if cfg.extraTools[0].Name != "search" {
		t.Fatalf("extraTools[0].Name = %q, want %q", cfg.extraTools[0].Name, "search")
	}
}

func TestWithTools_MakeTool(t *testing.T) {
	type Input struct {
		Query string `json:"query"`
	}

	tool := MakeTool("search", "Search", func(ctx context.Context, input Input) (string, error) {
		return "result for " + input.Query, nil
	})

	cfg := defaultRunConfig()
	WithTools(tool)(&cfg)

	if _, ok := cfg.toolHandlers["search"]; !ok {
		t.Error("tool handler from MakeTool should be registered via WithTools")
	}
	if len(cfg.extraTools) != 1 {
		t.Fatalf("len(extraTools) = %d, want 1", len(cfg.extraTools))
	}
	if cfg.extraTools[0].Name != "search" {
		t.Fatalf("extraTools[0].Name = %q, want %q", cfg.extraTools[0].Name, "search")
	}

	// Test that handler works
	handler := cfg.toolHandlers["search"]
	result, err := handler(context.Background(), []byte(`{"query": "test"}`))
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	if result != "result for test" {
		t.Errorf("Result = %v, want %q", result, "result for test")
	}
}

func TestExecuteToolCall_InjectsClientIntoContext(t *testing.T) {
	svc := &MessagesService{
		client: &Client{baseURL: "http://gateway.test"},
	}

	cfg := defaultRunConfig()
	cfg.toolHandlers["ctx_tool"] = func(ctx context.Context, input json.RawMessage) (any, error) {
		if got := toolExecutionClientFromContext(ctx); got != svc.client {
			t.Fatalf("toolExecutionClientFromContext(ctx) = %p, want %p", got, svc.client)
		}
		return "ok", nil
	}

	result := svc.executeToolCall(context.Background(), types.ToolUseBlock{
		Type:  "tool_use",
		ID:    "call_1",
		Name:  "ctx_tool",
		Input: map[string]any{"x": 1},
	}, &cfg)

	if result.Error != nil {
		t.Fatalf("executeToolCall() error = %v", result.Error)
	}
	if len(result.Content) != 1 {
		t.Fatalf("len(result.Content) = %d, want 1", len(result.Content))
	}
	tb, ok := result.Content[0].(types.TextBlock)
	if !ok {
		t.Fatalf("result.Content[0] type = %T, want types.TextBlock", result.Content[0])
	}
	if tb.Text != "ok" {
		t.Fatalf("result text = %q, want %q", tb.Text, "ok")
	}
}

func TestExecuteToolCall_MixesGatewayAndLocalTools(t *testing.T) {
	var gatewayCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls++
		if r.URL.Path != "/v1/server-tools:execute" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Provider-Key-Tavily"); got != "tvly-test" {
			t.Fatalf("X-Provider-Key-Tavily=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "gateway-result"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL),
		WithProviderKey("tavily", "tvly-test"),
		WithHTTPClient(server.Client()),
	)
	svc := &MessagesService{client: client}

	localTool := MakeTool("local_echo", "Echo", func(ctx context.Context, input struct {
		Text string `json:"text"`
	}) (string, error) {
		return "local:" + input.Text, nil
	})

	cfg := defaultRunConfig()
	WithTools(
		VAIWebSearch(Tavily),
		localTool,
	)(&cfg)

	gwResult := svc.executeToolCall(context.Background(), types.ToolUseBlock{
		Type:  "tool_use",
		ID:    "call_gw",
		Name:  "vai_web_search",
		Input: map[string]any{"query": "latest"},
	}, &cfg)
	if gwResult.Error != nil {
		t.Fatalf("gateway tool error: %v", gwResult.Error)
	}
	if gatewayCalls != 1 {
		t.Fatalf("gateway calls=%d, want 1", gatewayCalls)
	}
	gwText, ok := gwResult.Content[0].(types.TextBlock)
	if !ok || gwText.Text != "gateway-result" {
		t.Fatalf("gateway content=%#v", gwResult.Content)
	}

	localResult := svc.executeToolCall(context.Background(), types.ToolUseBlock{
		Type:  "tool_use",
		ID:    "call_local",
		Name:  "local_echo",
		Input: map[string]any{"text": "hi"},
	}, &cfg)
	if localResult.Error != nil {
		t.Fatalf("local tool error: %v", localResult.Error)
	}
	localText, ok := localResult.Content[0].(types.TextBlock)
	if !ok || localText.Text != "local:hi" {
		t.Fatalf("local content=%#v", localResult.Content)
	}
}

func TestWithStopWhen(t *testing.T) {
	cfg := defaultRunConfig()

	stopFn := func(r *Response) bool {
		return r.TextContent() == "DONE"
	}

	WithStopWhen(stopFn)(&cfg)

	if cfg.stopWhen == nil {
		t.Error("stopWhen should be set")
	}
}

func TestWithHooks(t *testing.T) {
	cfg := defaultRunConfig()

	var beforeCalled, afterCalled, onToolCalled, onStopCalled bool

	WithBeforeCall(func(req *MessageRequest) { beforeCalled = true })(&cfg)
	WithAfterResponse(func(resp *Response) { afterCalled = true })(&cfg)
	WithOnToolCall(func(name string, input map[string]any, output any, err error) { onToolCalled = true })(&cfg)
	WithOnStop(func(result *RunResult) { onStopCalled = true })(&cfg)

	// Test beforeCall hook
	cfg.beforeCall(&types.MessageRequest{})
	if !beforeCalled {
		t.Error("beforeCall hook should have been called")
	}

	// Test afterResponse hook
	cfg.afterResponse(&Response{})
	if !afterCalled {
		t.Error("afterResponse hook should have been called")
	}

	// Test onToolCall hook
	cfg.onToolCall("test", nil, nil, nil)
	if !onToolCalled {
		t.Error("onToolCall hook should have been called")
	}

	// Test onStop hook
	cfg.onStop(&RunResult{})
	if !onStopCalled {
		t.Error("onStop hook should have been called")
	}
}

func TestWithBuildTurnMessages(t *testing.T) {
	cfg := defaultRunConfig()
	if cfg.buildTurnMsgs != nil {
		t.Fatalf("buildTurnMsgs should be nil by default")
	}
	WithBuildTurnMessages(func(info TurnInfo) []types.Message {
		return info.History
	})(&cfg)
	if cfg.buildTurnMsgs == nil {
		t.Fatalf("buildTurnMsgs should be set")
	}
}

func TestMergeTools_DedupFunctionByName(t *testing.T) {
	reqTools := []types.Tool{
		{Type: types.ToolTypeFunction, Name: "search"},
	}
	extra := []types.Tool{
		{Type: types.ToolTypeFunction, Name: "search"},
		{Type: types.ToolTypeFunction, Name: "other"},
	}

	merged := mergeTools(reqTools, extra)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	if merged[0].Name != "search" || merged[1].Name != "other" {
		t.Fatalf("merged names = %q,%q", merged[0].Name, merged[1].Name)
	}
}

func TestOutputToContentBlocks(t *testing.T) {
	tests := []struct {
		name     string
		output   any
		expected string // Expected text in first block
	}{
		{
			name:     "string output",
			output:   "hello world",
			expected: "hello world",
		},
		{
			name:     "struct output",
			output:   struct{ Name string }{Name: "test"},
			expected: `{"Name":"test"}`,
		},
		{
			name:     "map output",
			output:   map[string]int{"count": 42},
			expected: `{"count":42}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := outputToContentBlocks(tt.output)
			if len(blocks) == 0 {
				t.Fatal("expected at least one content block")
			}
			if tb, ok := blocks[0].(types.TextBlock); ok {
				if tb.Text != tt.expected {
					t.Errorf("Text = %q, want %q", tb.Text, tt.expected)
				}
			} else {
				t.Errorf("expected TextBlock, got %T", blocks[0])
			}
		})
	}
}

func TestOutputToContentBlocks_ContentBlock(t *testing.T) {
	// Test single ContentBlock
	tb := types.TextBlock{Type: "text", Text: "test"}
	blocks := outputToContentBlocks(tb)
	if len(blocks) != 1 {
		t.Errorf("len(blocks) = %d, want 1", len(blocks))
	}

	// Test slice of ContentBlocks
	slice := []types.ContentBlock{
		types.TextBlock{Type: "text", Text: "one"},
		types.TextBlock{Type: "text", Text: "two"},
	}
	blocks = outputToContentBlocks(slice)
	if len(blocks) != 2 {
		t.Errorf("len(blocks) = %d, want 2", len(blocks))
	}
}

func TestRunStopReason_Constants(t *testing.T) {
	// Verify constants are defined correctly
	reasons := []RunStopReason{
		RunStopEndTurn,
		RunStopMaxToolCalls,
		RunStopMaxTurns,
		RunStopMaxTokens,
		RunStopTimeout,
		RunStopCancelled,
		RunStopCustom,
		RunStopError,
	}

	for _, r := range reasons {
		if r == "" {
			t.Error("RunStopReason should not be empty")
		}
	}
}

func TestRunResult_Structure(t *testing.T) {
	result := RunResult{
		Steps:         make([]RunStep, 0),
		ToolCallCount: 5,
		TurnCount:     2,
		Usage:         types.Usage{InputTokens: 100, OutputTokens: 50},
		StopReason:    RunStopEndTurn,
	}

	if result.ToolCallCount != 5 {
		t.Errorf("ToolCallCount = %d, want 5", result.ToolCallCount)
	}
	if result.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", result.TurnCount)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
}

func TestRunStep_Structure(t *testing.T) {
	step := RunStep{
		Index:      0,
		DurationMs: 150,
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "search", Input: map[string]any{"query": "test"}},
		},
		ToolResults: []ToolExecutionResult{
			{ToolUseID: "call_1", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "result"}}},
		},
	}

	if step.Index != 0 {
		t.Errorf("Index = %d, want 0", step.Index)
	}
	if len(step.ToolCalls) != 1 {
		t.Errorf("len(ToolCalls) = %d, want 1", len(step.ToolCalls))
	}
	if step.ToolCalls[0].Name != "search" {
		t.Errorf("ToolCalls[0].Name = %q, want %q", step.ToolCalls[0].Name, "search")
	}
}

func TestRunStreamEvents(t *testing.T) {
	// Test event types implement the interface
	events := []RunStreamEvent{
		StepStartEvent{Index: 0},
		StreamEventWrapper{Event: types.PingEvent{}},
		AudioChunkEvent{Data: []byte("chunk"), Format: "wav"},
		AudioUnavailableEvent{Reason: "tts_failed", Message: "provider down"},
		ToolCallStartEvent{ID: "call_1", Name: "test", Input: nil},
		ToolResultEvent{ID: "call_1", Name: "test", Content: nil},
		StepCompleteEvent{Index: 0, Response: nil},
		HistoryDeltaEvent{ExpectedLen: 0, Append: nil},
		RunCompleteEvent{Result: nil},
	}

	expectedTypes := []string{
		"step_start",
		"stream_event",
		"audio_chunk",
		"audio_unavailable",
		"tool_call_start",
		"tool_result",
		"step_complete",
		"history_delta",
		"run_complete",
	}

	for i, e := range events {
		if e.runStreamEventType() != expectedTypes[i] {
			t.Errorf("Event %d type = %q, want %q", i, e.runStreamEventType(), expectedTypes[i])
		}
	}
}

func TestRunStream_EmitsTopLevelAudioUnavailableEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := newScriptedProvider("test", []types.StreamEvent{
		types.MessageStartEvent{
			Type: "message_start",
			Message: types.MessageResponse{
				Type:  "message",
				Role:  "assistant",
				Model: "test-model",
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
			Delta: types.TextDelta{Type: "text_delta", Text: "hello"},
		},
		types.AudioUnavailableEvent{
			Type:    "audio_unavailable",
			Reason:  "tts_failed",
			Message: "provider down",
		},
		types.ContentBlockStopEvent{Type: "content_block_stop", Index: 0},
		func() types.StreamEvent {
			delta := types.MessageDeltaEvent{Type: "message_delta"}
			delta.Delta.StopReason = types.StopReasonEndTurn
			return delta
		}(),
		types.MessageStopEvent{Type: "message_stop"},
	})
	svc := newMessagesServiceForRunStreamTest(provider)

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hi")}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}

	sawWrapped := false
	sawTopLevel := false
	var topReason, topMessage string
	for event := range stream.Events() {
		switch e := event.(type) {
		case StreamEventWrapper:
			if _, ok := e.Event.(types.AudioUnavailableEvent); ok {
				sawWrapped = true
			}
		case AudioUnavailableEvent:
			sawTopLevel = true
			topReason = e.Reason
			topMessage = e.Message
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("stream.Err() = %v, want nil or EOF", err)
	}
	if !sawWrapped {
		t.Fatalf("expected wrapped audio_unavailable stream event")
	}
	if !sawTopLevel {
		t.Fatalf("expected top-level AudioUnavailableEvent")
	}
	if topReason != "tts_failed" || topMessage != "provider down" {
		t.Fatalf("top-level payload=(%q,%q), want (%q,%q)", topReason, topMessage, "tts_failed", "provider down")
	}
}

func TestRunStream_Methods(t *testing.T) {
	rs := &RunStream{
		events: make(chan RunStreamEvent, 1),
		done:   make(chan struct{}),
	}

	// Test Events() returns the channel
	if rs.Events() == nil {
		t.Error("Events() should return non-nil channel")
	}

	// Set the result before closing
	rs.result = &RunResult{StopReason: RunStopEndTurn}

	// Mark as closed and close done channel
	rs.closed.Store(true)
	close(rs.done)

	// Test Result() after done
	result := rs.Result()
	if result == nil {
		t.Error("Result() should return non-nil after done")
	}

	// Test Err()
	rs.err = nil
	if rs.Err() != nil {
		t.Error("Err() should return nil when no error")
	}

	// Test Close() is idempotent (already closed)
	err := rs.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}

func TestRunStream_Close_Idempotent(t *testing.T) {
	rs := &RunStream{
		events: make(chan RunStreamEvent, 1),
		done:   make(chan struct{}),
	}

	// First close
	err := rs.Close()
	if err != nil {
		t.Errorf("First Close() returned error: %v", err)
	}

	// Second close should not panic and return nil
	err = rs.Close()
	if err != nil {
		t.Errorf("Second Close() returned error: %v", err)
	}
}

func TestRunStream_Close_MidStream_ClosesEventsAndFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := &delayedStreamProvider{
		name:   "test",
		events: singleTurnTextStream("hello world"),
		delay:  60 * time.Millisecond,
	}
	svc := newMessagesServiceForRunStreamTest(provider)

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("start")}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}

	select {
	case _, ok := <-stream.Events():
		if !ok {
			t.Fatalf("events channel closed before Close()")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first event")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stream.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Close() did not return")
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-stream.Events():
			if !ok {
				result := stream.Result()
				if result == nil {
					t.Fatalf("stream.Result() = nil after Close()")
				}
				return
			}
		case <-deadline:
			t.Fatalf("events channel did not close after Close()")
		}
	}
}

func TestRun_ContextCanceled_StopReasonCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := newMessagesServiceForMessagesTest(newScriptedProvider("test").withCreateResponses(textResponse("done")))

	result, err := svc.Run(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result == nil {
		t.Fatalf("Run() result is nil")
	}
	if result.StopReason != RunStopCancelled {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopCancelled)
	}
}

func TestRun_ContextDeadlineExceeded_StopReasonTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	svc := newMessagesServiceForMessagesTest(newScriptedProvider("test").withCreateResponses(textResponse("done")))

	result, err := svc.Run(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context.DeadlineExceeded", err)
	}
	if result == nil {
		t.Fatalf("Run() result is nil")
	}
	if result.StopReason != RunStopTimeout {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopTimeout)
	}
}

func TestRunStream_ContextCanceled_StopReasonCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := &delayedStreamProvider{
		name:   "test",
		events: singleTurnTextStream(strings.Repeat("hello ", 20)),
		delay:  40 * time.Millisecond,
	}
	svc := newMessagesServiceForRunStreamTest(provider)

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("start")}},
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	select {
	case _, ok := <-stream.Events():
		if !ok {
			t.Fatalf("events closed before cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first stream event")
	}

	cancel()

	for range stream.Events() {
	}

	if runErr := stream.Err(); !errors.Is(runErr, context.Canceled) {
		t.Fatalf("stream.Err() = %v, want context.Canceled", runErr)
	}
	result := stream.Result()
	if result == nil {
		t.Fatalf("expected non-nil result")
	}
	if result.StopReason != RunStopCancelled {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopCancelled)
	}
}

func TestRunStream_ContextDeadlineExceeded_StopReasonTimeout(t *testing.T) {
	provider := &delayedStreamProvider{
		name:   "test",
		events: singleTurnTextStream(strings.Repeat("hello ", 20)),
		delay:  100 * time.Millisecond,
	}
	svc := newMessagesServiceForRunStreamTest(provider)

	stream, err := svc.RunStream(context.Background(), &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("start")}},
	}, WithRunTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	for range stream.Events() {
	}

	if runErr := stream.Err(); !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("stream.Err() = %v, want context.DeadlineExceeded", runErr)
	}
	result := stream.Result()
	if result == nil {
		t.Fatalf("expected non-nil result")
	}
	if result.StopReason != RunStopTimeout {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopTimeout)
	}
}

func TestToolExecutionResult_Structure(t *testing.T) {
	result := ToolExecutionResult{
		ToolUseID: "call_123",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "result"},
		},
		Error:    nil,
		ErrorMsg: "",
	}

	if result.ToolUseID != "call_123" {
		t.Errorf("ToolUseID = %q, want %q", result.ToolUseID, "call_123")
	}
	if len(result.Content) != 1 {
		t.Errorf("len(Content) = %d, want 1", len(result.Content))
	}
}

// TestInterruptBehavior_Constants verifies the interrupt behavior enum values.
func TestInterruptBehavior_Constants(t *testing.T) {
	if InterruptDiscard != 0 {
		t.Errorf("InterruptDiscard = %d, want 0", InterruptDiscard)
	}
	if InterruptSavePartial != 1 {
		t.Errorf("InterruptSavePartial = %d, want 1", InterruptSavePartial)
	}
	if InterruptSaveMarked != 2 {
		t.Errorf("InterruptSaveMarked = %d, want 2", InterruptSaveMarked)
	}
}

// TestRunStream_InterruptRequest_EmptyMessage tests that empty message is detected correctly.
func TestRunStream_InterruptRequest_EmptyMessage(t *testing.T) {
	tests := []struct {
		name     string
		msg      types.Message
		isCancel bool
	}{
		{
			name:     "empty message (cancel)",
			msg:      types.Message{},
			isCancel: true,
		},
		{
			name:     "empty role but nil content (cancel)",
			msg:      types.Message{Role: "", Content: nil},
			isCancel: true,
		},
		{
			name: "has role (interrupt)",
			msg: types.Message{
				Role:    "user",
				Content: nil,
			},
			isCancel: false,
		},
		{
			name: "has content (interrupt)",
			msg: types.Message{
				Role: "",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "test"},
				},
			},
			isCancel: false,
		},
		{
			name: "full message (interrupt)",
			msg: types.Message{
				Role: "user",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "test"},
				},
			},
			isCancel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isCancel := tt.msg.Role == "" && tt.msg.Content == nil
			if isCancel != tt.isCancel {
				t.Errorf("isCancel = %v, want %v", isCancel, tt.isCancel)
			}
		})
	}
}

// TestRunStream_Interrupt_ClosedStream verifies error handling when interrupting a closed stream.
func TestRunStream_Interrupt_ClosedStream(t *testing.T) {
	rs := &RunStream{
		events:    make(chan RunStreamEvent, 1),
		done:      make(chan struct{}),
		interrupt: make(chan interruptRequest, 1),
	}

	// Close the stream
	close(rs.done)
	rs.closed.Store(true)

	// Try to interrupt
	err := rs.Interrupt(types.Message{
		Role: "user",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "test"},
		},
	}, InterruptSaveMarked)

	if err == nil {
		t.Error("Interrupt() should return error for closed stream")
	}
	if err.Error() != "stream already closed" {
		t.Errorf("Error = %q, want %q", err.Error(), "stream already closed")
	}
}

// TestRunStream_Cancel_ClosedStream verifies Cancel returns nil for already-closed stream.
func TestRunStream_Cancel_ClosedStream(t *testing.T) {
	rs := &RunStream{
		events:    make(chan RunStreamEvent, 1),
		done:      make(chan struct{}),
		interrupt: make(chan interruptRequest, 1),
	}

	// Close the stream
	close(rs.done)
	rs.closed.Store(true)

	// Cancel should return nil (not error) for already-closed stream
	err := rs.Cancel()
	if err != nil {
		t.Errorf("Cancel() returned error for closed stream: %v", err)
	}
}

// TestRunStream_InterruptWithText verifies the convenience method.
func TestRunStream_InterruptWithText_ClosedStream(t *testing.T) {
	rs := &RunStream{
		events:    make(chan RunStreamEvent, 1),
		done:      make(chan struct{}),
		interrupt: make(chan interruptRequest, 1),
	}

	// Close the stream
	close(rs.done)
	rs.closed.Store(true)

	// Try to interrupt with text
	err := rs.InterruptWithText("hello")

	if err == nil {
		t.Error("InterruptWithText() should return error for closed stream")
	}
	if err.Error() != "stream already closed" {
		t.Errorf("Error = %q, want %q", err.Error(), "stream already closed")
	}
}

// TestInterruptedEvent_Structure verifies the InterruptedEvent type.
func TestInterruptedEvent_Structure(t *testing.T) {
	event := InterruptedEvent{
		PartialText: "partial response",
		Behavior:    InterruptSaveMarked,
	}

	if event.PartialText != "partial response" {
		t.Errorf("PartialText = %q, want %q", event.PartialText, "partial response")
	}
	if event.Behavior != InterruptSaveMarked {
		t.Errorf("Behavior = %d, want %d", event.Behavior, InterruptSaveMarked)
	}
	if event.runStreamEventType() != "interrupted" {
		t.Errorf("runStreamEventType() = %q, want %q", event.runStreamEventType(), "interrupted")
	}
}
