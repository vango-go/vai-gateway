package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LiveClientFrame is a marker interface for /v1/live websocket client frames.
type LiveClientFrame interface {
	LiveClientFrameType() string
}

// LiveServerEvent is a marker interface for /v1/live websocket server events.
type LiveServerEvent interface {
	LiveServerEventType() string
}

// LivePlaybackMarkFrame reports incremental assistant audio playback progress for a turn.
// Clients should send this periodically while audio is playing (e.g. every 250ms).
// played_ms is turn-relative and must be monotonic.
type LivePlaybackMarkFrame struct {
	Type     string `json:"type"` // "playback_mark"
	TurnID   string `json:"turn_id,omitempty"`
	PlayedMS int    `json:"played_ms"`
}

func (f LivePlaybackMarkFrame) LiveClientFrameType() string { return "playback_mark" }

// LiveStartFrame configures a live session.
type LiveStartFrame struct {
	Type       string     `json:"type"` // "start"
	RunRequest RunRequest `json:"run_request"`
}

func (f LiveStartFrame) LiveClientFrameType() string { return "start" }

// LiveToolResultFrame sends a tool result for a pending tool call execution.
type LiveToolResultFrame struct {
	Type        string         `json:"type"` // "tool_result"
	ExecutionID string         `json:"execution_id"`
	Content     []ContentBlock `json:"content,omitempty"`
	IsError     bool           `json:"is_error,omitempty"`
	Error       any            `json:"error,omitempty"`
}

func (f LiveToolResultFrame) LiveClientFrameType() string { return "tool_result" }

// LiveStopFrame requests graceful session shutdown.
type LiveStopFrame struct {
	Type string `json:"type"` // "stop"
}

func (f LiveStopFrame) LiveClientFrameType() string { return "stop" }

// LivePlaybackStateFrame reports whether assistant audio playback for a turn has stopped or finished.
type LivePlaybackStateFrame struct {
	Type   string `json:"type"` // "playback_state"
	TurnID string `json:"turn_id,omitempty"`
	State  string `json:"state"` // "finished" | "stopped"
}

func (f LivePlaybackStateFrame) LiveClientFrameType() string { return "playback_state" }

// LiveSessionStartedEvent confirms session startup and fixed audio contracts.
type LiveSessionStartedEvent struct {
	Type               string `json:"type"` // "session_started"
	InputFormat        string `json:"input_format"`
	InputSampleRateHz  int    `json:"input_sample_rate_hz"`
	OutputFormat       string `json:"output_format"`
	OutputSampleRateHz int    `json:"output_sample_rate_hz"`
	SilenceCommitMS    int    `json:"silence_commit_ms"`
}

func (e LiveSessionStartedEvent) LiveServerEventType() string { return "session_started" }

// LiveAssistantTextDeltaEvent streams assistant text output.
type LiveAssistantTextDeltaEvent struct {
	Type   string `json:"type"` // "assistant_text_delta"
	TurnID string `json:"turn_id,omitempty"`
	Text   string `json:"text"`
}

func (e LiveAssistantTextDeltaEvent) LiveServerEventType() string { return "assistant_text_delta" }

