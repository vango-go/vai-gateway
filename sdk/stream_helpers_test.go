package vai

import (
	"strings"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestAudioChunkFrom(t *testing.T) {
	event := AudioChunkEvent{Data: []byte("chunk"), Format: "wav"}
	chunk, ok := AudioChunkFrom(event)
	if !ok {
		t.Fatalf("expected audio chunk event")
	}
	if string(chunk.Data) != "chunk" || chunk.Format != "wav" {
		t.Fatalf("unexpected chunk payload: %#v", chunk)
	}
}

func TestAudioUnavailableFrom(t *testing.T) {
	event := AudioUnavailableEvent{Reason: "tts_failed", Message: "provider down"}
	unavailable, ok := AudioUnavailableFrom(event)
	if !ok {
		t.Fatalf("expected audio unavailable event")
	}
	if unavailable.Reason != "tts_failed" || unavailable.Message != "provider down" {
		t.Fatalf("unexpected unavailable payload: %#v", unavailable)
	}
}

func TestRunStreamProcess_OnAudioChunkCallback(t *testing.T) {
	events := make(chan RunStreamEvent, 2)
	events <- AudioChunkEvent{Data: []byte("a1"), Format: "wav"}
	events <- AudioChunkEvent{Data: []byte("a2"), Format: "wav"}
	close(events)

	done := make(chan struct{})
	close(done)

	rs := &RunStream{
		events: events,
		done:   done,
	}

	var got int
	_, err := rs.Process(StreamCallbacks{
		OnAudioChunk: func(data []byte, format string) {
			got++
			if format != "wav" {
				t.Fatalf("format = %q, want wav", format)
			}
		},
	})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if got != 2 {
		t.Fatalf("OnAudioChunk calls = %d, want 2", got)
	}
}

func TestRunStreamProcess_OnAudioUnavailableCallback(t *testing.T) {
	events := make(chan RunStreamEvent, 2)
	events <- AudioUnavailableEvent{Reason: "tts_failed", Message: "provider down"}
	events <- AudioUnavailableEvent{Reason: "tts_failed", Message: "ignored duplicate for callback count"}
	close(events)

	done := make(chan struct{})
	close(done)

	rs := &RunStream{
		events: events,
		done:   done,
	}

	var calls int
	var reasons []string
	_, err := rs.Process(StreamCallbacks{
		OnAudioUnavailable: func(reason, message string) {
			calls++
			reasons = append(reasons, reason+"|"+message)
		},
	})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("OnAudioUnavailable calls = %d, want 2", calls)
	}
	if reasons[0] != "tts_failed|provider down" {
		t.Fatalf("first callback = %q, want %q", reasons[0], "tts_failed|provider down")
	}
}

func TestToolUseStartFrom_MatchesWrappedToolUseStart(t *testing.T) {
	event := StreamEventWrapper{
		Event: types.ContentBlockStartEvent{
			Type:  "content_block_start",
			Index: 1,
			ContentBlock: types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "talk_to_user",
				Input: map[string]any{},
			},
		},
	}

	index, id, name, ok := ToolUseStartFrom(event)
	if !ok {
		t.Fatal("ToolUseStartFrom() ok=false, want true")
	}
	if index != 1 || id != "call_1" || name != "talk_to_user" {
		t.Fatalf("ToolUseStartFrom()=(%d,%q,%q), want (1,%q,%q)", index, id, name, "call_1", "talk_to_user")
	}
}

func TestToolInputDeltaFrom_MatchesWrappedInputJSONDelta(t *testing.T) {
	event := StreamEventWrapper{
		Event: types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: 3,
			Delta: types.InputJSONDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"message":"he`,
			},
		},
	}

	index, partialJSON, ok := ToolInputDeltaFrom(event)
	if !ok {
		t.Fatal("ToolInputDeltaFrom() ok=false, want true")
	}
	if index != 3 || partialJSON != `{"message":"he` {
		t.Fatalf("ToolInputDeltaFrom()=(%d,%q), want (3,%q)", index, partialJSON, `{"message":"he`)
	}
}

func TestToolUseStopFrom_MatchesWrappedContentBlockStop(t *testing.T) {
	event := StreamEventWrapper{
		Event: types.ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: 7,
		},
	}

	index, ok := ToolUseStopFrom(event)
	if !ok {
		t.Fatal("ToolUseStopFrom() ok=false, want true")
	}
	if index != 7 {
		t.Fatalf("ToolUseStopFrom() index=%d, want 7", index)
	}
}

