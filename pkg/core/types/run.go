package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RunRequest is the request body for /v1/runs and /v1/runs:stream.
type RunRequest struct {
	Request          MessageRequest `json:"request"`
	Run              RunConfig      `json:"run,omitempty"`
	ServerTools      []string       `json:"server_tools,omitempty"`
	ServerToolConfig map[string]any `json:"server_tool_config,omitempty"`
	Builtins         []string       `json:"builtins,omitempty"`
}

// RunConfig controls server-side tool-loop limits.
type RunConfig struct {
	MaxTurns      int  `json:"max_turns,omitempty"`
	MaxToolCalls  int  `json:"max_tool_calls,omitempty"`
	MaxTokens     int  `json:"max_tokens,omitempty"`
	TimeoutMS     int  `json:"timeout_ms,omitempty"`
	ParallelTools bool `json:"parallel_tools"`
	ToolTimeoutMS int  `json:"tool_timeout_ms,omitempty"`
}

// RunStopReason indicates why a run ended.
type RunStopReason string

const (
	RunStopReasonEndTurn      RunStopReason = "end_turn"
	RunStopReasonMaxToolCalls RunStopReason = "max_tool_calls"
	RunStopReasonMaxTurns     RunStopReason = "max_turns"
	RunStopReasonMaxTokens    RunStopReason = "max_tokens"
	RunStopReasonTimeout      RunStopReason = "timeout"
	RunStopReasonCancelled    RunStopReason = "cancelled"
	RunStopReasonError        RunStopReason = "error"
	RunStopReasonCustom       RunStopReason = "custom"
)

// RunResultEnvelope is the blocking /v1/runs response envelope.
type RunResultEnvelope struct {
	Result *RunResult `json:"result"`
}

// RunResult is the terminal result of a server-side tool loop.
type RunResult struct {
	Response      *MessageResponse `json:"response,omitempty"`
	Steps         []RunStep        `json:"steps"`
	ToolCallCount int              `json:"tool_call_count"`
	TurnCount     int              `json:"turn_count"`
	Usage         Usage            `json:"usage"`
	StopReason    RunStopReason    `json:"stop_reason"`
	Messages      []Message        `json:"messages,omitempty"`
}

// RunStep captures one model-turn iteration.
type RunStep struct {
	Index       int              `json:"index"`
	Response    *MessageResponse `json:"response,omitempty"`
	ToolCalls   []RunToolCall    `json:"tool_calls,omitempty"`
	ToolResults []RunToolResult  `json:"tool_results,omitempty"`
	DurationMS  int64            `json:"duration_ms"`
}

// RunToolCall is a tool invocation emitted by the model.
type RunToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// RunToolResult is a gateway-side tool execution result.
type RunToolResult struct {
	ToolUseID string         `json:"tool_use_id"`
	Content   []ContentBlock `json:"content"`
	IsError   bool           `json:"is_error,omitempty"`
	Error     *Error         `json:"error,omitempty"`
}

// RunStreamEvent is the interface implemented by /v1/runs:stream SSE events.
type RunStreamEvent interface {
	EventType() string
}

