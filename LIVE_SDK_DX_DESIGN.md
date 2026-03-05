# Live SDK DX Design

This document defines the proper Go SDK design for live audio mode in `vai-lite`.

It is grounded in the current implementation:

- `sdk/` already provides `Messages.RunStream` and `Runs.Stream`
- `pkg/core/types/live.go` already defines the current live wire types
- `pkg/gateway/handlers/live.go` already implements `GET /v1/live`
- `cmd/proxy-chatbot/main.go` and `cmd/proxy-chatbot/live_mode.go` already demonstrate the two voice UX shapes:
  - push-to-talk single-turn voice (`/st` / `/sp`)
  - long-lived live session mode (`/live`)

The goal of this design is to make live mode feel like `RunStream` extended for realistic voice interaction, without collapsing two materially different transports into one API.

---

## 1. Design Summary

The SDK should expose **three** distinct but interoperable interaction modes:

1. `Messages.RunStream`
   - The core turn-based streaming API
   - Works for text-first sessions
   - Works for push-to-talk voice turns

2. `Runs.Stream`
   - The gateway-side server loop API over SSE
   - Useful when the gateway should own tool execution

3. `Live.Connect`
   - A long-lived bidirectional session API for `/v1/live`
   - Used for full-duplex conversational voice mode

The key product principle is:

> Live mode is not a flag on `RunStream`; it is a separate session transport that should mirror `RunStream` output semantics wherever possible.

That means:

- separate API surface
- shared message history type
- shared assistant-output callback model
- distinct raw event streams

---

## 2. Mental Model

### 2.1 The two voice UX shapes

There are two valid voice experiences in this repo and they should remain distinct:

#### A. Push-to-talk voice turn

This is what the demo does with `/st` and `/sp`.

Flow:

1. record local audio
2. send one `audio_stt` user turn through `Messages.RunStream`
3. optionally receive streamed TTS output
4. stop

This remains a `RunStream` use case.

#### B. Full live conversation

This is what the demo does with `/live`.

Flow:

1. open a long-lived WebSocket session
2. continuously stream mic audio upstream
3. receive committed turns, text deltas, audio chunks, tool calls, resets, and synced history
4. report playback progress back to the gateway
5. close and resume typed or push-to-talk mode later

This is a `Live.Connect` use case.

### 2.2 What should be shared

The following should feel the same across `RunStream` and live mode:

- assistant text delta handling
- assistant audio chunk handling
- tool callback handling
- authoritative message history handoff

### 2.3 What should remain different

The following should remain explicit live-only concepts:

- binary upstream audio send
- playback marks and playback state reports
- session startup / shutdown
- turn commit notifications
- `audio_reset`
- `turn_cancelled`

---

## 3. Core Design Decisions

### 3.1 Do not make live mode a setting on `RunStream`

This is the most important decision.

`RunStream` is a finite request/response loop:

- one input request
- one streaming output sequence
- one terminal result

Live mode is a long-lived session:

- start frame plus ongoing client input after connect
- binary upstream audio
- client playback acknowledgements
- repeated committed turns within one session
- server-owned session history that evolves over time

Making live mode a `RunStream` setting would hide real protocol differences and produce a confusing API.

### 3.2 Add a separate `Client.Live` service

The public SDK should grow:

```go
type Client struct {
	Messages *MessagesService
	Runs     *RunsService
	Live     *LiveService
}
```

This service is the proper home for the current `/v1/live` protocol.

### 3.3 Unify at the callback/output layer, not the raw event type layer

`RunStream` and live mode should **not** share one raw event interface.

Reason:

- `RunStream` emits turn-loop and wrapped provider SSE events
- live mode emits session events and live-only control events

The correct consolidation point is a shared assistant-output processing layer:

- text delta
- audio chunk
- audio unavailable
- tool execution lifecycle
- synced history notifications

This allows demos and applications to reuse their rendering and tool-handling logic without pretending the transports are identical.

### 3.4 `[]Message` is the canonical cross-mode history type

This is already the correct seam in the repo and should remain so.

All mode switching should happen through `[]vai.Message`.

