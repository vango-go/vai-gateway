package vai

import "github.com/vango-go/vai-lite/pkg/core/types"

// Type aliases for streaming events.
// These re-export the core types for SDK users.

// StreamEvent is an alias for the core types.StreamEvent interface.
type StreamEvent = types.StreamEvent

// Delta is an alias for the core types.Delta interface.
type Delta = types.Delta

// Event types
type (
	// LiveEvent is an event from /v1/live.
	LiveEvent = types.LiveServerEvent

	// LiveClientFrame is a client frame for /v1/live.
	LiveClientFrame = types.LiveClientFrame

	// MessageStartEvent is sent at the beginning of a message.
	MessageStartEvent = types.MessageStartEvent

	// ContentBlockStartEvent is sent when a new content block begins.
	ContentBlockStartEvent = types.ContentBlockStartEvent

	// ContentBlockDeltaEvent is sent for incremental content updates.
	ContentBlockDeltaEvent = types.ContentBlockDeltaEvent

	// ContentBlockStopEvent is sent when a content block is complete.
	ContentBlockStopEvent = types.ContentBlockStopEvent

	// MessageDeltaEvent contains message-level updates (stop_reason, usage).
	MessageDeltaEvent = types.MessageDeltaEvent

	// MessageStopEvent is sent when the message is complete.
	MessageStopEvent = types.MessageStopEvent

	// PingEvent is sent periodically to keep the connection alive.
	PingEvent = types.PingEvent

	// GatewayAudioChunkEvent is a gateway-emitted streaming audio payload.
	GatewayAudioChunkEvent = types.AudioChunkEvent

	// GatewayAudioUnavailableEvent signals that audio streaming is no longer available.
	GatewayAudioUnavailableEvent = types.AudioUnavailableEvent

	// ErrorEvent signals an error during streaming.
	ErrorEvent = types.ErrorEvent
)

// Delta types
type (
	// TextDelta contains incremental text content.
	TextDelta = types.TextDelta

	// InputJSONDelta contains incremental JSON for tool inputs.
	InputJSONDelta = types.InputJSONDelta

	// ThinkingDelta contains incremental thinking content.
	ThinkingDelta = types.ThinkingDelta
)

// Content block types
type (
	// TextBlock represents text content.
	TextBlock = types.TextBlock

	// ImageBlock represents image content.
	ImageBlock = types.ImageBlock

	// AudioBlock represents audio content.
	AudioBlock = types.AudioBlock

	// AudioSTTBlock represents transcribe-before-LLM audio input.
	AudioSTTBlock = types.AudioSTTBlock

	// VideoBlock represents video content.
	VideoBlock = types.VideoBlock

	// DocumentBlock represents document content.
	DocumentBlock = types.DocumentBlock

	// ToolResultBlock represents the result of a tool call.
	ToolResultBlock = types.ToolResultBlock

	// ToolUseBlock represents a tool call from the model.
	ToolUseBlock = types.ToolUseBlock

	// ThinkingBlock represents model reasoning output.
	ThinkingBlock = types.ThinkingBlock
)