func TestToolStreamExtractors_NonMatchesReturnFalse(t *testing.T) {
	textEvent := StreamEventWrapper{
		Event: types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: 0,
			Delta: types.TextDelta{Type: "text_delta", Text: "hello"},
		},
	}
	if _, _, _, ok := ToolUseStartFrom(textEvent); ok {
		t.Fatal("ToolUseStartFrom() ok=true for non-tool event")
	}
	if _, _, ok := ToolInputDeltaFrom(textEvent); ok {
		t.Fatal("ToolInputDeltaFrom() ok=true for non-input_json_delta event")
	}
	if _, ok := ToolUseStopFrom(textEvent); ok {
		t.Fatal("ToolUseStopFrom() ok=true for non-stop event")
	}

	if _, _, _, ok := ToolUseStartFrom(AudioChunkEvent{}); ok {
		t.Fatal("ToolUseStartFrom() ok=true for non-wrapper event")
	}
}

func TestRunStreamProcess_OnToolInputCallbacks_BasicLifecycle(t *testing.T) {
	events := make(chan RunStreamEvent, 8)
	events <- StreamEventWrapper{
		Event: types.ContentBlockStartEvent{
			Type:  "content_block_start",
			Index: 1,
			ContentBlock: types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "talk_to_user",
				Input: map[string]any{},
			},
		},
	}
	events <- StreamEventWrapper{
		Event: types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: 1,
			Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `{"message":"hel`},
		},
	}
	events <- StreamEventWrapper{
		Event: types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: 1,
			Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `lo"}`},
		},
	}
	events <- StreamEventWrapper{
		Event: types.ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: 1,
		},
	}
	close(events)

	done := make(chan struct{})
	close(done)
	rs := &RunStream{events: events, done: done}

	var order []string
	var parts strings.Builder
	_, err := rs.Process(StreamCallbacks{
		OnToolUseStart: func(index int, id, name string) {
			order = append(order, "start")
			if index != 1 || id != "call_1" || name != "talk_to_user" {
				t.Fatalf("OnToolUseStart(%d,%q,%q), want (1,%q,%q)", index, id, name, "call_1", "talk_to_user")
			}
		},
		OnToolInputDelta: func(index int, id, name, partialJSON string) {
			order = append(order, "delta")
			if index != 1 || id != "call_1" || name != "talk_to_user" {
				t.Fatalf("OnToolInputDelta index/meta=(%d,%q,%q), want (1,%q,%q)", index, id, name, "call_1", "talk_to_user")
			}
			parts.WriteString(partialJSON)
		},
		OnToolUseStop: func(index int, id, name string) {
			order = append(order, "stop")
			if index != 1 || id != "call_1" || name != "talk_to_user" {
				t.Fatalf("OnToolUseStop(%d,%q,%q), want (1,%q,%q)", index, id, name, "call_1", "talk_to_user")
			}
		},
	})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if got := strings.Join(order, ","); got != "start,delta,delta,stop" {
		t.Fatalf("callback order = %q, want %q", got, "start,delta,delta,stop")
	}
	if got := parts.String(); got != `{"message":"hello"}` {
		t.Fatalf("joined input_json_delta = %q, want %q", got, `{"message":"hello"}`)
	}
}

func TestRunStreamProcess_OnToolInputDelta_WithoutPriorStart(t *testing.T) {
	events := make(chan RunStreamEvent, 2)
	events <- StreamEventWrapper{
		Event: types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: 5,
			Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `{"x":1}`},
		},
	}
	close(events)

	done := make(chan struct{})
	close(done)
	rs := &RunStream{events: events, done: done}

	calls := 0
	_, err := rs.Process(StreamCallbacks{
		OnToolInputDelta: func(index int, id, name, partialJSON string) {
			calls++
			if index != 5 {
				t.Fatalf("index=%d, want 5", index)
			}
			if id != "" || name != "" {
				t.Fatalf("id/name=(%q,%q), want empty strings", id, name)
			}
			if partialJSON != `{"x":1}` {
				t.Fatalf("partialJSON=%q, want %q", partialJSON, `{"x":1}`)
			}
		},
	})
	if err != nil {
		t.Fatalf("Process() error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("OnToolInputDelta calls=%d, want 1", calls)
	}
}