That gives users an easy rule:

> To switch modes, take the latest authoritative history from the current mode and seed it into the next mode.

No new history abstraction should be added unless there is a concrete problem that `[]Message` cannot solve.

---

## 4. Public Go SDK Design

## 4.1 `LiveService`

```go
type LiveService struct {
	client *Client
}
```

Primary entrypoint:

```go
func (s *LiveService) Connect(
	ctx context.Context,
	req *LiveConnectRequest,
	opts *LiveConnectOptions,
) (*LiveSession, error)
```

`Connect` is the first and only required public live SDK method in v1.

There should be no `Live.RunStream()` in the first SDK version.

Reason:

- `Connect` is enough to expose the current wire protocol cleanly
- the high-level live abstraction should follow real usage, not precede it
- the demo logic can be migrated onto this surface with very little invention

## 4.2 `LiveConnectRequest`

The SDK request should intentionally mirror the current gateway `run_request` shape:

```go
type LiveConnectRequest struct {
	Request          MessageRequest `json:"request"`
	Run              ServerRunConfig `json:"run,omitempty"`
	ServerTools      []string       `json:"server_tools,omitempty"`
	ServerToolConfig map[string]any `json:"server_tool_config,omitempty"`
	Builtins         []string       `json:"builtins,omitempty"` // deprecated compatibility alias
}
```

Design notes:

- This mirrors `types.RunRequest` on purpose.
- `Request.Messages` carries prior session history.
- `Request.Voice.Output` is required for live mode.
- `Builtins` remains present only as a compatibility alias because the gateway accepts it today.

This structure keeps mode switching simple because the same history slice can be dropped directly into `Request.Messages`.

## 4.3 `LiveConnectOptions`

The SDK needs a place for local-only behavior that is not serialized onto the wire.

```go
type LiveConnectOptions struct {
	Tools        []ToolWithHandler
	ToolHandlers map[string]ToolHandler
}
```

Semantics:

- `Tools` is the ergonomic path for client function tools
- `ToolHandlers` is the low-level path
- these are used only for responding to `tool_call`
- they are not serialized into the wire request

Why this is a struct instead of variadic options:

- avoids naming collisions with existing `RunOption` helpers
- keeps live-only configuration explicit
- makes the difference between serialized request data and local callback behavior obvious

## 4.4 `LiveSession`

```go
type LiveSession struct {
	// internal fields hidden
}
```

Required methods:

```go
func (s *LiveSession) Events() <-chan LiveEvent
func (s *LiveSession) Done() <-chan struct{}
func (s *LiveSession) Err() error
func (s *LiveSession) Close() error

func (s *LiveSession) SendFrame(frame LiveClientFrame) error
func (s *LiveSession) SendAudio(pcm []byte) error
func (s *LiveSession) SendToolResult(
	executionID string,
	content []ContentBlock,
	isError bool,
	err any,
) error
func (s *LiveSession) SendPlaybackMark(turnID string, playedMS int) error
func (s *LiveSession) SendPlaybackState(turnID string, state LivePlaybackState) error

func (s *LiveSession) HistorySnapshot() []Message
```

Design notes:

- `SendFrame` gives low-level completeness for the current wire protocol
- convenience send methods cover the common client responsibilities
- `HistorySnapshot()` returns the latest authoritative completed-session history known to the SDK

`HistorySnapshot()` must be defined carefully:

- it reflects only synced history from completed live turns
- it must not invent partial history for an in-flight turn
- it is the value callers use to leave live mode and resume `RunStream`

## 4.5 Live type exports

The SDK should re-export the current live wire types from `pkg/core/types/live.go` the same way it already re-exports stream and run types.

Expected aliases:

