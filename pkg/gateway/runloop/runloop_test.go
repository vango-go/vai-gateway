package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/builtins"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

type fakeExecutor struct {
	name string
}

func (f fakeExecutor) Name() string { return f.name }
func (f fakeExecutor) Definition() types.Tool {
	return types.Tool{Type: types.ToolTypeFunction, Name: f.name, Description: "d", InputSchema: &types.JSONSchema{Type: "object"}}
}
func (f fakeExecutor) Configured() bool { return true }
func (f fakeExecutor) Execute(ctx context.Context, input map[string]any) ([]types.ContentBlock, *types.Error) {
	return []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}}, nil
}

type failingExecutor struct {
	name string
}

func (f failingExecutor) Name() string { return f.name }
func (f failingExecutor) Definition() types.Tool {
	return types.Tool{Type: types.ToolTypeFunction, Name: f.name, Description: "d", InputSchema: &types.JSONSchema{Type: "object"}}
}
func (f failingExecutor) Configured() bool { return true }
func (f failingExecutor) Execute(ctx context.Context, input map[string]any) ([]types.ContentBlock, *types.Error) {
	return nil, &types.Error{Type: "api_error", Message: "tool exploded"}
}

type trackingExecutor struct {
	name  string
	delay time.Duration

	mu            sync.Mutex
	inFlight      int
	maxInFlight   int
	invocationCnt int
}

func (f *trackingExecutor) Name() string { return f.name }
func (f *trackingExecutor) Definition() types.Tool {
	return types.Tool{Type: types.ToolTypeFunction, Name: f.name, Description: "d", InputSchema: &types.JSONSchema{Type: "object"}}
}
func (f *trackingExecutor) Configured() bool { return true }
func (f *trackingExecutor) Execute(ctx context.Context, input map[string]any) ([]types.ContentBlock, *types.Error) {
	f.mu.Lock()
	f.invocationCnt++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-time.After(f.delay):
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}}, nil
}

func (f *trackingExecutor) MaxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

type scriptedProvider struct {
	name   string
	vision bool

	mu      sync.Mutex
	calls   int
	resps   []*types.MessageResponse
	streams [][]streamItem
	reqs    []*types.MessageRequest
}

type timeoutProvider struct{}

func (p *timeoutProvider) Name() string { return "test" }
func (p *timeoutProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}
func (p *timeoutProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *timeoutProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return nil, io.EOF
}

type streamItem struct {
	event types.StreamEvent
	err   error
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true, ToolStreaming: true, Vision: p.vision}
}

func (p *scriptedProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req != nil {
		copied := *req
		copied.Messages = append([]types.Message(nil), req.Messages...)
		p.reqs = append(p.reqs, &copied)
	}
	if p.calls >= len(p.resps) {
		return nil, io.EOF
	}
	resp := p.resps[p.calls]
	p.calls++
	return resp, nil
}

func (p *scriptedProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req != nil {
		copied := *req
		copied.Messages = append([]types.Message(nil), req.Messages...)
		p.reqs = append(p.reqs, &copied)
	}
	if p.calls >= len(p.streams) {
		return nil, io.EOF
	}
	items := p.streams[p.calls]
	p.calls++
	cpy := make([]streamItem, len(items))
	copy(cpy, items)
	return &scriptedEventStream2{items: cpy}, nil
}

type scriptedEventStream2 struct {
	items []streamItem
	i     int
}

func (s *scriptedEventStream2) Next() (types.StreamEvent, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	item := s.items[s.i]
	s.i++
	return item.event, item.err
}

func (s *scriptedEventStream2) Close() error { return nil }

type toolExecutorFunc func(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error)

func (fn toolExecutorFunc) Execute(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error) {
	return fn(ctx, name, input)
}

func TestRunBlocking_NoToolCompletion(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
	}}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}, Tools: nil}, Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.StopReason != types.RunStopReasonEndTurn {
		t.Fatalf("stop_reason=%q", result.StopReason)
	}
	if result.TurnCount != 1 {
		t.Fatalf("turn_count=%d", result.TurnCount)
	}
	if got := result.Response.TextContent(); got != "done" {
		t.Fatalf("response=%q", got)
	}
}

