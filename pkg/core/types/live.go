package types

// LiveClientFrame is a marker interface for /v1/live websocket client frames.
type LiveClientFrame interface {
	LiveClientFrameType() string
}

// LiveServerEvent is a marker interface for /v1/live websocket server events.
type LiveServerEvent interface {
	LiveServerEventType() string
}

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

// LiveSessionStartedEvent confirms session startup and fixed audio contracts.
type LiveSessionStartedEvent struct {
	Type              string `json:"type"` // "session_started"
	InputFormat       string `json:"input_format"`
	InputSampleRateHz int    `json:"input_sample_rate_hz"`
	OutputFormat      string `json:"output_format"`
	OutputSampleRateHz int   `json:"output_sample_rate_hz"`
	SilenceCommitMS   int    `json:"silence_commit_ms"`
}

func (e LiveSessionStartedEvent) LiveServerEventType() string { return "session_started" }

// LiveAssistantTextDeltaEvent streams assistant text output.
type LiveAssistantTextDeltaEvent struct {
	Type string `json:"type"` // "assistant_text_delta"
	Text string `json:"text"`
}

func (e LiveAssistantTextDeltaEvent) LiveServerEventType() string { return "assistant_text_delta" }

// LiveTalkToUserTextDeltaEvent streams talk_to_user text output.
type LiveTalkToUserTextDeltaEvent struct {
	Type   string `json:"type"` // "talk_to_user_text_delta"
	CallID string `json:"call_id,omitempty"`
	Index  int    `json:"index"`
	Text   string `json:"text"`
}

func (e LiveTalkToUserTextDeltaEvent) LiveServerEventType() string { return "talk_to_user_text_delta" }

// LiveAudioChunkEvent streams synthesized audio chunks for talk_to_user output.
type LiveAudioChunkEvent struct {
	Type         string `json:"type"` // "audio_chunk"
	Format       string `json:"format"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Audio        string `json:"audio"` // base64
	IsFinal      bool   `json:"is_final,omitempty"`
}

func (e LiveAudioChunkEvent) LiveServerEventType() string { return "audio_chunk" }

// LiveToolCallEvent requests client execution for an arbitrary function tool.
type LiveToolCallEvent struct {
	Type        string         `json:"type"` // "tool_call"
	ExecutionID string         `json:"execution_id"`
	Name        string         `json:"name"`
	Input       map[string]any `json:"input"`
}

func (e LiveToolCallEvent) LiveServerEventType() string { return "tool_call" }

// LiveUserTurnCommittedEvent indicates an utterance has been committed and sent as audio_stt.
type LiveUserTurnCommittedEvent struct {
	Type       string `json:"type"` // "user_turn_committed"
	AudioBytes int    `json:"audio_bytes"`
}

func (e LiveUserTurnCommittedEvent) LiveServerEventType() string { return "user_turn_committed" }

// LiveTurnCompleteEvent indicates a turn has completed and includes synced history.
type LiveTurnCompleteEvent struct {
	Type       string        `json:"type"` // "turn_complete"
	StopReason RunStopReason `json:"stop_reason"`
	History    []Message     `json:"history"`
}

func (e LiveTurnCompleteEvent) LiveServerEventType() string { return "turn_complete" }

// LiveAudioUnavailableEvent indicates voice output is unavailable for the current response.
type LiveAudioUnavailableEvent struct {
	Type    string `json:"type"` // "audio_unavailable"
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e LiveAudioUnavailableEvent) LiveServerEventType() string { return "audio_unavailable" }

// LiveErrorEvent reports protocol/runtime errors over /v1/live.
type LiveErrorEvent struct {
	Type    string `json:"type"` // "error"
	Fatal   bool   `json:"fatal"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (e LiveErrorEvent) LiveServerEventType() string { return "error" }