// RunStartEvent is emitted once at stream start.
type RunStartEvent struct {
	Type            string `json:"type"`
	RequestID       string `json:"request_id,omitempty"`
	Model           string `json:"model"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

func (e RunStartEvent) EventType() string { return "run_start" }

// RunStepStartEvent is emitted at each turn start.
type RunStepStartEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func (e RunStepStartEvent) EventType() string { return "step_start" }

// RunStreamEventWrapper wraps provider stream events.
type RunStreamEventWrapper struct {
	Type  string      `json:"type"`
	Event StreamEvent `json:"event"`
}

func (e RunStreamEventWrapper) EventType() string { return "stream_event" }

// RunToolCallStartEvent is emitted before gateway tool execution.
type RunToolCallStartEvent struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func (e RunToolCallStartEvent) EventType() string { return "tool_call_start" }

// RunToolResultEvent is emitted after gateway tool execution.
type RunToolResultEvent struct {
	Type    string         `json:"type"`
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
	Error   *Error         `json:"error,omitempty"`
}

func (e RunToolResultEvent) EventType() string { return "tool_result" }

// RunStepCompleteEvent is emitted when a step finishes.
type RunStepCompleteEvent struct {
	Type     string           `json:"type"`
	Index    int              `json:"index"`
	Response *MessageResponse `json:"response,omitempty"`
}

func (e RunStepCompleteEvent) EventType() string { return "step_complete" }

// RunHistoryDeltaEvent appends deterministic history messages.
type RunHistoryDeltaEvent struct {
	Type        string    `json:"type"`
	ExpectedLen int       `json:"expected_len"`
	Append      []Message `json:"append"`
}

func (e RunHistoryDeltaEvent) EventType() string { return "history_delta" }

// RunCompleteEvent is the terminal success completion event.
type RunCompleteEvent struct {
	Type   string     `json:"type"`
	Result *RunResult `json:"result"`
}

func (e RunCompleteEvent) EventType() string { return "run_complete" }

// RunPingEvent is an optional keepalive event.
type RunPingEvent struct {
	Type string `json:"type"`
}

func (e RunPingEvent) EventType() string { return "ping" }

// RunErrorEvent is the terminal error event.
type RunErrorEvent struct {
	Type  string `json:"type"`
	Error Error  `json:"error"`
}

func (e RunErrorEvent) EventType() string { return "error" }

// UnknownRunStreamEvent is an opaque run-stream event used for forward compatibility.
type UnknownRunStreamEvent struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

func (e UnknownRunStreamEvent) EventType() string { return e.Type }

func (e UnknownRunStreamEvent) MarshalJSON() ([]byte, error) {
	if len(e.Raw) > 0 {
		return copiedRaw(e.Raw), nil
	}
	return json.Marshal(struct {
		Type string `json:"type"`
	}{
		Type: e.Type,
	})
}

// UnmarshalRunStreamEvent deserializes a /v1/runs:stream event from JSON.
func UnmarshalRunStreamEvent(data []byte) (RunStreamEvent, error) {
	var typeHolder struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typeHolder); err != nil {
		return nil, err
	}
	if strings.TrimSpace(typeHolder.Type) == "" {
		return nil, fmt.Errorf("missing run stream event type")
	}

	switch typeHolder.Type {
	case "run_start":
		var event RunStartEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "step_start":
		var event RunStepStartEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "stream_event":
		var raw struct {
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		nested, err := UnmarshalStreamEvent(raw.Event)
		if err != nil {
			return nil, err
		}
		return RunStreamEventWrapper{
			Type:  raw.Type,
			Event: nested,
		}, nil

	case "tool_call_start":
		var event RunToolCallStartEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "tool_result":
		var raw struct {
			Type    string            `json:"type"`
			ID      string            `json:"id"`
			Name    string            `json:"name"`
			Content []json.RawMessage `json:"content"`
			IsError bool              `json:"is_error,omitempty"`
			Error   *Error            `json:"error,omitempty"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		content, err := unmarshalContentBlocks(raw.Content)
		if err != nil {
			return nil, err
		}
		return RunToolResultEvent{
			Type:    raw.Type,
			ID:      raw.ID,
			Name:    raw.Name,
			Content: content,
			IsError: raw.IsError,
			Error:   raw.Error,
		}, nil

	case "step_complete":
		var raw struct {
			Type     string          `json:"type"`
			Index    int             `json:"index"`
			Response json.RawMessage `json:"response,omitempty"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		response, err := unmarshalOptionalMessageResponse(raw.Response)
		if err != nil {
			return nil, err
		}
		return RunStepCompleteEvent{
			Type:     raw.Type,
			Index:    raw.Index,
			Response: response,
		}, nil

	case "history_delta":
		var event RunHistoryDeltaEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "run_complete":
		var raw struct {
			Type   string          `json:"type"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		result, err := unmarshalRunResult(raw.Result)
		if err != nil {
			return nil, err
		}
		return RunCompleteEvent{
			Type:   raw.Type,
			Result: result,
		}, nil

	case "ping":
		var event RunPingEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "audio_chunk":
		var event AudioChunkEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "audio_unavailable":
		var event AudioUnavailableEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	case "error":
		var event RunErrorEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil

	default:
		return UnknownRunStreamEvent{
			Type: typeHolder.Type,
			Raw:  copiedRaw(data),
		}, nil
	}
}