```go
type LiveEvent = types.LiveServerEvent
type LiveClientFrame = types.LiveClientFrame

type LiveSessionStartedEvent = types.LiveSessionStartedEvent
type LiveAssistantTextDeltaEvent = types.LiveAssistantTextDeltaEvent
type LiveAudioChunkEvent = types.LiveAudioChunkEvent
type LiveToolCallEvent = types.LiveToolCallEvent
type LiveUserTurnCommittedEvent = types.LiveUserTurnCommittedEvent
type LiveTurnCompleteEvent = types.LiveTurnCompleteEvent
type LiveAudioUnavailableEvent = types.LiveAudioUnavailableEvent
type LiveAudioResetEvent = types.LiveAudioResetEvent
type LiveTurnCancelledEvent = types.LiveTurnCancelledEvent
type LiveErrorEvent = types.LiveErrorEvent

type LiveStartFrame = types.LiveStartFrame
type LiveToolResultFrame = types.LiveToolResultFrame
type LivePlaybackMarkFrame = types.LivePlaybackMarkFrame
type LivePlaybackStateFrame = types.LivePlaybackStateFrame
type LiveStopFrame = types.LiveStopFrame
```

Also add:

```go
type LivePlaybackState string

const (
	LivePlaybackStateFinished LivePlaybackState = "finished"
	LivePlaybackStateStopped  LivePlaybackState = "stopped"
)
```

---

## 5. Shared Event Handling DX

## 5.1 Reuse `StreamCallbacks` as the common callback surface

The SDK already has a good assistant-output callback type:

- `OnTextDelta`
- `OnAudioChunk`
- `OnAudioUnavailable`
- `OnToolCallStart`
- `OnToolResult`

That is the right base for shared handling across `RunStream` and live mode.

The design is:

1. keep `RunStream.Process(callbacks StreamCallbacks)` as-is
2. add `LiveSession.Process(callbacks StreamCallbacks) error`
3. extend `StreamCallbacks` with optional live-only hooks

Additional live-only hooks:

```go
type StreamCallbacks struct {
	// existing fields...

	OnSessionStarted   func(LiveSessionStartedEvent)
	OnUserTurnCommitted func(turnID string, audioBytes int)
	OnTurnComplete     func(turnID string, stopReason ServerRunStopReason, history []Message)
	OnTurnCancelled    func(turnID, reason string)
	OnAudioReset       func(turnID, reason string)
}
```

Mapping rules:

- live `assistant_text_delta` -> `OnTextDelta`
- live `audio_chunk` -> `OnAudioChunk`
- live `audio_unavailable` -> `OnAudioUnavailable`
- live client tool auto-execution -> `OnToolCallStart` / `OnToolResult`
- live `session_started` -> `OnSessionStarted`
- live `user_turn_committed` -> `OnUserTurnCommitted`
- live `turn_complete` -> `OnTurnComplete`
- live `turn_cancelled` -> `OnTurnCancelled`
- live `audio_reset` -> `OnAudioReset`

This gives applications one shared event-handling style without forcing one shared raw event model.

## 5.2 Auto tool execution in live sessions

If `LiveConnectOptions` includes tool handlers, the session should automatically:

1. receive `tool_call`
2. decode the input
3. invoke the local handler
4. send `tool_result`
5. fire `OnToolCallStart` / `OnToolResult`

This mirrors the ergonomic role that `WithTools(...)` already plays for `RunStream`.

The live session should still expose raw events via `Events()` for users who want to manage tools manually.

## 5.3 Do not unify raw events

There should be no attempt to create a single `Event` union that both `RunStream.Events()` and `LiveSession.Events()` use.

That would either:

- erase important protocol differences, or
- create an overly broad and confusing event model

The correct DX is:

- raw events remain transport-specific
- callbacks and history handoff are shared

---

## 6. History Handoff and Mode Switching

## 6.1 Switching from `RunStream` to live mode

The handoff should be:

```go
history := run.Result().Messages

session, err := client.Live.Connect(ctx, &vai.LiveConnectRequest{
	Request: vai.MessageRequest{
		Model:    model,
		Messages: history,
		Voice:    vai.VoiceOutput(voiceID, vai.WithAudioFormat(vai.AudioFormatPCM)),
	},
	Run: vai.ServerRunConfig{
		MaxTurns:      8,
		MaxToolCalls:  20,
		TimeoutMS:     60000,
		ParallelTools: true,
		ToolTimeoutMS: 30000,
	},
}, nil)
```