func TestRunBlocking_ToolLoop(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonToolUse,
			Content: []types.ContentBlock{types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  builtins.BuiltinWebSearch,
				Input: map[string]any{"query": "q"},
			}},
		},
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonEndTurn,
			Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "final"}},
		},
	}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}}, Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.StopReason != types.RunStopReasonEndTurn {
		t.Fatalf("stop_reason=%q", result.StopReason)
	}
	if result.ToolCallCount != 1 {
		t.Fatalf("tool_call_count=%d", result.ToolCallCount)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("steps=%d", len(result.Steps))
	}
	if len(result.Messages) != 4 {
		b, _ := json.Marshal(result.Messages)
		t.Fatalf("messages=%d %s", len(result.Messages), string(b))
	}
}

func TestRunBlocking_ToolErrorAddsNonEmptyContent(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonToolUse,
			Content: []types.ContentBlock{types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  builtins.BuiltinWebSearch,
				Input: map[string]any{"query": "q"},
			}},
		},
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonEndTurn,
			Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "final"}},
		},
	}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(failingExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(result.Steps) < 1 || len(result.Steps[0].ToolResults) != 1 {
		t.Fatalf("unexpected tool results: %+v", result.Steps)
	}
	toolResult := result.Steps[0].ToolResults[0]
	if !toolResult.IsError {
		t.Fatalf("expected tool error result: %+v", toolResult)
	}
	if toolResult.Error == nil || toolResult.Error.Message != "tool exploded" {
		t.Fatalf("unexpected error payload: %+v", toolResult.Error)
	}
	if len(toolResult.Content) != 1 {
		t.Fatalf("content len=%d", len(toolResult.Content))
	}
	text, ok := toolResult.Content[0].(types.TextBlock)
	if !ok {
		t.Fatalf("content[0]=%T, want types.TextBlock", toolResult.Content[0])
	}
	if text.Text != "tool exploded" {
		t.Fatalf("text=%q, want tool exploded", text.Text)
	}
}

func TestRunBlocking_InjectsImageRefsWithoutMutatingHistory(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
	}}}

	originalImage := types.ImageBlock{
		Type: "image",
		Source: types.ImageSource{
			Type:      "url",
			URL:       "https://example.com/cat.png",
			MediaType: "image/png",
		},
	}
	userHistory := []types.Message{{
		Role: "user",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "tell me about this image"},
			originalImage,
		},
	}}

	controller := &Controller{
		Provider: provider,
		Tools: toolExecutorFunc(func(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error) {
			return nil, nil
		}),
	}
	_, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: userHistory},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(provider.reqs) != 1 {
		t.Fatalf("provider requests=%d, want 1", len(provider.reqs))
	}
	gotBlocks := provider.reqs[0].Messages[0].ContentBlocks()
	if len(gotBlocks) != 3 {
		t.Fatalf("provider blocks=%d, want 3", len(gotBlocks))
	}
	if tb, ok := gotBlocks[1].(types.TextBlock); !ok || tb.Text != "img-01" {
		t.Fatalf("provider blocks[1]=%#v, want img-01 text block", gotBlocks[1])
	}
	originalBlocks := userHistory[0].ContentBlocks()
	if len(originalBlocks) != 2 {
		t.Fatalf("original history mutated: len=%d", len(originalBlocks))
	}
}