func TestRunStreamProcess_OnToolInputCallbacks_MultipleToolIndices(t *testing.T) {
	events := make(chan RunStreamEvent, 12)
	events <- StreamEventWrapper{Event: types.ContentBlockStartEvent{
		Type:         "content_block_start",
		Index:        1,
		ContentBlock: types.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: "tool_a", Input: map[string]any{}},
	}}
	events <- StreamEventWrapper{Event: types.ContentBlockStartEvent{
		Type:         "content_block_start",
		Index:        2,
		ContentBlock: types.ToolUseBlock{Type: "tool_use", ID: "call_2", Name: "tool_b", Input: map[string]any{}},
	}}
	events <- StreamEventWrapper{Event: types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: 2,
		Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `{"b":`},
	}}
	events <- StreamEventWrapper{Event: types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: 1,
		Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `{"a":`},
	}}
	events <- StreamEventWrapper{Event: types.ContentBlockStopEvent{Type: "content_block_stop", Index: 2}}
	events <- StreamEventWrapper{Event: types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: 1,
		Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: `1}`},
	}}
	events <- StreamEventWrapper{Event: types.ContentBlockStopEvent{Type: "content_block_stop", Index: 1}}
	close(events)

	done := make(chan struct{})
	close(done)
	rs := &RunStream{events: events, done: done}

	type seenDelta struct {
		index int
		id    string
		name  string
		part  string
	}
	var deltas []seenDelta
	_, err := rs.Process(StreamCallbacks{
		OnToolInputDelta: func(index int, id, name, partialJSON string) {
			deltas = append(deltas, seenDelta{index: index, id: id, name: name, part: partialJSON})
		},
	})
	if err != nil {
		t.Fatalf("Process() error=%v", err)
	}
	if len(deltas) != 3 {
		t.Fatalf("delta count=%d, want 3", len(deltas))
	}
	if deltas[0].index != 2 || deltas[0].id != "call_2" || deltas[0].name != "tool_b" {
		t.Fatalf("delta[0]=%+v, want index=2 id=call_2 name=tool_b", deltas[0])
	}
	if deltas[1].index != 1 || deltas[1].id != "call_1" || deltas[1].name != "tool_a" {
		t.Fatalf("delta[1]=%+v, want index=1 id=call_1 name=tool_a", deltas[1])
	}
	if deltas[2].index != 1 || deltas[2].id != "call_1" || deltas[2].name != "tool_a" {
		t.Fatalf("delta[2]=%+v, want index=1 id=call_1 name=tool_a", deltas[2])
	}
}

func TestRunStreamProcess_ExecutionCallbacksUnchanged(t *testing.T) {
	events := make(chan RunStreamEvent, 3)
	events <- StreamEventWrapper{
		Event: types.ContentBlockStartEvent{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: types.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_stream",
				Name:  "talk_to_user",
				Input: map[string]any{},
			},
		},
	}
	events <- ToolCallStartEvent{
		ID:    "call_exec",
		Name:  "talk_to_user",
		Input: map[string]any{"message": "hello"},
	}
	close(events)

	done := make(chan struct{})
	close(done)
	rs := &RunStream{events: events, done: done}

	var streamStarts, execStarts int
	_, err := rs.Process(StreamCallbacks{
		OnToolUseStart: func(index int, id, name string) {
			streamStarts++
			if index != 0 || id != "call_stream" || name != "talk_to_user" {
				t.Fatalf("OnToolUseStart got (%d,%q,%q)", index, id, name)
			}
		},
		OnToolCallStart: func(id, name string, input map[string]any) {
			execStarts++
			if id != "call_exec" || name != "talk_to_user" {
				t.Fatalf("OnToolCallStart got (%q,%q)", id, name)
			}
			if got, ok := input["message"].(string); !ok || got != "hello" {
				t.Fatalf("input[message]=%v (%T), want %q", input["message"], input["message"], "hello")
			}
		},
	})
	if err != nil {
		t.Fatalf("Process() error=%v", err)
	}
	if streamStarts != 1 {
		t.Fatalf("OnToolUseStart calls=%d, want 1", streamStarts)
	}
	if execStarts != 1 {
		t.Fatalf("OnToolCallStart calls=%d, want 1", execStarts)
	}
}