This must work without any history conversion helper.

## 6.2 Switching from live mode back to `RunStream`

The handoff should be:

```go
history := session.HistorySnapshot()

stream, err := client.Messages.RunStream(ctx, &vai.MessageRequest{
	Model:    model,
	Messages: history,
	MaxTokens: 512,
})
```

The semantics are:

- `HistorySnapshot()` is authoritative
- it includes only completed live turns
- if the user leaves live mode mid-turn, the next `RunStream` request starts from the last committed history

## 6.3 Shared history ownership rule

The SDK should not create a new higher-level session history object.

Users should own a single `[]Message` history value in their application state.

Recommended rule:

- after each typed or push-to-talk `RunStream`, replace history with `result.Messages`
- after each live session turn completes or when the live session ends, replace history with `session.HistorySnapshot()`

This keeps mode switching explicit and easy to reason about.

## 6.4 Live turn completion semantics

The live session should treat `turn_complete.history` as authoritative.

It should:

- update internal history snapshot immediately
- expose the updated history through `HistorySnapshot()`
- pass the new history to `OnTurnComplete`

That makes the live session the same kind of history authority that `RunResult.Messages` already is for `RunStream`.

---

## 7. Relationship to the Demo

The current demo structure is correct and should remain the product guide for SDK boundaries.

### 7.1 `/st` and `/sp` stay on `RunStream`

The push-to-talk demo should continue to use:

- local mic capture
- `audio_stt`
- `Messages.RunStream`
- optional streamed TTS playback

This is simpler than live mode and covers a real UX.

### 7.2 `/live` moves onto the new SDK surface

The logic in `cmd/proxy-chatbot/live_mode.go` should be treated as the extraction target for the first live SDK version.

What should move into `sdk/live.go`:

- WebSocket connection and header setup
- `start` frame send
- `session_started` decode
- typed event decode and dispatch
- tool call auto-execution
- playback mark/state send helpers
- history snapshot maintenance

What should stay outside the core SDK:

- microphone capture
- speaker playback
- barge-in heuristics based on local audio levels
- terminal UI

That boundary keeps the core SDK transport-focused and reusable.

---

## 8. Non-Goals

The following are explicitly out of scope for this SDK design:

- turning live mode into a `RunStream` boolean
- unifying raw live and run events into one catch-all event type
- adding microphone or speaker drivers to the core SDK
- inventing a new history abstraction beyond `[]Message`
- adding a high-level `Live.RunStream()` wrapper in the first pass
- changing the current live wire protocol from `start` / `session_started`

---

## 9. Recommended Delivery Order

### Phase 1: Low-level public live client

Add:

- `Client.Live`
- `LiveService.Connect`
- `LiveSession`
- live type aliases
- live frame send helpers
- history snapshot support

Goal:

- everything in `cmd/proxy-chatbot/live_mode.go` can be rebuilt on public SDK APIs

### Phase 2: Shared callback processing

Add:

- `LiveSession.Process(callbacks StreamCallbacks) error`
- live-specific optional callback hooks
- shared callback-to-event mapping

Goal:

- typed voice mode and live mode can share one assistant-output rendering path

### Phase 3: Demo refactor

Move the demo to:

- keep `/st` and `/sp` on `Messages.RunStream`
- move `/live` to `client.Live.Connect`
- share history between modes with one `[]Message` variable

Goal:

- the demo becomes the proof that mode switching is simple and natural

### Phase 4: Re-evaluate high-level live convenience

Only after real use:

- decide whether a higher-level `Live.RunStream()` or similar convenience API is still needed

Default answer today:

- not yet

---

## 10. Final API Position

The proper SDK DX is:

- `Messages.RunStream` for turn-based typed and push-to-talk interactions
- `Live.Connect` for full live conversations
- shared `[]Message` history between both
- shared callback semantics for assistant output
- separate raw event streams per transport

This gives users a clean mental model:

- same conversation
- same history
- same rendering logic
- different transport depending on whether the interaction is turn-based or live

That is the correct consolidation level for `vai-lite`.