func TestRunBlocking_KeepsImageToolResultsCanonicalAndResolvesRefs(t *testing.T) {
	provider := &scriptedProvider{name: "test", vision: true, resps: []*types.MessageResponse{
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonToolUse,
			Content: []types.ContentBlock{types.ToolUseBlock{
				Type: "tool_use",
				ID:   "call_1",
				Name: servertools.ToolImage,
				Input: map[string]any{
					"prompt": "make it dramatic",
					"images": []any{map[string]any{"id": "img-01"}},
				},
			}},
		},
		{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonEndTurn,
			Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "final"}},
		},
	}}

	inputImage := types.ImageBlock{
		Type: "image",
		Source: types.ImageSource{
			Type:      "url",
			URL:       "https://example.com/input.png",
			MediaType: "image/png",
		},
	}
	generatedImage := types.ImageBlock{
		Type: "image",
		Source: types.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      "aGVsbG8=",
		},
	}
	controller := &Controller{
		Provider: provider,
		Tools: toolExecutorFunc(func(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error) {
			if name != servertools.ToolImage {
				t.Fatalf("unexpected tool name %q", name)
			}
			reg := servertools.ImageRefRegistryFromContext(ctx)
			if reg == nil {
				t.Fatal("expected image ref registry in tool context")
			}
			if _, ok := reg.Lookup("img-01"); !ok {
				t.Fatal("expected img-01 to resolve in tool context")
			}
			return []types.ContentBlock{
				types.TextBlock{Type: "text", Text: `{"tool":"vai_image","provider":"gem-dev","model":"gemini-3.1-flash-image-preview","prompt":"make it dramatic","referenced_image_ids":["img-01"],"generated_image_ids":[]}`},
				generatedImage,
			}, nil
		}),
	}

	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{
			Model: "m",
			Messages: []types.Message{{
				Role: "user",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "edit this"},
					inputImage,
				},
			}},
		},
		Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.Response == nil || result.Response.TextContent() != "final" {
		t.Fatalf("unexpected final response: %#v", result.Response)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("messages=%d, want 4", len(result.Messages))
	}
	if len(provider.reqs) != 2 {
		t.Fatalf("provider requests=%d, want 2", len(provider.reqs))
	}
	secondTurnBlocks := provider.reqs[1].Messages[2].ContentBlocks()
	secondTurnResult, ok := secondTurnBlocks[0].(types.ToolResultBlock)
	if !ok {
		t.Fatalf("second turn block=%T, want ToolResultBlock", secondTurnBlocks[0])
	}
	if len(secondTurnResult.Content) < 3 {
		t.Fatalf("second turn tool result content=%#v", secondTurnResult.Content)
	}
	if tb, ok := secondTurnResult.Content[1].(types.TextBlock); !ok || tb.Text != "img-02" {
		t.Fatalf("expected injected img-02 ref before generated image, got %#v", secondTurnResult.Content[1])
	}
	if _, ok := secondTurnResult.Content[2].(types.ImageBlock); !ok {
		t.Fatalf("expected real generated image in multimodal planner history, got %T", secondTurnResult.Content[2])
	}
	toolMsgBlocks := result.Messages[2].ContentBlocks()
	tr, ok := toolMsgBlocks[0].(types.ToolResultBlock)
	if !ok {
		t.Fatalf("tool message block=%T, want ToolResultBlock", toolMsgBlocks[0])
	}
	meta, ok := tr.Content[0].(types.TextBlock)
	if !ok || !strings.Contains(meta.Text, `"generated_image_ids":["img-02"]`) {
		t.Fatalf("tool metadata=%#v", tr.Content[0])
	}
	lastAssistant := result.Messages[3].ContentBlocks()
	if len(lastAssistant) != 1 {
		t.Fatalf("assistant history should remain text-only, got %#v", lastAssistant)
	}
	if _, ok := lastAssistant[0].(types.TextBlock); !ok {
		t.Fatalf("assistant history should not duplicate image output: %#v", lastAssistant)
	}
}

func TestRunBlocking_ReplacesImagesWithPlaceholdersForNonVisionPlanner(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
	}}}

	userHistory := []types.Message{{
		Role: "user",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "describe this"},
			types.ImageBlock{
				Type: "image",
				Source: types.ImageSource{
					Type:      "url",
					URL:       "https://example.com/cat.png",
					MediaType: "image/png",
				},
			},
		},
	}}

	controller := &Controller{Provider: provider}
	controller.Tools = toolExecutorFunc(func(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error) {
		return nil, nil
	})
	_, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: userHistory},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(provider.reqs) != 1 {
		t.Fatalf("provider requests=%d, want 1", len(provider.reqs))
	}
	blocks := provider.reqs[0].Messages[0].ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("provider blocks=%d, want 3", len(blocks))
	}
	if tb, ok := blocks[1].(types.TextBlock); !ok || tb.Text != "img-01" {
		t.Fatalf("expected img-01 ref, got %#v", blocks[1])
	}
	if tb, ok := blocks[2].(types.TextBlock); !ok || tb.Text != "[image omitted for non-vision model]" {
		t.Fatalf("expected placeholder text, got %#v", blocks[2])
	}
	for _, block := range blocks {
		if _, ok := block.(types.ImageBlock); ok {
			t.Fatalf("non-vision planner request should not contain image block: %#v", blocks)
		}
	}
}

