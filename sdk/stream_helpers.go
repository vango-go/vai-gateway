package vai

import (
	"fmt"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

// StreamCallbacks defines handlers for stream events.
// All callbacks are optional - nil callbacks are skipped.
type StreamCallbacks struct {
	// Content deltas
	OnTextDelta        func(text string)     // Text content as it streams
	OnThinkingDelta    func(thinking string) // Claude's reasoning process (extended thinking)
	OnAudioChunk       func(data []byte, format string)
	OnAudioUnavailable func(reason, message string)

	// Tool-use stream lifecycle (model content_block_* events).
	OnToolUseStart   func(index int, id, name string)
	OnToolInputDelta func(index int, id, name, partialJSON string)
	OnToolUseStop    func(index int, id, name string)

	// Tool execution lifecycle (SDK tool runner events).
	OnToolCallStart func(id, name string, input map[string]any)                    // Tool execution starting
	OnToolResult    func(id, name string, content []types.ContentBlock, err error) // Tool execution complete

	// Lifecycle (for RunStream)
	OnStepStart    func(index int)                                      // New step beginning
	OnStepComplete func(index int, response *Response)                  // Step finished
	OnInterrupted  func(partialText string, behavior InterruptBehavior) // Stream was interrupted

	// Errors
	OnError func(err error)
}

// TextDeltaFrom extracts text from any event that contains a text delta.
// Returns the text and true if found, empty string and false otherwise.
func TextDeltaFrom(event RunStreamEvent) (string, bool) {
	switch e := event.(type) {
	case StreamEventWrapper:
		if delta, ok := e.Event.(types.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.(types.TextDelta); ok {
				return text.Text, true
			}
		}
	}
	return "", false
}

// ThinkingDeltaFrom extracts thinking content from thinking delta events.
// Returns the thinking text and true if found.
func ThinkingDeltaFrom(event RunStreamEvent) (string, bool) {
	switch e := event.(type) {
	case StreamEventWrapper:
		if delta, ok := e.Event.(types.ContentBlockDeltaEvent); ok {
			if thinking, ok := delta.Delta.(types.ThinkingDelta); ok {
				return thinking.Thinking, true
			}
		}
	}
	return "", false
}

// AudioChunkFrom extracts streaming audio chunks from run stream events.
func AudioChunkFrom(event RunStreamEvent) (AudioChunkEvent, bool) {
	audio, ok := event.(AudioChunkEvent)
	return audio, ok
}

// AudioUnavailableFrom extracts top-level audio-unavailable run events.
func AudioUnavailableFrom(event RunStreamEvent) (AudioUnavailableEvent, bool) {
	unavailable, ok := event.(AudioUnavailableEvent)
	return unavailable, ok
}

// ToolUseStartFrom extracts tool-use start metadata from wrapped stream events.
func ToolUseStartFrom(event RunStreamEvent) (index int, id, name string, ok bool) {
	wrapped, ok := event.(StreamEventWrapper)
	if !ok {
		return 0, "", "", false
	}
	start, ok := wrapped.Event.(types.ContentBlockStartEvent)
	if !ok {
		return 0, "", "", false
	}
	tool, ok := start.ContentBlock.(types.ToolUseBlock)
	if !ok {
		return 0, "", "", false
	}
	return start.Index, tool.ID, tool.Name, true
}

// ToolInputDeltaFrom extracts input_json_delta payloads from wrapped stream events.
func ToolInputDeltaFrom(event RunStreamEvent) (index int, partialJSON string, ok bool) {
	wrapped, ok := event.(StreamEventWrapper)
	if !ok {
		return 0, "", false
	}
	delta, ok := wrapped.Event.(types.ContentBlockDeltaEvent)
	if !ok {
		return 0, "", false
	}
	inputDelta, ok := delta.Delta.(types.InputJSONDelta)
	if !ok {
		return 0, "", false
	}
	return delta.Index, inputDelta.PartialJSON, true
}

// ToolUseStopFrom extracts tool-use stop indices from wrapped stream events.
func ToolUseStopFrom(event RunStreamEvent) (index int, ok bool) {
	wrapped, ok := event.(StreamEventWrapper)
	if !ok {
		return 0, false
	}
	stop, ok := wrapped.Event.(types.ContentBlockStopEvent)
	if !ok {
		return 0, false
	}
	return stop.Index, true
}

// Process consumes the stream with callbacks and returns the accumulated text.
// This is a convenience method for the common pattern of handling stream events.
//
// Example:
//
//	text, err := stream.Process(vai.StreamCallbacks{
//	    OnTextDelta:  func(t string) { fmt.Print(t) },
//	})
func (rs *RunStream) Process(callbacks StreamCallbacks) (string, error) {
	var text strings.Builder
	toolMetaByIndex := make(map[int]toolStreamMeta)

	for event := range rs.Events() {
		switch e := event.(type) {
		case StreamEventWrapper:
			rs.processStreamEvent(e.Event, &text, callbacks, toolMetaByIndex)

		case AudioChunkEvent:
			if callbacks.OnAudioChunk != nil {
				callbacks.OnAudioChunk(e.Data, e.Format)
			}

		case AudioUnavailableEvent:
			if callbacks.OnAudioUnavailable != nil {
				callbacks.OnAudioUnavailable(e.Reason, e.Message)
			}

		case StepStartEvent:
			if callbacks.OnStepStart != nil {
				callbacks.OnStepStart(e.Index)
			}

		case StepCompleteEvent:
			if callbacks.OnStepComplete != nil {
				callbacks.OnStepComplete(e.Index, e.Response)
			}

		case ToolCallStartEvent:
			if callbacks.OnToolCallStart != nil {
				callbacks.OnToolCallStart(e.ID, e.Name, e.Input)
			}

		case ToolResultEvent:
			if callbacks.OnToolResult != nil {
				callbacks.OnToolResult(e.ID, e.Name, e.Content, e.Error)
			}

		case InterruptedEvent:
			if callbacks.OnInterrupted != nil {
				callbacks.OnInterrupted(e.PartialText, e.Behavior)
			}

		case RunCompleteEvent:
			// Run finished, loop will exit

		}
	}

	if err := rs.Err(); err != nil {
		if callbacks.OnError != nil {
			callbacks.OnError(err)
		}
		return text.String(), err
	}

	return text.String(), nil
}

// processStreamEvent handles the inner stream events (from the LLM).
func (rs *RunStream) processStreamEvent(event types.StreamEvent, text *strings.Builder, callbacks StreamCallbacks, toolMetaByIndex map[int]toolStreamMeta) {
	switch e := event.(type) {
	case types.ContentBlockStartEvent:
		if tool, ok := e.ContentBlock.(types.ToolUseBlock); ok {
			meta := toolStreamMeta{id: tool.ID, name: tool.Name}
			toolMetaByIndex[e.Index] = meta
			if callbacks.OnToolUseStart != nil {
				callbacks.OnToolUseStart(e.Index, meta.id, meta.name)
			}
		}

	case types.ContentBlockDeltaEvent:
		switch delta := e.Delta.(type) {
		case types.TextDelta:
			text.WriteString(delta.Text)
			if callbacks.OnTextDelta != nil {
				callbacks.OnTextDelta(delta.Text)
			}
		case types.ThinkingDelta:
			if callbacks.OnThinkingDelta != nil {
				callbacks.OnThinkingDelta(delta.Thinking)
			}
		case types.InputJSONDelta:
			if callbacks.OnToolInputDelta != nil {
				meta := toolMetaByIndex[e.Index]
				callbacks.OnToolInputDelta(e.Index, meta.id, meta.name, delta.PartialJSON)
			}
		}

	case types.ContentBlockStopEvent:
		if callbacks.OnToolUseStop != nil {
			if meta, ok := toolMetaByIndex[e.Index]; ok {
				callbacks.OnToolUseStop(e.Index, meta.id, meta.name)
			}
		}
		delete(toolMetaByIndex, e.Index)

	case types.ErrorEvent:
		if callbacks.OnError != nil {
			callbacks.OnError(fmt.Errorf("%s: %s", e.Error.Type, e.Error.Message))
		}
	}
}

type toolStreamMeta struct {
	id   string
	name string
}