// UnmarshalRunResultEnvelope deserializes a blocking /v1/runs response.
func UnmarshalRunResultEnvelope(data []byte) (*RunResultEnvelope, error) {
	var raw struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	result, err := unmarshalRunResult(raw.Result)
	if err != nil {
		return nil, err
	}
	return &RunResultEnvelope{Result: result}, nil
}

func unmarshalRunResult(data []byte) (*RunResult, error) {
	if isNullRaw(data) {
		return nil, nil
	}

	var raw struct {
		Response json.RawMessage `json:"response,omitempty"`
		Steps    []struct {
			Index       int               `json:"index"`
			Response    json.RawMessage   `json:"response,omitempty"`
			ToolCalls   []RunToolCall     `json:"tool_calls,omitempty"`
			ToolResults []json.RawMessage `json:"tool_results,omitempty"`
			DurationMS  int64             `json:"duration_ms"`
		} `json:"steps"`
		ToolCallCount int           `json:"tool_call_count"`
		TurnCount     int           `json:"turn_count"`
		Usage         Usage         `json:"usage"`
		StopReason    RunStopReason `json:"stop_reason"`
		Messages      []Message     `json:"messages,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	response, err := unmarshalOptionalMessageResponse(raw.Response)
	if err != nil {
		return nil, err
	}

	steps := make([]RunStep, 0, len(raw.Steps))
	for _, rawStep := range raw.Steps {
		stepResponse, err := unmarshalOptionalMessageResponse(rawStep.Response)
		if err != nil {
			return nil, err
		}

		toolResults := make([]RunToolResult, 0, len(rawStep.ToolResults))
		for _, rawToolResult := range rawStep.ToolResults {
			tr, err := unmarshalRunToolResult(rawToolResult)
			if err != nil {
				return nil, err
			}
			toolResults = append(toolResults, tr)
		}

		steps = append(steps, RunStep{
			Index:       rawStep.Index,
			Response:    stepResponse,
			ToolCalls:   rawStep.ToolCalls,
			ToolResults: toolResults,
			DurationMS:  rawStep.DurationMS,
		})
	}

	return &RunResult{
		Response:      response,
		Steps:         steps,
		ToolCallCount: raw.ToolCallCount,
		TurnCount:     raw.TurnCount,
		Usage:         raw.Usage,
		StopReason:    raw.StopReason,
		Messages:      raw.Messages,
	}, nil
}

func unmarshalRunToolResult(data []byte) (RunToolResult, error) {
	var raw struct {
		ToolUseID string            `json:"tool_use_id"`
		Content   []json.RawMessage `json:"content"`
		IsError   bool              `json:"is_error,omitempty"`
		Error     *Error            `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return RunToolResult{}, err
	}
	content, err := unmarshalContentBlocks(raw.Content)
	if err != nil {
		return RunToolResult{}, err
	}
	return RunToolResult{
		ToolUseID: raw.ToolUseID,
		Content:   content,
		IsError:   raw.IsError,
		Error:     raw.Error,
	}, nil
}

func unmarshalContentBlocks(rawBlocks []json.RawMessage) ([]ContentBlock, error) {
	content := make([]ContentBlock, 0, len(rawBlocks))
	for _, blockRaw := range rawBlocks {
		block, err := UnmarshalContentBlock(blockRaw)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
	}
	return content, nil
}

func unmarshalOptionalMessageResponse(data []byte) (*MessageResponse, error) {
	if isNullRaw(data) {
		return nil, nil
	}
	return UnmarshalMessageResponse(data)
}

func isNullRaw(data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	return trimmed == "" || trimmed == "null"
}
