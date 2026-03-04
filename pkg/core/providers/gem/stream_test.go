package gem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestEventStream_TextDeltas(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")
	seq := seqResponses(
		&genai.GenerateContentResponse{
			ResponseID: "r1",
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{genai.NewPartFromText("Hel")},
					},
					FinishReason: genai.FinishReasonUnspecified,
				},
			},
		},
		&genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{genai.NewPartFromText("lo")},
					},
					FinishReason: genai.FinishReasonStop,
				},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     5,
				CandidatesTokenCount: 2,
				TotalTokenCount:      7,
			},
		},
	)

	stream := newEventStream(context.Background(), provider, "gemini-2.5-flash", 1, seq, &requestBuild{})
	events := collectEvents(t, stream)

	if len(events) == 0 {
		t.Fatalf("expected streaming events")
	}
	if _, ok := events[0].(types.MessageStartEvent); !ok {
		t.Fatalf("first event should be message_start, got %T", events[0])
	}

	text := collectTextDeltas(events)
	if text != "Hello" {
		t.Fatalf("text deltas = %q, want %q", text, "Hello")
	}
	if !hasEventType[types.ContentBlockStopEvent](events) {
		t.Fatalf("expected content_block_stop")
	}
	md, ok := findEvent[types.MessageDeltaEvent](events)
	if !ok {
		t.Fatalf("expected message_delta")
	}
	if md.Delta.StopReason != types.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", md.Delta.StopReason)
	}
	if md.Usage.TotalTokens != 7 {
		t.Fatalf("usage total tokens = %d", md.Usage.TotalTokens)
	}
}

func TestEventStream_VertexPartialArgsRootStringMode(t *testing.T) {
	t.Parallel()

	provider := NewVertex("test-key")
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "fc_1",
								Name: "talk_to_user",
								PartialArgs: []*genai.PartialArg{
									{JsonPath: "$.text", StringValue: "hel"},
									{JsonPath: "$.text", StringValue: "lo"},
								},
							},
							ThoughtSignature: []byte("opaque-sig"),
						},
					},
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
	}

	stream := newEventStream(context.Background(), provider, "gemini-2.5-flash", 42, seqResponses(resp), &requestBuild{})
	events := collectEvents(t, stream)

	start, ok := findEvent[types.ContentBlockStartEvent](events)
	if !ok {
		t.Fatalf("missing content_block_start")
	}
	tb, ok := start.ContentBlock.(types.ToolUseBlock)
	if !ok {
		t.Fatalf("expected tool_use start block, got %T", start.ContentBlock)
	}
	if tb.ID != "fc_1" || tb.Name != "talk_to_user" {
		t.Fatalf("unexpected tool_use block: %+v", tb)
	}

	rawJSON := collectToolJSON(events, start.Index)
	if !json.Valid([]byte(rawJSON)) {
		t.Fatalf("tool JSON is invalid: %q", rawJSON)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		t.Fatalf("tool JSON unmarshal failed: %v", err)
	}
	if parsed["text"] != "hello" {
		t.Fatalf("parsed text = %#v, want hello", parsed["text"])
	}
	if _, ok := parsed[thoughtSignatureKey]; !ok {
		t.Fatalf("missing thought signature in streamed args")
	}

	md, ok := findEvent[types.MessageDeltaEvent](events)
	if !ok {
		t.Fatalf("expected message_delta")
	}
	if md.Delta.StopReason != types.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want tool_use", md.Delta.StopReason)
	}
}