func TestRunStream_OrderingAndEOFTerminal(t *testing.T) {
	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn
	provider := &scriptedProvider{name: "test", streams: [][]streamItem{{
		{event: types.MessageStartEvent{Type: "message_start", Message: types.MessageResponse{Type: "message", Role: "assistant", Model: "m"}}},
		{event: types.ContentBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: types.TextBlock{Type: "text", Text: ""}}},
		{event: types.ContentBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: types.TextDelta{Type: "text_delta", Text: "done"}}},
		{event: delta, err: io.EOF},
	}}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch}), StreamIdleTimeout: time.Second, PublicModel: "anthropic/test", RequestID: "req_1"}
	events := make([]string, 0, 16)
	result, err := controller.RunStream(context.Background(), &types.RunRequest{Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}}, Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000}}, func(ev types.RunStreamEvent) error {
		events = append(events, ev.EventType())
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "run_start") || !strings.Contains(joined, "step_start") || !strings.Contains(joined, "stream_event") || !strings.Contains(joined, "step_complete") || !strings.Contains(joined, "history_delta") || !strings.Contains(joined, "run_complete") {
		t.Fatalf("events=%s", joined)
	}
	if result.Response == nil || result.Response.TextContent() != "done" {
		t.Fatalf("result=%+v", result)
	}
}

func TestRunStream_VoiceEventsAreTopLevelRunEvents(t *testing.T) {
	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn
	provider := &scriptedProvider{name: "test", streams: [][]streamItem{{
		{event: types.MessageStartEvent{Type: "message_start", Message: types.MessageResponse{Type: "message", Role: "assistant", Model: "m"}}},
		{event: types.ContentBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: types.TextBlock{Type: "text", Text: ""}}},
		{event: types.ContentBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: types.TextDelta{Type: "text_delta", Text: "hello."}}},
		{event: delta, err: io.EOF},
	}}}

	pipeline := voice.NewPipelineWithProviders(nil, &fakeTTSProvider{})
	controller := &Controller{
		Provider:          provider,
		Tools:             builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch}),
		VoicePipeline:     pipeline,
		StreamIdleTimeout: time.Second,
		PublicModel:       "anthropic/test",
		RequestID:         "req_1",
	}
	seen := make([]types.RunStreamEvent, 0, 16)
	_, err := controller.RunStream(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{
			Model:    "m",
			Messages: []types.Message{{Role: "user", Content: "hi"}},
			Voice:    &types.VoiceConfig{Output: &types.VoiceOutputConfig{Voice: "v", Format: "pcm"}},
		},
		Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	}, func(ev types.RunStreamEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	sawAudioChunk := false
	sawWrappedAudio := false
	for _, ev := range seen {
		switch e := ev.(type) {
		case types.AudioChunkEvent:
			sawAudioChunk = true
		case types.RunStreamEventWrapper:
			if _, ok := e.Event.(types.AudioChunkEvent); ok {
				sawWrappedAudio = true
			}
		}
	}
	if !sawAudioChunk {
		t.Fatalf("expected top-level audio_chunk event, saw=%v", seen)
	}
	if sawWrappedAudio {
		t.Fatalf("audio_chunk should not be wrapped inside stream_event: %+v", seen)
	}
}

