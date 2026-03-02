package vai

import (
	"errors"
	"io"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestStream_Response_ReconstructsToolInputFromInputJSONDelta(t *testing.T) {
	stream := newStreamFromEventStream(&scriptedEventStream{
		events: toolUseEventsForStreamTest([]string{`{"a":6,`, `"b":7}`}, true),
	})

	for range stream.Events() {
	}

	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response")
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("len(tool uses) = %d, want 1", len(uses))
	}

	input := uses[0].Input
	if got, ok := input["a"].(float64); !ok || got != 6 {
		t.Fatalf("input[a] = %v (%T), want 6", input["a"], input["a"])
	}
	if got, ok := input["b"].(float64); !ok || got != 7 {
		t.Fatalf("input[b] = %v (%T), want 7", input["b"], input["b"])
	}
}

func TestStream_Response_CompactsSparseContentIndices(t *testing.T) {
	stream := newStreamFromEventStream(&scriptedEventStream{
		events: toolUseEventsForStreamTestAtIndex([]string{`{"a":6,`, `"b":7}`}, true, 1),
	})

	for range stream.Events() {
	}

	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response")
	}
	if got := len(resp.Content); got != 1 {
		t.Fatalf("len(response content) = %d, want 1", got)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("len(tool uses) = %d, want 1", len(uses))
	}
	if got := uses[0].Name; got != "multiply" {
		t.Fatalf("tool name = %q, want %q", got, "multiply")
	}
}

func TestStream_Response_ReconstructsToolInputWithoutContentBlockStop(t *testing.T) {
	stream := newStreamFromEventStream(&scriptedEventStream{
		events: toolUseEventsForStreamTest([]string{`{"a":6,`, `"b":7}`}, false),
	})

	for range stream.Events() {
	}

	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response")
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("len(tool uses) = %d, want 1", len(uses))
	}

	input := uses[0].Input
	if got, ok := input["a"].(float64); !ok || got != 6 {
		t.Fatalf("input[a] = %v (%T), want 6", input["a"], input["a"])
	}
	if got, ok := input["b"].(float64); !ok || got != 7 {
		t.Fatalf("input[b] = %v (%T), want 7", input["b"], input["b"])
	}
}

func TestStream_Response_InvalidInputJSONDelta_DoesNotFail(t *testing.T) {
	stream := newStreamFromEventStream(&scriptedEventStream{
		events: toolUseEventsForStreamTest([]string{`{"a":6`}, true),
	})

	for range stream.Events() {
	}

	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response")
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("len(tool uses) = %d, want 1", len(uses))
	}
	if len(uses[0].Input) != 0 {
		t.Fatalf("expected empty input after invalid JSON, got %#v", uses[0].Input)
	}
}

func TestStream_Response_ConsumesTerminalEventWhenReturnedWithEOF(t *testing.T) {
	terminalDelta := types.MessageDeltaEvent{
		Type: "message_delta",
		Usage: types.Usage{
			InputTokens:  3,
			OutputTokens: 5,
			TotalTokens:  8,
		},
	}
	terminalDelta.Delta.StopReason = types.StopReasonEndTurn

	stream := newStreamFromEventStream(&terminalEOFEventStream{
		events: []types.StreamEvent{
			types.MessageStartEvent{
				Type: "message_start",
				Message: types.MessageResponse{
					Type:  "message",
					Role:  "assistant",
					Model: "test-model",
				},
			},
			types.ContentBlockStartEvent{
				Type:  "content_block_start",
				Index: 0,
				ContentBlock: types.TextBlock{
					Type: "text",
					Text: "",
				},
			},
			types.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: 0,
				Delta: types.TextDelta{
					Type: "text_delta",
					Text: "done",
				},
			},
		},
		terminalEvent: terminalDelta,
	})

	for range stream.Events() {
	}

	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.StopReason != types.StopReasonEndTurn {
		t.Fatalf("response stop reason = %q, want %q", resp.StopReason, types.StopReasonEndTurn)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 5 || resp.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v, want input=3 output=5 total=8", resp.Usage)
	}

	if err := stream.Err(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream.Err() = %v, want io.EOF", err)
	}
}

type terminalEOFEventStream struct {
	events        []types.StreamEvent
	index         int
	terminalEvent types.StreamEvent
	emittedFinal  bool
}

func (s *terminalEOFEventStream) Next() (types.StreamEvent, error) {
	if s.index < len(s.events) {
		ev := s.events[s.index]
		s.index++
		return ev, nil
	}
	if !s.emittedFinal {
		s.emittedFinal = true
		return s.terminalEvent, io.EOF
	}
	return nil, io.EOF
}

func (s *terminalEOFEventStream) Close() error {
	return nil
}

func toolUseEventsForStreamTest(inputParts []string, includeBlockStop bool) []types.StreamEvent {
	return toolUseEventsForStreamTestAtIndex(inputParts, includeBlockStop, 0)
}

func toolUseEventsForStreamTestAtIndex(inputParts []string, includeBlockStop bool, index int) []types.StreamEvent {
	events := []types.StreamEvent{
		types.MessageStartEvent{
			Type: "message_start",
			Message: types.MessageResponse{
				Type:  "message",
				Role:  "assistant",
				Model: "test-model",
			},
		},
		types.ContentBlockStartEvent{
			Type:  "content_block_start",
			Index: index,
			ContentBlock: types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "multiply",
				Input: map[string]any{},
			},
		},
	}

	for _, part := range inputParts {
		events = append(events, types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: index,
			Delta: types.InputJSONDelta{
				Type:        "input_json_delta",
				PartialJSON: part,
			},
		})
	}

	if includeBlockStop {
		events = append(events, types.ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: index,
		})
	}

	delta := types.MessageDeltaEvent{
		Type: "message_delta",
	}
	delta.Delta.StopReason = types.StopReasonToolUse

	events = append(events,
		delta,
		types.MessageStopEvent{Type: "message_stop"},
	)

	return events
}