func TestEventStream_BufferedArgsAndDeveloperFallback(t *testing.T) {
	t.Parallel()

	t.Run("vertex buffered jsonpath", func(t *testing.T) {
		provider := NewVertex("test-key")
		resp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									Name: "complex_tool",
									PartialArgs: []*genai.PartialArg{
										{JsonPath: "$.a", StringValue: "value"},
									},
								},
							},
						},
					},
				},
			},
		}
		stream := newEventStream(context.Background(), provider, "gemini-2.5-flash", 7, seqResponses(resp), &requestBuild{})
		events := collectEvents(t, stream)

		start, ok := findEvent[types.ContentBlockStartEvent](events)
		if !ok {
			t.Fatalf("missing content_block_start")
		}
		rawJSON := collectToolJSON(events, start.Index)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
			t.Fatalf("unmarshal tool json: %v", err)
		}
		if parsed["a"] != "value" {
			t.Fatalf("expected a=value, got %#v raw=%q", parsed["a"], rawJSON)
		}
	})

	t.Run("developer full args delta", func(t *testing.T) {
		provider := NewDeveloper("test-key")
		resp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									ID:   "call_dev",
									Name: "do_work",
									Args: map[string]any{"x": "y"},
								},
							},
						},
					},
				},
			},
		}
		stream := newEventStream(context.Background(), provider, "gemini-2.5-flash", 8, seqResponses(resp), &requestBuild{})
		events := collectEvents(t, stream)

		start, ok := findEvent[types.ContentBlockStartEvent](events)
		if !ok {
			t.Fatalf("missing content_block_start")
		}
		tb := start.ContentBlock.(types.ToolUseBlock)
		if tb.ID != "call_dev" {
			t.Fatalf("tool id = %q", tb.ID)
		}

		rawJSON := collectToolJSON(events, start.Index)
		if strings.TrimSpace(rawJSON) == "" {
			t.Fatalf("expected tool input JSON delta")
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
			t.Fatalf("unmarshal tool json: %v", err)
		}
		if parsed["x"] != "y" {
			t.Fatalf("parsed args = %#v", parsed)
		}
	})
}

func TestEventStream_IteratorErrorEmitsErrorEvent(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")
	stream := newEventStream(
		context.Background(),
		provider,
		"gemini-2.5-flash",
		1,
		func(yield func(*genai.GenerateContentResponse, error) bool) {
			_ = yield(nil, errors.New("boom"))
		},
		&requestBuild{},
	)

	ev, err := stream.Next()
	if err != nil {
		t.Fatalf("unexpected err from first Next: %v", err)
	}
	if _, ok := ev.(types.MessageStartEvent); !ok {
		t.Fatalf("expected message_start, got %T", ev)
	}

	ev, err = stream.Next()
	if err != nil {
		t.Fatalf("unexpected err while reading error event: %v", err)
	}
	if _, ok := ev.(types.ErrorEvent); !ok {
		t.Fatalf("expected error event, got %T", ev)
	}

	_, err = stream.Next()
	if err == nil {
		t.Fatalf("expected terminal error after error event")
	}
}

func seqResponses(responses ...*genai.GenerateContentResponse) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, resp := range responses {
			if !yield(resp, nil) {
				return
			}
		}
	}
}

func collectEvents(t *testing.T, stream EventStream) []types.StreamEvent {
	t.Helper()
	defer stream.Close()

	out := make([]types.StreamEvent, 0, 16)
	for {
		ev, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			// Next may return an error after yielding an error event.
			return out
		}
		out = append(out, ev)
	}
}

func collectTextDeltas(events []types.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		delta, ok := ev.(types.ContentBlockDeltaEvent)
		if !ok {
			continue
		}
		if td, ok := delta.Delta.(types.TextDelta); ok {
			b.WriteString(td.Text)
		}
	}
	return b.String()
}

func collectToolJSON(events []types.StreamEvent, idx int) string {
	var b strings.Builder
	for _, ev := range events {
		delta, ok := ev.(types.ContentBlockDeltaEvent)
		if !ok || delta.Index != idx {
			continue
		}
		if td, ok := delta.Delta.(types.InputJSONDelta); ok {
			b.WriteString(td.PartialJSON)
		}
	}
	return b.String()
}

func hasEventType[T any](events []types.StreamEvent) bool {
	_, ok := findEvent[T](events)
	return ok
}

func findEvent[T any](events []types.StreamEvent) (T, bool) {
	var zero T
	for _, ev := range events {
		if v, ok := ev.(T); ok {
			return v, true
		}
	}
	return zero, false
}