func TestRunStream_VoiceLongBurst_NoBackpressureTruncation(t *testing.T) {
	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn
	provider := &scriptedProvider{name: "test", streams: [][]streamItem{{
		{event: types.MessageStartEvent{Type: "message_start", Message: types.MessageResponse{Type: "message", Role: "assistant", Model: "m"}}},
		{event: types.ContentBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: types.TextBlock{Type: "text", Text: ""}}},
		{event: types.ContentBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: types.TextDelta{Type: "text_delta", Text: "long response body."}}},
		{event: delta, err: io.EOF},
	}}}

	pipeline := voice.NewPipelineWithProviders(nil, &fakeTTSProvider{
		chunksOnFinal: 220,
		chunkPayload:  []byte{1, 2, 3, 4},
	})
	controller := &Controller{
		Provider:          provider,
		Tools:             builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch}),
		VoicePipeline:     pipeline,
		StreamIdleTimeout: 2 * time.Second,
		PublicModel:       "anthropic/test",
		RequestID:         "req_1",
	}

	seen := make([]types.RunStreamEvent, 0, 256)
	_, err := controller.RunStream(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{
			Model:    "m",
			Messages: []types.Message{{Role: "user", Content: "hi"}},
			Voice:    &types.VoiceConfig{Output: &types.VoiceOutputConfig{Voice: "v", Format: "pcm"}},
		},
		Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	}, func(ev types.RunStreamEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	audioChunkCount := 0
	finalChunkCount := 0
	backpressureUnavailable := false
	for _, ev := range seen {
		switch e := ev.(type) {
		case types.AudioChunkEvent:
			audioChunkCount++
			if e.IsFinal {
				finalChunkCount++
			}
		case types.AudioUnavailableEvent:
			if e.Reason == "backpressure" {
				backpressureUnavailable = true
			}
		}
	}
	if audioChunkCount < 150 {
		t.Fatalf("audio chunk count=%d, want >=150; events=%v", audioChunkCount, seen)
	}
	if finalChunkCount != 1 {
		t.Fatalf("final chunk count=%d, want 1; events=%v", finalChunkCount, seen)
	}
	if backpressureUnavailable {
		t.Fatalf("unexpected backpressure audio_unavailable event: %v", seen)
	}
}

type fakeTTSProvider struct {
	chunksPerSend int
	chunksOnFinal int
	chunkPayload  []byte
}

func (f *fakeTTSProvider) Name() string { return "fake" }
func (f *fakeTTSProvider) Synthesize(ctx context.Context, text string, opts tts.SynthesizeOptions) (*tts.Synthesis, error) {
	return &tts.Synthesis{Audio: []byte{1, 2, 3}, Format: "pcm"}, nil
}
func (f *fakeTTSProvider) SynthesizeStream(ctx context.Context, text string, opts tts.SynthesizeOptions) (*tts.SynthesisStream, error) {
	stream := tts.NewSynthesisStream()
	stream.Send([]byte{1, 2, 3})
	stream.FinishSending()
	return stream, nil
}
func (f *fakeTTSProvider) NewStreamingContext(ctx context.Context, opts tts.StreamingContextOptions) (*tts.StreamingContext, error) {
	sc := tts.NewStreamingContext()
	chunksPerSend := f.chunksPerSend
	if chunksPerSend <= 0 {
		chunksPerSend = 1
	}
	chunkPayload := f.chunkPayload
	if len(chunkPayload) == 0 {
		chunkPayload = []byte{1, 2, 3}
	}
	sc.SendFunc = func(text string, isFinal bool) error {
		if strings.TrimSpace(text) != "" {
			for i := 0; i < chunksPerSend; i++ {
				if !sc.PushAudio(chunkPayload) {
					return nil
				}
			}
		}
		if isFinal {
			for i := 0; i < f.chunksOnFinal; i++ {
				if !sc.PushAudio(chunkPayload) {
					break
				}
			}
			sc.FinishAudio()
		}
		return nil
	}
	sc.CloseFunc = func() error {
		return nil
	}
	return sc, nil
}

type fakeRunloopSTTProvider struct {
	transcript string
}

func (f *fakeRunloopSTTProvider) Name() string { return "fake-stt" }
func (f *fakeRunloopSTTProvider) Transcribe(ctx context.Context, audio io.Reader, opts stt.TranscribeOptions) (*stt.Transcript, error) {
	return &stt.Transcript{Text: f.transcript}, nil
}
func (f *fakeRunloopSTTProvider) TranscribeStream(context.Context, io.Reader, stt.TranscribeOptions) (<-chan stt.TranscriptDelta, error) {
	return nil, nil
}
func (f *fakeRunloopSTTProvider) NewStreamingSTT(context.Context, stt.TranscribeOptions) (*stt.StreamingSTT, error) {
	return nil, nil
}

type captureCreateProvider struct {
	lastReq *types.MessageRequest
	resp    *types.MessageResponse
}

func (p *captureCreateProvider) Name() string { return "test" }
func (p *captureCreateProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}
func (p *captureCreateProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	if req != nil {
		reqCopy := *req
		p.lastReq = &reqCopy
	}
	if p.resp != nil {
		return p.resp, nil
	}
	return &types.MessageResponse{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
	}, nil
}
func (p *captureCreateProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	return nil, io.EOF
}