// LiveAudioChunkEvent streams synthesized audio chunks for assistant speech.
type LiveAudioChunkEvent struct {
	Type         string `json:"type"` // "audio_chunk"
	TurnID       string `json:"turn_id,omitempty"`
	Format       string `json:"format"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Audio        string `json:"audio"` // base64
	IsFinal      bool   `json:"is_final,omitempty"`
}

func (e LiveAudioChunkEvent) LiveServerEventType() string { return "audio_chunk" }

// LiveToolCallEvent requests client execution for an arbitrary function tool.
type LiveToolCallEvent struct {
	Type        string         `json:"type"` // "tool_call"
	TurnID      string         `json:"turn_id,omitempty"`
	ExecutionID string         `json:"execution_id"`
	Name        string         `json:"name"`
	Input       map[string]any `json:"input"`
}

func (e LiveToolCallEvent) LiveServerEventType() string { return "tool_call" }

// LiveUserTurnCommittedEvent indicates an utterance has been committed and sent as audio_stt.
type LiveUserTurnCommittedEvent struct {
	Type       string `json:"type"` // "user_turn_committed"
	TurnID     string `json:"turn_id,omitempty"`
	AudioBytes int    `json:"audio_bytes"`
}

func (e LiveUserTurnCommittedEvent) LiveServerEventType() string { return "user_turn_committed" }

// LiveTurnCompleteEvent indicates a turn has completed and includes synced history.
type LiveTurnCompleteEvent struct {
	Type       string        `json:"type"` // "turn_complete"
	TurnID     string        `json:"turn_id,omitempty"`
	StopReason RunStopReason `json:"stop_reason"`
	History    []Message     `json:"history"`
}

func (e LiveTurnCompleteEvent) LiveServerEventType() string { return "turn_complete" }

// LiveAudioUnavailableEvent indicates voice output is unavailable for the current response.
type LiveAudioUnavailableEvent struct {
	Type    string `json:"type"` // "audio_unavailable"
	TurnID  string `json:"turn_id,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e LiveAudioUnavailableEvent) LiveServerEventType() string { return "audio_unavailable" }

// LiveAudioResetEvent instructs the client to immediately drop any buffered assistant audio for a turn.
// This is used for barge-in interruption and similar hard-stop conditions.
type LiveAudioResetEvent struct {
	Type   string `json:"type"` // "audio_reset"
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (e LiveAudioResetEvent) LiveServerEventType() string { return "audio_reset" }

// LiveTurnCancelledEvent indicates a turn was cancelled during the grace period.
type LiveTurnCancelledEvent struct {
	Type   string `json:"type"` // "turn_cancelled"
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (e LiveTurnCancelledEvent) LiveServerEventType() string { return "turn_cancelled" }

// LiveErrorEvent reports protocol/runtime errors over /v1/live.
type LiveErrorEvent struct {
	Type    string `json:"type"` // "error"
	Fatal   bool   `json:"fatal"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (e LiveErrorEvent) LiveServerEventType() string { return "error" }

// UnmarshalLiveClientFrame deserializes a /v1/live client frame from JSON.
func UnmarshalLiveClientFrame(data []byte) (LiveClientFrame, error) {
	var typeHolder struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typeHolder); err != nil {
		return nil, err
	}
	if strings.TrimSpace(typeHolder.Type) == "" {
		return nil, fmt.Errorf("missing live client frame type")
	}

	switch typeHolder.Type {
	case "start":
		var frame LiveStartFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil, err
		}
		return frame, nil
	case "tool_result":
		var raw struct {
			Type        string            `json:"type"`
			ExecutionID string            `json:"execution_id"`
			Content     []json.RawMessage `json:"content,omitempty"`
			IsError     bool              `json:"is_error,omitempty"`
			Error       any               `json:"error,omitempty"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		content, err := unmarshalContentBlocks(raw.Content)
		if err != nil {
			return nil, err
		}
		return LiveToolResultFrame{
			Type:        raw.Type,
			ExecutionID: raw.ExecutionID,
			Content:     content,
			IsError:     raw.IsError,
			Error:       raw.Error,
		}, nil
	case "stop":
		var frame LiveStopFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil, err
		}
		return frame, nil
	case "playback_state":
		var frame LivePlaybackStateFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil, err
		}
		return frame, nil
	case "playback_mark":
		var frame LivePlaybackMarkFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil, err
		}
		return frame, nil
	default:
		return nil, fmt.Errorf("unknown live client frame type %q", typeHolder.Type)
	}
}

// UnmarshalLiveServerEvent deserializes a /v1/live server event from JSON.
func UnmarshalLiveServerEvent(data []byte) (LiveServerEvent, error) {
	var typeHolder struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typeHolder); err != nil {
		return nil, err
	}
	if strings.TrimSpace(typeHolder.Type) == "" {
		return nil, fmt.Errorf("missing live server event type")
	}

	switch typeHolder.Type {
	case "session_started":
		var event LiveSessionStartedEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "assistant_text_delta":
		var event LiveAssistantTextDeltaEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "audio_chunk":
		var event LiveAudioChunkEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "tool_call":
		var event LiveToolCallEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "user_turn_committed":
		var event LiveUserTurnCommittedEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "turn_complete":
		var event LiveTurnCompleteEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "audio_unavailable":
		var event LiveAudioUnavailableEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "audio_reset":
		var event LiveAudioResetEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "turn_cancelled":
		var event LiveTurnCancelledEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	case "error":
		var event LiveErrorEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		return event, nil
	default:
		return nil, fmt.Errorf("unknown live server event type %q", typeHolder.Type)
	}
}