// Other types
type (
	// ServerRunRequest is the request body for RunsService calls.
	ServerRunRequest = types.RunRequest

	// ServerRunConfig controls gateway-side run limits.
	ServerRunConfig = types.RunConfig

	// ServerRunStopReason indicates why a gateway-owned run ended.
	ServerRunStopReason = types.RunStopReason

	// ServerRunResultEnvelope is the blocking /v1/runs response.
	ServerRunResultEnvelope = types.RunResultEnvelope

	// ServerRunResult is the terminal server-side run result.
	ServerRunResult = types.RunResult

	// ServerRunStep captures one server-side run step.
	ServerRunStep = types.RunStep

	// ServerRunEvent is an SSE event from /v1/runs:stream.
	ServerRunEvent = types.RunStreamEvent

	// ServerRunStartEvent marks run start.
	ServerRunStartEvent = types.RunStartEvent

	// ServerRunStepStartEvent marks step start.
	ServerRunStepStartEvent = types.RunStepStartEvent

	// ServerRunStreamEventWrapper wraps nested /v1/messages stream events.
	ServerRunStreamEventWrapper = types.RunStreamEventWrapper

	// ServerRunToolCallStartEvent marks builtin tool execution start.
	ServerRunToolCallStartEvent = types.RunToolCallStartEvent

	// ServerRunToolResultEvent contains builtin tool execution output.
	ServerRunToolResultEvent = types.RunToolResultEvent

	// ServerRunStepCompleteEvent marks step completion.
	ServerRunStepCompleteEvent = types.RunStepCompleteEvent

	// ServerRunHistoryDeltaEvent carries deterministic history updates.
	ServerRunHistoryDeltaEvent = types.RunHistoryDeltaEvent

	// ServerRunCompleteEvent marks terminal successful completion.
	ServerRunCompleteEvent = types.RunCompleteEvent

	// ServerRunPingEvent is a keepalive.
	ServerRunPingEvent = types.RunPingEvent

	// ServerRunErrorEvent is the terminal error event.
	ServerRunErrorEvent = types.RunErrorEvent

	// LiveSessionStartedEvent confirms live session startup.
	LiveSessionStartedEvent = types.LiveSessionStartedEvent

	// LiveAssistantTextDeltaEvent streams assistant text in live mode.
	LiveAssistantTextDeltaEvent = types.LiveAssistantTextDeltaEvent

	// LiveAudioChunkEvent streams assistant audio in live mode.
	LiveAudioChunkEvent = types.LiveAudioChunkEvent

	// LiveToolCallEvent requests client tool execution in live mode.
	LiveToolCallEvent = types.LiveToolCallEvent

	// LiveUserTurnCommittedEvent indicates the gateway committed a live user turn.
	LiveUserTurnCommittedEvent = types.LiveUserTurnCommittedEvent

	// LiveTurnCompleteEvent indicates a live turn completed and includes synced history.
	LiveTurnCompleteEvent = types.LiveTurnCompleteEvent

	// LiveAudioUnavailableEvent indicates live audio is unavailable for the current turn.
	LiveAudioUnavailableEvent = types.LiveAudioUnavailableEvent

	// LiveAudioResetEvent indicates buffered live audio for a turn must be dropped.
	LiveAudioResetEvent = types.LiveAudioResetEvent

	// LiveTurnCancelledEvent indicates a live turn was cancelled.
	LiveTurnCancelledEvent = types.LiveTurnCancelledEvent

	// LiveErrorEvent is a live protocol/runtime error.
	LiveErrorEvent = types.LiveErrorEvent

	// LiveStartFrame starts a live session.
	LiveStartFrame = types.LiveStartFrame

	// LiveToolResultFrame sends a client tool result in live mode.
	LiveToolResultFrame = types.LiveToolResultFrame

	// LivePlaybackMarkFrame reports playback progress in live mode.
	LivePlaybackMarkFrame = types.LivePlaybackMarkFrame

	// LivePlaybackStateFrame reports playback completion/stoppage in live mode.
	LivePlaybackStateFrame = types.LivePlaybackStateFrame

	// LiveStopFrame requests graceful live session shutdown.
	LiveStopFrame = types.LiveStopFrame

	// Usage contains token counts and cost information.
	Usage = types.Usage

	// StopReason indicates why generation stopped.
	StopReason = types.StopReason

	// ToolChoice specifies how the model should choose tools.
	ToolChoice = types.ToolChoice

	// JSONSchema represents a JSON Schema for structured output.
	JSONSchema = types.JSONSchema

	// OutputFormat specifies structured output requirements.
	OutputFormat = types.OutputFormat

	// OutputConfig specifies multimodal output settings.
	OutputConfig = types.OutputConfig

	// VoiceConfig configures STT/TTS processing.
	VoiceConfig = types.VoiceConfig

	// VoiceOutputConfig configures text-to-speech options.
	VoiceOutputConfig = types.VoiceOutputConfig
)

// Stop reason constants
const (
	StopReasonEndTurn      = types.StopReasonEndTurn
	StopReasonMaxTokens    = types.StopReasonMaxTokens
	StopReasonStopSequence = types.StopReasonStopSequence
	StopReasonToolUse      = types.StopReasonToolUse
)