func TestRunBlocking_VoiceOutputAppended(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}},
	}}}

	pipeline := voice.NewPipelineWithProviders(nil, &fakeTTSProvider{})
	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch}), VoicePipeline: pipeline}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}, Voice: &types.VoiceConfig{Output: &types.VoiceOutputConfig{Voice: "v", Format: "pcm"}}}, Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if audio := result.Response.AudioContent(); audio == nil {
		t.Fatalf("expected audio content in response: %+v", result.Response.Content)
	}
}

func TestRunBlocking_AudioSTTPreprocessedAndTranscriptStored(t *testing.T) {
	provider := &captureCreateProvider{
		resp: &types.MessageResponse{
			Type:       "message",
			Role:       "assistant",
			Model:      "m",
			StopReason: types.StopReasonEndTurn,
			Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}},
		},
	}

	pipeline := voice.NewPipelineWithProviders(&fakeRunloopSTTProvider{transcript: "from audio stt"}, &fakeTTSProvider{})
	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch}), VoicePipeline: pipeline}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{
			Model:    "m",
			STTModel: "cartesia/ink-whisper",
			Messages: []types.Message{{
				Role: "user",
				Content: []types.ContentBlock{
					types.AudioSTTBlock{
						Type: "audio_stt",
						Source: types.AudioSource{
							Type:      "base64",
							MediaType: "audio/wav",
							Data:      "AAAA",
						},
					},
				},
			}},
		},
		Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if provider.lastReq == nil {
		t.Fatalf("expected provider request capture")
	}
	blocks := provider.lastReq.Messages[0].ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("len(blocks)=%d, want 1", len(blocks))
	}
	tb, ok := blocks[0].(types.TextBlock)
	if !ok {
		t.Fatalf("expected transcribed text block, got %T", blocks[0])
	}
	if tb.Text != "from audio stt" {
		t.Fatalf("text=%q, want %q", tb.Text, "from audio stt")
	}
	if got := result.Response.UserTranscript(); got != "from audio stt" {
		t.Fatalf("user transcript=%q, want %q", got, "from audio stt")
	}
}

func TestRunBlocking_AudioSTTWithoutPipelineRejected(t *testing.T) {
	provider := &captureCreateProvider{}
	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	_, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{
			Model: "m",
			Messages: []types.Message{{
				Role: "user",
				Content: []types.ContentBlock{
					types.AudioSTTBlock{
						Type: "audio_stt",
						Source: types.AudioSource{
							Type:      "base64",
							MediaType: "audio/wav",
							Data:      "AAAA",
						},
					},
				},
			}},
		},
		Run: types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestRunBlocking_StopReasonMaxTurns(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonToolUse,
		Content: []types.ContentBlock{types.ToolUseBlock{
			Type:  "tool_use",
			ID:    "call_1",
			Name:  builtins.BuiltinWebSearch,
			Input: map[string]any{"query": "q"},
		}},
	}}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 1, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.StopReason != types.RunStopReasonMaxTurns {
		t.Fatalf("stop_reason=%q", result.StopReason)
	}
	if result.TurnCount != 1 {
		t.Fatalf("turn_count=%d", result.TurnCount)
	}
}

func TestRunBlocking_StopReasonMaxToolCalls(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonToolUse,
		Content: []types.ContentBlock{
			types.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: builtins.BuiltinWebSearch, Input: map[string]any{"query": "a"}},
			types.ToolUseBlock{Type: "tool_use", ID: "call_2", Name: builtins.BuiltinWebSearch, Input: map[string]any{"query": "b"}},
		},
	}}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 1, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.StopReason != types.RunStopReasonMaxToolCalls {
		t.Fatalf("stop_reason=%q", result.StopReason)
	}
}

func TestRunBlocking_StopReasonMaxTokens(t *testing.T) {
	provider := &scriptedProvider{name: "test", resps: []*types.MessageResponse{{
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		StopReason: types.StopReasonEndTurn,
		Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
		Usage:      types.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10},
	}}}

	controller := &Controller{Provider: provider, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, MaxTokens: 10, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.StopReason != types.RunStopReasonMaxTokens {
		t.Fatalf("stop_reason=%q", result.StopReason)
	}
}

func TestRunBlocking_Timeout(t *testing.T) {
	controller := &Controller{Provider: &timeoutProvider{}, Tools: builtins.NewRegistry(fakeExecutor{name: builtins.BuiltinWebSearch})}
	result, err := controller.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 20, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if result == nil || result.StopReason != types.RunStopReasonTimeout {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunBlocking_ParallelVsSequentialToolExecution(t *testing.T) {
	newProvider := func() *scriptedProvider {
		return &scriptedProvider{name: "test", resps: []*types.MessageResponse{
			{
				Type:       "message",
				Role:       "assistant",
				Model:      "m",
				StopReason: types.StopReasonToolUse,
				Content: []types.ContentBlock{
					types.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: builtins.BuiltinWebSearch, Input: map[string]any{"query": "a"}},
					types.ToolUseBlock{Type: "tool_use", ID: "call_2", Name: builtins.BuiltinWebSearch, Input: map[string]any{"query": "b"}},
				},
			},
			{
				Type:       "message",
				Role:       "assistant",
				Model:      "m",
				StopReason: types.StopReasonEndTurn,
				Content:    []types.ContentBlock{types.TextBlock{Type: "text", Text: "done"}},
			},
		}}
	}

	parallelExec := &trackingExecutor{name: builtins.BuiltinWebSearch, delay: 20 * time.Millisecond}
	parallelController := &Controller{Provider: newProvider(), Tools: builtins.NewRegistry(parallelExec)}
	parallelResult, err := parallelController.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: true, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("parallel err=%v", err)
	}
	if parallelResult.StopReason != types.RunStopReasonEndTurn {
		t.Fatalf("parallel stop_reason=%q", parallelResult.StopReason)
	}
	if parallelExec.MaxInFlight() < 2 {
		t.Fatalf("parallel max_in_flight=%d, expected concurrent execution", parallelExec.MaxInFlight())
	}

	seqExec := &trackingExecutor{name: builtins.BuiltinWebSearch, delay: 20 * time.Millisecond}
	seqController := &Controller{Provider: newProvider(), Tools: builtins.NewRegistry(seqExec)}
	seqResult, err := seqController.RunBlocking(context.Background(), &types.RunRequest{
		Request: types.MessageRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: "hi"}}},
		Run:     types.RunConfig{MaxTurns: 8, MaxToolCalls: 20, TimeoutMS: 60000, ParallelTools: false, ToolTimeoutMS: 30000},
	})
	if err != nil {
		t.Fatalf("sequential err=%v", err)
	}
	if seqResult.StopReason != types.RunStopReasonEndTurn {
		t.Fatalf("sequential stop_reason=%q", seqResult.StopReason)
	}
	if seqExec.MaxInFlight() != 1 {
		t.Fatalf("sequential max_in_flight=%d, expected sequential execution", seqExec.MaxInFlight())
	}
}

func TestToTypesError_GeminiErrorMapped(t *testing.T) {
	got := toTypesError(&gem.Error{
		Type:          gem.ErrRateLimit,
		Message:       "quota exceeded",
		Code:          "RESOURCE_EXHAUSTED",
		ProviderError: map[string]any{"status": "RESOURCE_EXHAUSTED"},
	}, "req_test")

	if got.Type != string(core.ErrRateLimit) {
		t.Fatalf("type=%q", got.Type)
	}
	if got.Message != "quota exceeded" {
		t.Fatalf("message=%q", got.Message)
	}
	if got.Code != "RESOURCE_EXHAUSTED" {
		t.Fatalf("code=%q", got.Code)
	}
	if got.RequestID != "req_test" {
		t.Fatalf("request_id=%q", got.RequestID)
	}
	if got.ProviderError == nil {
		t.Fatal("provider_error should be preserved")
	}
}

func TestToTypesError_UnknownErrorKeepsCauseInProviderError(t *testing.T) {
	got := toTypesError(errors.New("boom"), "req_test")
	if got.Type != string(core.ErrAPI) {
		t.Fatalf("type=%q", got.Type)
	}
	if got.Message != "internal error" {
		t.Fatalf("message=%q", got.Message)
	}
	if got.RequestID != "req_test" {
		t.Fatalf("request_id=%q", got.RequestID)
	}
	if got.ProviderError == nil {
		t.Fatal("provider_error should contain original cause")
	}
	if got.ProviderError.(string) != "boom" {
		t.Fatalf("provider_error=%v", got.ProviderError)
	}
}
