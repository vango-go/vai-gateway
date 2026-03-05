# Developer Guide (vai-lite)

`vai-lite` is a trimmed-down Go SDK for tool-using LLM agents with two execution modes:

- **Direct mode** (default): SDK calls providers directly.
- **Proxy mode**: SDK calls VAI Gateway endpoints (`/v1/messages`, `/v1/runs`, `/v1/runs:stream`) via `WithBaseURL(...)`.

Most examples use `panic(err)` for brevity. In production code, handle errors explicitly and consider retries/backoff for transient provider failures.

The high-level primitives are:

- Single-turn:
  - `Messages.Create()` — one request/response turn
  - `Messages.Stream()` / `Messages.CreateStream()` — one streaming turn
- Agent loop:
  - `Messages.Run()` — blocking tool-loop execution
  - `Messages.RunStream()` — streaming tool-loop execution with interrupts/cancel and deterministic history deltas
- Gateway server loop:
  - `Runs.Create()` — blocking gateway-side tool loop
  - `Runs.Stream()` — streaming gateway-side tool loop over SSE
- Live session:
  - `Live.Connect()` — proxy-only `/v1/live` WebSocket session with typed events, client tool handling, and authoritative history sync

`Run`/`RunStream` are still the core agent APIs; the single-turn methods are useful when you don’t want loop orchestration.

Everything else is in service of these primitives:

- Multi-provider routing via model strings like `anthropic/claude-sonnet-4`
- A canonical request/response format based on **Anthropic Messages API** (messages + typed content blocks)
- A tools system with:
  - “native tools” normalization (e.g. `web_search`, `code_execution`) that providers execute
  - “function tools” that **your process executes** (via `MakeTool`, `FuncAsTool`, `ToolSet`, handlers)

This repo intentionally **does not** include:

- Standalone audio service endpoints (`client.Audio.*`)

---

## Table of Contents

- [1. Repository Layout](#1-repository-layout)
- [2. Installation and Requirements](#2-installation-and-requirements)
- [3. Provider Authentication](#3-provider-authentication)
  - [3.1 OpenAI-Compatible Chat Providers](#31-openai-compatible-chat-providers)
- [4. Mental Model: Request → Provider → Tool Loop](#4-mental-model-request--provider--tool-loop)
- [5. Core Types](#5-core-types)
  - [5.1 Model strings](#51-model-strings)
  - [5.2 Messages + content blocks](#52-messages--content-blocks)
  - [5.3 Tools and tool choice](#53-tools-and-tool-choice)
  - [5.4 OutputFormat (structured output)](#54-outputformat-structured-output)
  - [5.5 VoiceConfig (Cartesia non-live)](#55-voiceconfig-cartesia-non-live)
- [6. The Tool Loop](#6-the-tool-loop)
  - [6.1 Run](#61-run)
  - [6.2 RunStream](#62-runstream)
  - [6.3 Stop conditions](#63-stop-conditions)
  - [6.4 Tool execution details](#64-tool-execution-details)
  - [6.5 Timeouts, cancellation, and interrupts](#65-timeouts-cancellation-and-interrupts)
  - [6.6 Deterministic history](#66-deterministic-history)
- [7. Defining Tools (Function Tools)](#7-defining-tools-function-tools)
  - [7.1 `MakeTool` (recommended)](#71-maketool-recommended)
  - [7.2 `FuncAsTool` (returns tool + handler)](#72-funcastool-returns-tool--handler)
  - [7.3 `ToolSet` (group tools + handlers)](#73-toolset-group-tools--handlers)
  - [7.4 Manual `WithToolHandler`](#74-manual-withtoolhandler)
  - [7.5 Output shaping: returning content blocks](#75-output-shaping-returning-content-blocks)
- [8. Native Tools (Provider-Executed)](#8-native-tools-provider-executed)
- [9. Streaming Events (RunStream)](#9-streaming-events-runstream)
  - [9.1 Event model](#91-event-model)
  - [9.2 Helper extractors](#92-helper-extractors)
  - [9.3 `RunStream.Process` convenience](#93-runstreamprocess-convenience)
  - [9.4 `Messages.Stream` audio side channel](#94-messagesstream-audio-side-channel)
  - [9.5 Live SDK + event helpers](#95-live-sdk--event-helpers)
- [10. Errors and Observability](#10-errors-and-observability)
- [11. Testing and Local Dev](#11-testing-and-local-dev)
  - [11.1 Integration provider filtering + reliability controls](#111-integration-provider-filtering--reliability-controls)
  - [11.2 CI release gate workflow](#112-ci-release-gate-workflow)
- [12. Gotchas and Design Notes](#12-gotchas-and-design-notes)
- [13. Live Audio Mode (Gateway WebSocket)](#13-live-audio-mode-gateway-websocket)
  - [13.1 Current Status](#131-current-status)
  - [13.2 Public SDK Surface](#132-public-sdk-surface)
  - [13.3 Startup and Authentication](#133-startup-and-authentication)
  - [13.4 Client Frames](#134-client-frames)
  - [13.5 Server Events](#135-server-events)
  - [13.6 Session Processing and Helper Layer](#136-session-processing-and-helper-layer)
  - [13.7 Turn Semantics](#137-turn-semantics)
  - [13.8 Current Limits and Reference Client](#138-current-limits-and-reference-client)

---

## 1. Repository Layout

Key directories you’ll touch:

- `sdk/` — The public SDK you import (`github.com/vango-go/vai-lite/sdk`)
  - `sdk/client.go` — client construction, mode switch (direct vs proxy), provider registration
  - `sdk/messages.go` — `Create` / `Stream` / `CreateStream` / `Extract` / `Run` / `RunStream`
  - `sdk/runs_service.go` — gateway server-run surface: `Runs.Create` / `Runs.Stream`
  - `sdk/run.go` — the tool loop implementation
  - `sdk/tools.go` — tool builders (`MakeTool`, `ToolSet`, native tool constructors)
  - `sdk/stream.go` — stream wrapper that accumulates final responses
  - `sdk/stream_helpers.go` — helper callbacks + extractor utilities for RunStream events
  - `sdk/live.go` — proxy-only live session SDK for `/v1/live`
  - `sdk/live_helpers.go` — reusable client-side live turn tracking and playback reporting helpers
  - `sdk/tool_arg_decoder.go` — incremental decoder for streamed `input_json_delta` string fields (`content` / `message` / `text`)
  - `sdk/errors.go` — SDK error aliases, `TransportError`, and `FormatError(err)` rich rendering
  - `sdk/content.go` — content-block constructors (text/image/video/document/tool_result)
  - `sdk/schema.go` — schema generation helpers for function tools / structured output
- `pkg/core/` — shared core types + providers (SDK uses these directly)
  - `pkg/core/engine.go` — provider registry + `provider/model` routing
  - `pkg/core/types/*` — canonical API types (requests, responses, blocks, tools, streaming events)
  - `pkg/core/providers/*` — provider implementations (anthropic/openai/gem/groq/etc.)
  - `pkg/core/sseframe/parser.go` — shared SSE frame parser used by streaming providers
  - `pkg/core/errorfmt/format.go` — shared rich error formatter used by SDK + CLIs

---

## 2. Installation and Requirements

### Go version

`go.mod` currently targets `go 1.24.7`.

### Add to your project

```bash
go get github.com/vango-go/vai-lite/sdk
```

### Import path

```go
import vai "github.com/vango-go/vai-lite/sdk"
```

---

## 3. Provider Authentication

In direct mode, `vai-lite` calls providers directly, so **your process** must have provider keys available.

In proxy mode, `WithProviderKey(...)` maps to gateway `X-Provider-Key-*` headers; `WithGatewayAPIKey(...)` sets `Authorization: Bearer ...` for gateway auth.

### Environment variables

The core engine loads keys from environment variables in the form:

- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `GROQ_API_KEY`
- `CEREBRAS_API_KEY`
- `OPENROUTER_API_KEY`
- `GEMINI_API_KEY` (for `gem-dev`, also accepts `GOOGLE_API_KEY` as a fallback)
- `VERTEXAI_API_KEY` (for `gem-vert`)
- `CARTESIA_API_KEY` (for non-live STT/TTS voice mode)

Example:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
```

### Overriding keys programmatically

You can override environment keys by passing `WithProviderKey`:

```go
client := vai.NewClient(
	vai.WithProviderKey("anthropic", "sk-ant-..."),
)
```

### Proxy mode configuration

```go
client := vai.NewClient(
	vai.WithBaseURL("https://api.vai.example.com"),
	vai.WithGatewayAPIKey("vai_sk_..."), // optional for self-host auth_mode=disabled
	vai.WithProviderKey("anthropic", "sk-ant-..."),
	vai.WithProviderKey("cartesia", "sk-cartesia-..."), // sent for voice requests
)
```

Proxy mode notes:
- `Messages.Run` / `Messages.RunStream` remain client-side loops.
- `Runs.Create` / `Runs.Stream` call gateway server-side loops.
- Server-side runs reject client function tools (use provider-native tools and/or gateway-managed **server tools** instead).
- Gateway server tools are enabled via `server_tools` + `server_tool_config` (legacy `builtins` remains accepted as a compatibility alias).
- The gateway also exposes `GET /v1/live`, and the Go SDK provides `Client.Live.Connect(...)` for proxy-mode live sessions.
- Live sessions use the same `[]vai.Message` history type as `RunStream`, so callers can move between typed/push-to-talk/live modes without a conversion layer.
- For non-streaming proxy calls, pass request contexts with explicit deadlines in production.

### 3.1 OpenAI-Compatible Chat Providers

`openai`, `groq`, `cerebras`, and `openrouter` all use the same Chat Completions translation layer in `pkg/core/providers/openai`.
Wrappers differ primarily by base URL, capabilities, and compatibility options.

OpenAI provider options used by compatibility wrappers:

- `WithResponseModelPrefix("...")` — controls normalized response model prefix (for example `groq/...`).
- `WithMaxTokensField(...)` — choose `max_tokens` or `max_completion_tokens`.
- `WithStreamIncludeUsage(bool)` — toggle `stream_options.include_usage` in streaming requests.
- `WithChatCompletionsPath("...")` — override endpoint path under the configured base URL.
- `WithAuth(...)` and `WithExtraHeader(s)` — customize auth/header behavior for OpenAI-compatible gateways.

Current defaults in-tree:

- `openai` uses `max_completion_tokens` and emits `openai/<model>` in normalized responses.
- `groq` and `cerebras` use `max_tokens` and emit `groq/<model>` / `cerebras/<model>`.
- `openrouter` uses `max_tokens`, emits `openrouter/<model>`, and supports optional attribution headers via `openrouter.WithSiteURL(...)` / `openrouter.WithSiteName(...)`.

### Gemini providers

Gemini is split into two providers:

- `gem-dev` (Gemini Developer API backend; key from `GEMINI_API_KEY`, fallback `GOOGLE_API_KEY`)
- `gem-vert` (Vertex AI backend; key from `VERTEXAI_API_KEY`)

---

## 4. Mental Model: Request → Provider → Tool Loop

At a high level:

1. You create a `Client`.
2. You build a `MessageRequest` (model + messages + tools + options).
3. You run an agent loop using:
   - `Messages.Run()` (blocking) or
   - `Messages.RunStream()` (streaming)
4. The model responds with:
   - text/thinking, and possibly
   - tool calls (`tool_use` blocks)
5. `vai-lite` executes tool calls *that have registered handlers* (client-side “function tools”).
6. Tool results are injected back into the conversation as `tool_result` blocks and the loop continues until a stop condition is met.

Native tools (like `web_search`) are **executed by the provider**. You still see tool-ish blocks/events, but there is no local handler to register.

---

## 5. Core Types

Most public types are re-exported aliases around `pkg/core/types`.

### 5.1 Model strings

All model IDs are of the form:

```
provider/model-name
```

Examples:

- `anthropic/claude-sonnet-4`
- `openai/gpt-4o`
- `oai-resp/gpt-4o` (OpenAI Responses API provider in this repo; name is provider-specific)
- `groq/llama-3.3-70b`
- `openrouter/openai/gpt-4o`
- `gem-dev/gemini-2.5-flash`
- `gem-vert/gemini-3-flash-preview`
- `cerebras/llama-3.1-8b`

Routing happens in `pkg/core/engine.go` by splitting on the first `/`.

### 5.2 Messages + content blocks

The canonical request format is Anthropic-style:

```go
req := &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{
			Role: "user",
			Content: []vai.ContentBlock{
				vai.Text("What's in this image?"),
				vai.ImageURL("https://example.com/cat.jpg"),
			},
		},
	},
}
```

Notes:

- `Message.Content` can be:
  - a `string`, or
  - a `[]ContentBlock`
- `vai.Text(...)` returns a `ContentBlock` (a typed block).
- `vai.Audio(bytes, mediaType)` creates input audio blocks.
- `audio_stt` blocks are transcribed via Cartesia STT before LLM invocation.
- When `req.Voice.Output` is set, output text is synthesized to audio via Cartesia TTS.

### 5.3 Tools and tool choice

Tools live on `MessageRequest.Tools`:

```go
req.Tools = []vai.Tool{
	vai.WebSearch(),
	vai.CodeExecution(),
}
```

For function tools created via `MakeTool(...)`, the recommended pattern is to enable them via `WithTools(tool)`, which:

- registers the handler, and
- automatically attaches the tool definition for that run/stream.

```go
	tool := vai.MakeTool("get_weather", "Get weather for a location", func(ctx context.Context, in struct {
		Location string `json:"location"`
	}) (string, error) {
		return "72F and sunny in " + in.Location, nil
	})

	result, err := client.Messages.Run(ctx, &vai.MessageRequest{
		Model: "openai/gpt-4o",
		Messages: []vai.Message{
			{Role: "user", Content: vai.Text("What's the weather in Austin?")},
		},
		Tools: []vai.Tool{
			vai.WebSearch(),
			vai.CodeExecution(),
		},
	}, vai.WithTools(tool))
	if err != nil {
		panic(err)
	}

fmt.Println(result.Response.TextContent())
```

Tool selection policy:

```go
req.ToolChoice = vai.ToolChoiceAuto() // model decides
// req.ToolChoice = vai.ToolChoiceAny()
// req.ToolChoice = vai.ToolChoiceNone()
// req.ToolChoice = vai.ToolChoiceTool("some_tool_name")
```

### 5.4 OutputFormat (structured output)

You can request JSON schema structured output by setting:

```go
schema := vai.SchemaFromStruct[struct {
	Person  string `json:"person" desc:"Person name"`
	Role    string `json:"role"`
	Company string `json:"company"`
}]()

req.OutputFormat = &vai.OutputFormat{
	Type:       "json_schema",
	JSONSchema: schema,
}
```

Important:

- `Messages.Extract(ctx, req, &dest)` executes the request and unmarshals JSON into your struct.
- `ExtractTyped[T](ctx, client.Messages, req)` is the generic helper that returns `(T, *Response, error)`.
- If `req.OutputFormat` is nil, `Extract` injects a JSON schema generated from your destination struct type.
- If `req.OutputFormat` is already set, `Extract` preserves it.
- Structured-output behavior is provider/model-specific.

### 5.5 VoiceConfig (Cartesia non-live)

`MessageRequest` supports top-level voice model routing plus output voice config:

```go
req := &vai.MessageRequest{
	Model: "openai/gpt-4o",
	Messages: []vai.Message{
		{
			Role: "user",
			Content: vai.ContentBlocks(
				vai.AudioSTT(wavBytes, "audio/wav", vai.WithSTTLanguage("en")),
			),
		},
	},
	STTModel: "cartesia/ink-whisper", // optional; default shown
	TTSModel: "cartesia/sonic-3",     // optional; default shown
	Voice: vai.VoiceOutput(
		"a0e99841-438c-4a64-b679-ae501e7d6091",
		vai.WithAudioFormat(vai.AudioFormatWAV), // wav/mp3/pcm
	),
}
```

Convenience helpers:

- `vai.AudioSTT(...)`
- `vai.WithSTTLanguage(...)`
- `vai.VoiceOutput(voiceID, ...)`

Semantics:

- `audio_stt` blocks are transcribed to `text` before model execution.
- Raw `audio` blocks are never auto-transcribed; they remain raw multimodal input.
- Output synthesis is opt-in via `req.Voice.Output`.
- `voice.input` has been removed and is rejected (`param=voice.input`).
- Missing Cartesia configuration while STT/TTS is requested is a fail-fast error.
- `Messages.Create` and `Messages.Stream` store concatenated `audio_stt` transcript in response metadata (`user_transcript`).
- `Messages.Stream` preserves `Events()` and emits audio deltas through `AudioEvents()`.
- `RunStream` emits `AudioChunkEvent` in `Events()` and appends final audio to terminal `RunResult.Response`.

---

## 6. The Tool Loop

### 6.1 Run

`Run` is a blocking loop that keeps calling the model and executing tools until it reaches a terminal condition.

```go
client := vai.NewClient()

tool := vai.MakeTool("increment", "Increment a counter",
	func(ctx context.Context, in struct{}) (string, error) {
		return "ok", nil
	},
)

result, err := client.Messages.Run(ctx, &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Call increment once and then summarize.")},
	},
}, vai.WithTools(tool), vai.WithMaxToolCalls(5))
if err != nil {
	// err can be provider errors, context cancellation, etc.
	panic(err)
}

fmt.Println(result.Response.TextContent())
fmt.Println("Tool calls:", result.ToolCallCount)
fmt.Println("Turns:", result.TurnCount)
```

What you get back (`RunResult`):

- `Response`: final assistant response (the terminal response)
- `Steps`: per-turn steps (responses + tool calls + tool results)
- `ToolCallCount`: total tool calls across the loop
- `TurnCount`: total model turns
- `Usage`: aggregated usage across turns (provider-reported)
  - If a provider omits `total_tokens`, aggregation defensively computes `total_tokens = input_tokens + output_tokens` for that turn.
- `StopReason`: why the loop ended
- `Messages` (optional): snapshot of final message history

### 6.2 RunStream

`RunStream` runs the same loop but emits events while it happens.

```go
stream, err := client.Messages.RunStream(ctx, req,
	vai.WithTools(tool),
	vai.WithMaxToolCalls(5),
)
if err != nil {
	panic(err)
}
defer stream.Close()

for event := range stream.Events() {
	if text, ok := vai.TextDeltaFrom(event); ok {
		fmt.Print(text)
	}
}

if err := stream.Err(); err != nil {
	panic(err)
}

final := stream.Result()
fmt.Println("\nStopReason:", final.StopReason)
```

### 6.3 Stop conditions

`RunOption`s:

- Safety limits:
  - `WithMaxToolCalls(n)`
  - `WithMaxTurns(n)`
  - `WithMaxTokensRun(n)` (uses aggregated `Usage.TotalTokens`)
  - `WithRunTimeout(d)`
- Tool execution:
  - `WithParallelTools(true|false)` (default: `true`)
  - `WithToolTimeout(d)` (default: `30s`)
- Custom stop:
  - `WithStopWhen(func(*Response) bool { ... })`

Examples:

```go
result, err := client.Messages.Run(ctx, req,
	vai.WithMaxTurns(8),
	vai.WithMaxToolCalls(20),
	vai.WithRunTimeout(60*time.Second),
	vai.WithStopWhen(func(r *vai.Response) bool {
		return strings.Contains(r.TextContent(), "DONE")
	}),
)
```

### 6.4 Tool execution details

When the model requests tools (via `tool_use` blocks), the SDK:

1. Looks up a registered handler by tool name.
2. Reconstructs streamed tool inputs (`input_json_delta`) into the final tool `input` object.
3. Marshals the tool `input` object to JSON.
4. Calls your handler with `context.Context` + `json.RawMessage`.
5. Converts the returned value into `[]ContentBlock`:
   - `string` → `[{type:"text", text:"..."}]`
   - `ContentBlock` → wrapped into a slice
   - `[]ContentBlock` → used as-is
   - other types → JSON-marshaled into a `text` block
6. Injects a `tool_result` block back into the conversation.

For `RunStream` specifically:

- `ToolCallStartEvent` is emitted when local SDK tool execution begins (once per tool call) and includes the complete parsed input.
- Tool-use streaming lifecycle is available through `RunStream.Process` callbacks:
  - `OnToolUseStart(index, id, name)`
  - `OnToolInputDelta(index, id, name, partialJSON)`
  - `OnToolUseStop(index, id, name)`
- Equivalent extractor helpers exist if you consume raw events: `ToolUseStartFrom`, `ToolInputDeltaFrom`, `ToolUseStopFrom`.

You can still inspect wrapped provider events (`StreamEventWrapper`) directly when you need full raw stream payloads.

If no handler exists for a tool name, the SDK returns a tool result like:

> Tool 'X' was called but no handler is registered.

This is intentional: it lets the model see that the tool is unavailable and continue.

### 6.5 Timeouts, cancellation, and interrupts

There are three layers:

1. **Whole-run timeout** (`WithRunTimeout`)
2. **Per-tool timeout** (`WithToolTimeout`)
3. **Context cancellation** (your `ctx`)

Stop reason mapping:

- `context.DeadlineExceeded` maps to `timeout`
- `context.Canceled` maps to `cancelled`

Additionally, `RunStream` supports:

- `stream.Cancel()` — abort the current stream and terminate the run loop (StopReason becomes `cancelled`)
- `stream.Interrupt(msg, behavior)` — stop the current stream, optionally save partial output, inject a new user message, and continue

Interrupt behavior controls what happens to partial assistant output:

- `InterruptDiscard` — do not save partial output to history
- `InterruptSavePartial` — save partial output as-is
- `InterruptSaveMarked` — save partial output with a marker (`[interrupted]`)

### 6.6 Deterministic history

`RunStream` emits `HistoryDeltaEvent` events that describe exactly which messages should be appended to a caller-owned history.

This is useful when:

- you want to keep the SDK stateless and own your own history slice
- you want deterministic history updates even across interrupts

`HistoryDeltaEvent` also includes an `ExpectedLen` field. If you want mismatch detection (e.g. when you rewrite history between turns),
use `DefaultHistoryHandlerStrict` instead of `DefaultHistoryHandler`.

In general, `HistoryDeltaEvent` is emitted once per completed step (after tools run, if any) with the assistant message and (if applicable)
the `tool_result` message. If you interrupt a stream with a “save partial” behavior, the saved partial assistant message is emitted as a delta too.

Basic pattern:

```go
history := append([]vai.Message(nil), req.Messages...)

apply := vai.DefaultHistoryHandler(&history)

for ev := range stream.Events() {
	apply(ev)
}
```

Strict variant (mismatch detection):

```go
history := append([]vai.Message(nil), req.Messages...)

apply := vai.DefaultHistoryHandlerStrict(&history)
defer func() {
	if r := recover(); r != nil {
		// Strict handler panics if your history diverges from the stream's ExpectedLen.
		panic(r)
	}
}()

for ev := range stream.Events() {
	apply(ev)
}
```

For advanced context management (pinned memory, trimming, reordering), build per-turn messages at turn boundaries:

```go
stream, err := client.Messages.RunStream(ctx, req,
	vai.WithBuildTurnMessages(func(info vai.TurnInfo) []vai.Message {
		return info.History
	}),
)
if err != nil {
	panic(err)
}
defer stream.Close()
```

`WithBuildTurnMessages` affects only the messages sent to the model on the next turn. It does not change the append-only history represented by `HistoryDeltaEvent`.

---

## 7. Defining Tools (Function Tools)

Function tools are executed by **your process** during the tool loop.

### 7.1 `MakeTool` (recommended)

`MakeTool` gives you:

- a `ToolWithHandler` value containing both:
  - a `Tool` definition (`tool.Tool`)
  - a handler embedded in the returned value
- automatic JSON schema generation from the input struct type

Example:

```go
type SearchInput struct {
	Query string `json:"query" desc:"Search query"`
}

search := vai.MakeTool("search_internal", "Search an internal index.",
	func(ctx context.Context, in SearchInput) (string, error) {
		return "results for: " + in.Query, nil
	},
)

req := &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Search internal for 'incident 123'")},
	},
}

// WithTools registers the handler and attaches the tool definition for this run.
result, err := client.Messages.Run(ctx, req, vai.WithTools(search))
if err != nil {
	panic(err)
}
```

### 7.2 `FuncAsTool` (returns tool + handler)

`FuncAsTool` is a lightweight helper returning `(Tool, ToolHandler)`:

```go
tool, handler := vai.FuncAsTool("get_weather", "Get weather.",
	func(ctx context.Context, in struct {
		Location string `json:"location"`
	}) (string, error) {
		return "sunny in " + in.Location, nil
	},
)

result, err := client.Messages.Run(ctx, req,
	vai.WithToolHandler(tool.Name, handler),
)
```

### 7.3 `ToolSet` (group tools + handlers)

`ToolSet` is useful for bundling many tools:

```go
ts := vai.NewToolSet()

ts.AddNative(vai.WebSearch())

echoTool, echoHandler := vai.FuncAsTool("echo", "Echo input.",
	func(ctx context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, nil
	},
)
ts.Add(echoTool, echoHandler)

stream, err := client.Messages.RunStream(ctx, req,
	vai.WithToolSet(ts),
)
```

### 7.4 Manual `WithToolHandler`

If you want full control:

```go
client.Messages.Run(ctx, req,
	vai.WithToolHandler("tool_name", func(ctx context.Context, raw json.RawMessage) (any, error) {
		// parse raw, do work, return value
		return map[string]any{"ok": true}, nil
	}),
)
```

### 7.5 Output shaping: returning content blocks

Your handler can return:

- `string` (becomes a single text block)
- `vai.ContentBlock`
- `[]vai.ContentBlock`
- any other JSON-marshalable object (encoded into a text block)

Example returning rich blocks:

```go
return []vai.ContentBlock{
	vai.Text("Found 3 results."),
	vai.Text("1) ..."),
}, nil
```

---

## 8. Native Tools (Provider-Executed)

Native tools are represented as `Tool{Type: "...", Config: ...}` and are executed by the provider if the provider/model supports them.

SDK constructors:

- `vai.WebSearch(...)` — web search (Anthropic, OpenAI Responses, Gemini)
- `vai.WebFetch(...)` — web page content extraction (Anthropic only)
- `vai.CodeExecution(...)`
- `vai.ComputerUse(width, height)`
- `vai.TextEditor()`
- `vai.FileSearch(...)` (provider-specific)

Important:

- Native tool support is **provider/model dependent**.
- The providers in `pkg/core/providers/*` map these normalized tool types to provider-specific tool names and request formats.
- Since providers execute these tools, you generally do **not** register handlers for them.
- `WebFetch` currently maps to Anthropic's `web_fetch_20250910` only. For other providers, use `VAIWebFetch(...)` (gateway-native) or `LocalVAIWebFetch(...)` (local adapter-backed).

Native web search mapping:

| Provider | `WebSearch()` maps to | `WebFetch()` maps to |
|----------|-----------------------|----------------------|
| Anthropic | `web_search_20250305` | `web_fetch_20250910` |
| OpenAI Responses | `web_search` | _(skipped)_ |
| Gemini | `googleSearch` grounding | _(skipped)_ |
| OpenAI Chat | _(skipped)_ | _(skipped)_ |

### 8.1 VAI Web Tools: Gateway vs Local

`VAIWebSearch` / `VAIWebFetch` are now **gateway-native** tool builders:

```go
search := vai.VAIWebSearch(vai.Tavily)      // or vai.Exa
fetch := vai.VAIWebFetch(vai.Tavily)        // or vai.Firecrawl
```

These are function tools with built-in handlers that call gateway endpoint `POST /v1/server-tools:execute`.

Requirements:
- Proxy mode (`WithBaseURL(...)`)
- BYOK provider key via `WithProviderKey(...)` for the selected tool provider
- Use with `Messages.Run` / `Messages.RunStream` (the SDK loop executes the tool handler)

If you want adapter-backed execution inside your process (no gateway execution), use:
- `vai.LocalVAIWebSearch(...)`
- `vai.LocalVAIWebFetch(...)`

Example local usage:

```go
import (
    vai "github.com/vango-go/vai-lite/sdk"
    "github.com/vango-go/vai-lite/sdk/adapters/tavily"
)

search := vai.LocalVAIWebSearch(tavily.NewSearch(os.Getenv("TAVILY_API_KEY")))
fetch := vai.LocalVAIWebFetch(tavily.NewExtract(os.Getenv("TAVILY_API_KEY")))
```

Key difference:
- `vai.WebSearch()` / `vai.WebFetch()` → provider-native tools
- `vai.VAIWebSearch(vai.Provider)` / `vai.VAIWebFetch(vai.Provider)` → gateway-executed VAI tools
- `vai.LocalVAIWebSearch(...)` / `vai.LocalVAIWebFetch(...)` → local adapter-executed VAI tools

Local adapters:

| Package | Search | Fetch | Import |
|---------|--------|-------|--------|
| Tavily | `tavily.NewSearch(apiKey)` | `tavily.NewExtract(apiKey)` | `sdk/adapters/tavily` |
| Firecrawl | — | `firecrawl.NewScrape(apiKey)` | `sdk/adapters/firecrawl` |
| Exa | `exa.NewSearch(apiKey)` | `exa.NewContents(apiKey)` | `sdk/adapters/exa` |

Local custom providers implement:

```go
type WebSearchProvider interface {
    Search(ctx context.Context, query string, opts WebSearchOpts) ([]WebSearchHit, error)
}

type WebFetchProvider interface {
    Fetch(ctx context.Context, url string, opts WebFetchOpts) (*WebFetchResult, error)
}
```

Local configuration options:

```go
search := vai.LocalVAIWebSearch(tavily.NewSearch(apiKey), vai.LocalVAIWebSearchConfig{
    ToolName:       "my_search",
    MaxResults:     10,
    AllowedDomains: []string{"docs.example.com"},
    BlockedDomains: []string{"spam.site"},
})

fetch := vai.LocalVAIWebFetch(firecrawl.NewScrape(apiKey), vai.LocalVAIWebFetchConfig{
    ToolName: "my_fetch",
    Format:   "text",
})
```

DX sugar helpers for local adapters remain available: `tavily.FromEnv()`, `firecrawl.FromEnv()`, and `exa.FromEnv()` (plus `tavily.ExtractFromEnv()` / `exa.ContentsFromEnv()` for fetch-style adapters).

### 8.2 Gateway-Executed VAI-Native Web Tools (`/v1/runs`)

When you use the gateway server-side tool loop (`Runs.Create` / `Runs.Stream` → `/v1/runs`), the gateway executes tools instead of your process.

That means you cannot send a Go adapter object like `tavily.NewSearch(...)` to the gateway. Instead, you:

1. Enable a gateway server tool by name (e.g. `vai_web_search`, `vai_web_fetch`).
2. Select which web provider adapter the gateway should use (e.g. Tavily vs Exa vs Firecrawl).
3. Provide the tool provider API key as BYOK **out-of-band** (never in model-visible tool inputs).

Current wire contract:
- Request fields: `server_tools` + `server_tool_config`.
- Tool provider BYOK headers (examples): `X-Provider-Key-Tavily`, `X-Provider-Key-Exa`, `X-Provider-Key-Firecrawl`.
- Legacy alias: `builtins` (deprecated) is still accepted; if both are present they must match.

**Important:** the LLM/tool call input must never include API keys. Keys are provided to the gateway via request headers (HTTP or WebSocket upgrade) and the gateway uses them ephemerally.

---

## 9. Streaming Events (RunStream)

### 9.1 Event model

`RunStream.Events()` yields a mix of:

1. **Wrapped provider stream events** (`StreamEventWrapper`)
   - these are `pkg/core/types.StreamEvent` (e.g. `content_block_delta`, `message_delta`, `error`)
2. **Tool-loop lifecycle events**
   - `StepStartEvent`
   - `AudioChunkEvent` (when `req.Voice.Output` is enabled)
   - `ToolCallStartEvent`
   - `ToolResultEvent`
   - `StepCompleteEvent`
   - `HistoryDeltaEvent`
   - `InterruptedEvent`
   - `RunCompleteEvent`

`ToolCallStartEvent` corresponds to SDK local tool execution start (not raw provider detection), and its `Input` reflects the full parsed tool input object.
Use wrapped provider events if you need lower-level "tool call is being streamed now" signals.

Provider stream contract note:

- Consumers process any non-nil event even if `Next()` also returns `io.EOF` in the same call.
- In-tree providers normalize to emit terminal events with `nil` error and return `io.EOF` on the next call.

### 9.2 Helper extractors

Helpers in `sdk/stream_helpers.go`:

- `TextDeltaFrom(event)` — extracts text chunks from wrapped stream deltas
- `ThinkingDeltaFrom(event)` — extracts thinking chunks if present
- `AudioChunkFrom(event)` — extracts run-level audio chunks
- `ToolUseStartFrom(event)` — extracts tool-use start metadata from wrapped `content_block_start`
- `ToolInputDeltaFrom(event)` — extracts streamed `input_json_delta` partial JSON
- `ToolUseStopFrom(event)` — extracts tool-use stop indices from wrapped `content_block_stop`

Example:

```go
for ev := range stream.Events() {
	if t, ok := vai.TextDeltaFrom(ev); ok {
		fmt.Print(t)
	}
}
```

### 9.3 `RunStream.Process` convenience

`Process` consumes events and calls your callbacks:

```go
text, err := stream.Process(vai.StreamCallbacks{
	OnTextDelta: func(t string) { fmt.Print(t) },
	OnToolUseStart: func(index int, id, name string) {
		log.Printf("tool stream start idx=%d id=%s name=%s", index, id, name)
	},
	OnToolInputDelta: func(index int, id, name, partialJSON string) {
		log.Printf("tool stream delta idx=%d id=%s name=%s json=%q", index, id, name, partialJSON)
	},
	OnToolUseStop: func(index int, id, name string) {
		log.Printf("tool stream stop idx=%d id=%s name=%s", index, id, name)
	},
	OnAudioChunk: func(data []byte, format string) { play(data, format) },
	OnToolCallStart: func(id, name string, input map[string]any) {
		log.Printf("tool %s(%v)", name, input)
	},
	OnToolResult: func(id, name string, content []vai.ContentBlock, err error) {
		log.Printf("tool %s done (err=%v)", name, err)
	},
})
```

It returns the accumulated output text and any final error.

Tool-stream vs execution semantics:

- `OnToolUseStart` / `OnToolInputDelta` / `OnToolUseStop` correspond to model stream `content_block_*` tool-use blocks.
- `OnToolCallStart` / `OnToolResult` correspond to SDK local tool execution lifecycle.
- In out-of-order streams, `OnToolInputDelta` may fire before start metadata; in that case `id`/`name` are empty and index is still stable.

For TTS/captions from streamed tool args, use per-tool-index `ToolArgStringDecoder`:

```go
decoders := map[int]*vai.ToolArgStringDecoder{}

_, err = stream.Process(vai.StreamCallbacks{
	OnToolUseStart: func(index int, id, name string) {
		if name == "talk_to_user" {
			decoders[index] = vai.NewToolArgStringDecoder(vai.ToolArgStringDecoderOptions{})
		}
	},
	OnToolInputDelta: func(index int, id, name, partialJSON string) {
		decoder := decoders[index]
		if decoder == nil {
			return
		}
		update := decoder.Push(partialJSON)
		if update.Found && update.Delta != "" {
			fmt.Print(update.Delta) // live speech/captions
		}
	},
	OnToolUseStop: func(index int, id, name string) {
		delete(decoders, index)
	},
})
```

Decoder notes:

- Default key candidates are `content`, `message`, `text` (in that order).
- The decoder locks the first detected key for stable low-latency output.
- V1 scope is root-level key extraction (nested JSON paths are intentionally out of scope).

### 9.4 `Messages.Stream` audio side channel

For single-turn streaming:

- `Events()` remains `chan types.StreamEvent` (text/tool deltas unchanged).
- `AudioEvents()` emits synthesized audio chunks when `req.Voice.Output` is set.
- `Response()` includes a final `audio` content block when synthesis succeeded.
- If voice output is not enabled, `AudioEvents()` is closed immediately.
- In proxy mode, gateway-emitted `audio_chunk` events are forwarded through `AudioEvents()`; `audio_unavailable` closes the audio side channel while text streaming continues.
- TTS streaming errors are fail-fast: stream ends with `Stream.Err()`.

### 9.5 Live SDK + event helpers

The gateway implements `GET /v1/live`, and the public Go SDK now exposes it as `Client.Live.Connect(...)`.

Important boundaries:

- `Messages.RunStream` is still the turn-based API.
- `Live.Connect` is a long-lived bidirectional session API.
- Both modes share the same canonical history type: `[]vai.Message`.
- The raw event streams remain separate (`RunStreamEvent` vs `LiveEvent`), but the callback model intentionally overlaps.

Live-specific helpers currently in `sdk/`:

- `LiveCallbacks` — callback processor for `LiveSession.Process(...)`
- `LiveTextDeltaFrom(event)` — extracts assistant text deltas from a `LiveEvent`
- `LiveAudioChunkFrom(event)` — extracts assistant audio chunks from a `LiveEvent`
- `LiveTurnCompleteFrom(event)` — extracts authoritative turn-complete history sync events
- `LiveTurnTracker` — reusable client-side turn suppression helper for stale/cancelled/reset turns
- `LivePlaybackReporter` — reusable playback mark/state reporting helper

Use Section 13 for the full live SDK and protocol contract. Treat [LIVE_SDK_DX_DESIGN.md](/Users/collinshill/Documents/projects/vai-lite/LIVE_SDK_DX_DESIGN.md) as background design rationale; the code in `sdk/`, `pkg/core/types/live.go`, and `pkg/gateway/handlers/live.go` is the current source of truth.

---

## 10. Errors and Observability

### Provider errors

Provider errors are returned from underlying provider calls. The core also defines canonical error wrappers in `pkg/core/errors.go`.

Common cases:

- auth errors (missing/invalid API key)
- provider overloads / rate limits
- invalid request shapes (bad model string, invalid tool schema, etc.)

For human-readable CLI/log output, prefer:

```go
fmt.Println(vai.FormatError(err))
```

`vai.FormatError` includes rich details when available (for example `op`, `url`, `param`, `code`, `request_id`, `retry_after`, and compact `provider_error`).

### Handling errors (recommended)

General guidance:

- Use `context.Context` deadlines (`WithRunTimeout`, `WithToolTimeout`, or your `ctx`) to bound retries and prevent stuck runs.
- Retry **transient** provider errors (rate limits / overload / generic API errors) with exponential backoff + jitter.
- Avoid retrying invalid requests (bad model string, malformed tool schema, etc.) unless you modify the request.
- If you retry a whole `Run`/`RunStream`, treat tool execution as potentially non-idempotent (design tools to be idempotent if possible, or persist tool outcomes and replay them carefully).

The SDK may return `*core.Error` for provider failures, which provides a retry signal and optional `RetryAfter`:

```go
import (
	"errors"

	core "github.com/vango-go/vai-lite/pkg/core"
)

var apiErr *core.Error
if errors.As(err, &apiErr) && apiErr.IsRetryable() {
	// consider sleeping based on apiErr.RetryAfter (if set), then retry
}
```

Minimal retry loop for `Run` (illustrative):

```go
import (
	"errors"
	"math/rand"
	"time"

	core "github.com/vango-go/vai-lite/pkg/core"
)

backoff := func(attempt int) time.Duration {
	base := 200 * time.Millisecond
	max := 5 * time.Second
	d := base << attempt
	if d > max {
		d = max
	}
	// jitter in [0.5, 1.5)
	jitter := 0.5 + rand.Float64()
	return time.Duration(float64(d) * jitter)
}

for attempt := 0; attempt < 4; attempt++ {
	result, err := client.Messages.Run(ctx, req, opts...)
	if err == nil {
		_ = result
		break
	}

	var apiErr *core.Error
	if errors.As(err, &apiErr) && apiErr.IsRetryable() && attempt < 3 {
		sleepFor := backoff(attempt)
		if apiErr.RetryAfter != nil {
			sleepFor = time.Duration(*apiErr.RetryAfter) * time.Second
		}
		time.Sleep(sleepFor)
		continue
	}

	// non-retryable (or out of attempts)
	panic(err)
}
```

### Tool handler errors

If a tool handler returns an error, the SDK:

- emits a `ToolResultEvent` with `Error != nil` (RunStream)
- injects a `tool_result` block with `is_error: true`
- continues the loop (the model can respond to the tool failure)

### Logging

The SDK itself is intentionally minimal about logging. You can provide a logger with:

```go
client := vai.NewClient(vai.WithLogger(myLogger))
```

Providers may log debug messages in some cases.

For production SLOs, fallback policy, incident handling, and release criteria, see `PRODUCTION_OPERATIONS.md`.

---

## 11. Testing and Local Dev

### Running tests in restricted environments

Some environments restrict Go’s default cache dir. If `go test ./...` fails with a cache permission error, run with repo-local caches:

```bash
GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.gomodcache go test ./...
```

### Formatting

```bash
gofmt -w $(find pkg sdk -name '*.go')
```

### 11.1 Integration provider filtering + reliability controls

Integration tests support per-provider targeting:

```bash
VAI_INTEGRATION_PROVIDERS=gem-vert go test -tags=integration ./integration -count=1 -timeout=45m -v
```

`VAI_INTEGRATION_PROVIDERS` accepts a comma-separated provider list (`anthropic,oai-resp,groq,openrouter,gem-dev,gem-vert`) or `all`.

Reliability controls for capacity-prone providers:

- `VAI_INTEGRATION_RETRY_ATTEMPTS`
- `VAI_INTEGRATION_GEM_DEV_RETRY_ATTEMPTS`
- `VAI_INTEGRATION_GEM_VERT_RETRY_ATTEMPTS`
- `VAI_INTEGRATION_RETRY_BASE_MS`
- `VAI_INTEGRATION_RETRY_MAX_MS`
- `VAI_INTEGRATION_GEM_DEV_MIN_GAP_MS`
- `VAI_INTEGRATION_GEM_VERT_MIN_GAP_MS`
- `VAI_INTEGRATION_DIAGNOSTICS=1` (structured retry/classification logs)

### 11.2 CI release gate workflow

The provider matrix release gate is defined in:

- `.github/workflows/integration-release-gate.yml`

It runs tagged integration tests per provider, enforces credential presence, and uploads JSON + summary artifacts for deterministic triage.

---

## 12. Gotchas and Design Notes

### 12.1 “Only Run / RunStream” does not mean “no streaming”

`RunStream` still relies on provider streaming internally, because it’s how it:

- prints tokens as they arrive
- detects tool calls early
- supports interruption while a turn is in progress

### 12.2 Tool definitions vs handler registration

If you manually attach a function tool definition to `req.Tools`, you must also register its handler:

- `WithTools(tool)` for `MakeTool(...)`
- or `WithToolHandler(name, handler)` / `WithToolSet(...)`

If you don’t, the model will call the tool and receive the “no handler registered” result.

Recommended: prefer `WithTools(...)` / `WithToolSet(...)` for function tools, since they both register handlers and attach tool definitions for the run/stream.

### 12.3 Structured output helper behavior

`Messages.Extract(...)` first attempts to parse `Response.TextContent()` as direct JSON. If that fails, it tries common fenced JSON blocks (for example, ```json ... ```). If no parseable JSON is found, it returns an explicit error.

### 12.4 Native tool behavior depends on the provider

Even though `vai-lite` normalizes tool *types*, providers still differ in:

- whether the model can call that tool
- whether tool execution happens on the provider side
- what events/blocks appear in the stream

Plan for graceful degradation.

### 12.5 Gemini thought signatures in tool loops

Gemini/Gemini OAuth may require `thoughtSignature` on repeated function-call context. The SDK preserves this by carrying it on `tool_use` input as `__thought_signature` in history and translating it back to provider-native `thoughtSignature` on subsequent turns.

If you manage history manually, preserve this field on assistant `tool_use` blocks.

### 12.6 OpenAI-compatible providers should compose, not fork

For OpenAI-compatible Chat Completions providers, prefer composing `pkg/core/providers/openai` and configuring behavior with options (`baseURL`, model prefix, max tokens field, headers) instead of duplicating translation and streaming logic.

`oai-resp` remains separate by design because the OpenAI Responses API has a different request/response and streaming shape from Chat Completions.

---

## 13. Live Audio Mode (Gateway WebSocket)

This section documents the **current** `/v1/live` implementation in:

- `sdk/live.go`
- `sdk/live_helpers.go`
- `pkg/core/types/live.go`
- `pkg/gateway/handlers/live.go`
- `cmd/proxy-chatbot/live_mode.go` (reference client)

`LIVE_SDK_DX_DESIGN.md` is a design document for how this surface was intended to evolve. It is useful context, but the current shipped contract is the implementation above.

### 13.1 Current Status

Today’s `/v1/live` implementation is both:

- a gateway WebSocket endpoint, and
- a public proxy-only Go SDK surface via `Client.Live.Connect(...)`

Current behavior:

- One WebSocket session handles live mic input, streaming STT, model turns, and streaming TTS output.
- The first client text frame is `type:"start"` carrying a `run_request`.
- The gateway responds with `type:"session_started"` describing the negotiated audio contract.
- Live mode uses normal assistant text as the spoken channel.
- `talk_to_user` is deprecated in live mode and rejected if present as a client function tool or invoked during execution.
- Client function tools still work over `tool_call` / `tool_result`.
- Gateway server tools still work via `server_tools` and optional `server_tool_config`.
- `turn_complete.history` is the authoritative synced conversation history for the session.

### 13.2 Public SDK Surface

The public Go SDK surface is:

- `client.Live.Connect(ctx, req, opts)`
- `LiveConnectRequest`
- `LiveConnectOptions`
- `LiveSession`
- `LiveCallbacks`
- `LiveTurnTracker`
- `LivePlaybackReporter`

`Client.Live` is initialized by `NewClient(...)` alongside `Messages` and `Runs`.

Current request shape:

```go
req := &vai.LiveConnectRequest{
	Request: vai.MessageRequest{
		Model:    "openai/gpt-4o-mini",
		Messages: history,
		Voice: vai.VoiceOutput("voice-id",
			vai.WithAudioFormat(vai.AudioFormatPCM),
		),
	},
	Run: vai.ServerRunConfig{
		MaxTurns:      8,
		MaxToolCalls:  20,
		ParallelTools: true,
		ToolTimeoutMS: 30000,
	},
	ServerTools: []string{"vai_web_search"},
}

session, err := client.Live.Connect(ctx, req, &vai.LiveConnectOptions{
	Tools: []vai.ToolWithHandler{
		localTool,
	},
})
if err != nil {
	panic(err)
}
defer session.Close()
```

Important SDK semantics:

- `Live.Connect` is proxy-only and returns an invalid-request error unless `WithBaseURL(...)` is configured.
- `LiveConnectRequest` is a dedicated public type, but it intentionally mirrors the gateway `run_request` shape.
- `LiveConnectOptions.Tools` / `ToolHandlers` register local client tool handlers for `tool_call` events.
- `LiveSession.HistorySnapshot()` returns the latest authoritative history, updated only from `turn_complete.history`.
- `LiveSession.Close()` sends a best-effort `{"type":"stop"}` before closing the socket.
- `LiveSession.SendAudio(...)` sends binary PCM frames only.

Mode switching is intentionally cheap:

- `Messages.RunStream` consumes `[]vai.Message`
- `Live.Connect` consumes `[]vai.Message`
- `RunStream.Result().Messages` or `LiveSession.HistorySnapshot()` can seed the next mode directly

### 13.3 Startup and Authentication

Authentication and BYOK are provided on the WebSocket upgrade request via headers, not in the first JSON frame.

Current live auth requirements:

- Gateway auth, when enabled: `Authorization: Bearer <gateway key>`
- SDK/gateway version header: `X-VAI-Version: 1`
- Upstream model key: provider-specific `X-Provider-Key-*` header matching `run_request.request.model`
- Voice pipeline key: `X-Provider-Key-Cartesia`
- Server-tool BYOK headers when used (for example `X-Provider-Key-Tavily`, `X-Provider-Key-Exa`, `X-Provider-Key-Firecrawl`)

`Client.Live.Connect(...)` derives these headers from existing SDK configuration:

- `WithGatewayAPIKey(...)`
- `WithProviderKey(provider, key)`

Current startup contract:

- Client sends a JSON text frame:

```json
{
  "type": "start",
  "run_request": {
    "request": {
      "model": "anthropic/claude-sonnet-4",
      "messages": [],
      "max_tokens": 512,
      "voice": {
        "output": {
          "voice": "a167e0f3-df7e-4d52-a9c3-f949145efdab",
          "format": "pcm",
          "sample_rate": 16000
        }
      }
    },
    "run": {
      "max_turns": 8,
      "max_tool_calls": 20,
      "timeout_ms": 60000,
      "parallel_tools": true,
      "tool_timeout_ms": 30000
    },
    "server_tools": ["vai_web_search"]
  }
}
```

- The gateway validates the embedded `RunRequest` using the same strict request rules as `/v1/runs`.
- `run_request.request.voice.output.voice` is required.
- `run_request.request.voice.output.format` defaults to `pcm` if omitted.
- `run_request.request.voice.output.sample_rate` defaults to `16000` if omitted and must be one of:
  - `8000`
  - `16000`
  - `22050`
  - `24000`
  - `44100`
  - `48000`
- `run_request.request.stt_model` defaults to `cartesia/ink-whisper` if omitted.
- `builtins` is still accepted as a compatibility alias for `server_tools`.
- The SDK startup path accepts only `session_started` as the first server event; a startup `error` becomes a connect failure.

The gateway replies with:

```json
{
  "type": "session_started",
  "input_format": "pcm_s16le",
  "input_sample_rate_hz": 16000,
  "output_format": "pcm_s16le",
  "output_sample_rate_hz": 16000,
  "silence_commit_ms": 900
}
```

The SDK validates the startup contract conservatively:

- input format must be `pcm_s16le`
- input sample rate must be `16000`
- output format must be `pcm_s16le`
- output sample rate must be positive

### 13.4 Client Frames

Current client-to-server traffic is:

- Binary WebSocket frames:
  - raw `pcm_s16le` mono audio at `16000 Hz`
- JSON text frames:
  - `tool_result`
  - `playback_mark`
  - `playback_state`
  - `stop`

Current frame shapes:

```json
{"type":"tool_result","execution_id":"exec_1","content":[{"type":"text","text":"done"}]}
```

```json
{"type":"playback_mark","turn_id":"turn_1","played_ms":320}
```

```json
{"type":"playback_state","turn_id":"turn_1","state":"finished"}
```

```json
{"type":"stop"}
```

Notes:

- `playback_state.state` currently accepts only `finished` or `stopped`.
- `playback_mark.played_ms` must be non-negative.
- `tool_result.is_error` and `tool_result.error` are optional and map tool failures back into the live run.
- The SDK exposes helpers for these frames via:
  - `LiveSession.SendFrame(...)`
  - `LiveSession.SendAudio(...)`
  - `LiveSession.SendToolResult(...)`
  - `LiveSession.SendPlaybackMark(...)`
  - `LiveSession.SendPlaybackState(...)`

### 13.5 Server Events

Current server-to-client events are:

- `session_started`
- `assistant_text_delta`
- `audio_chunk`
- `tool_call`
- `user_turn_committed`
- `turn_complete`
- `audio_unavailable`
- `audio_reset`
- `turn_cancelled`
- `error`

Representative shapes:

```json
{"type":"assistant_text_delta","turn_id":"turn_1","text":"Hello"}
```

```json
{"type":"audio_chunk","turn_id":"turn_1","format":"pcm_s16le","sample_rate_hz":16000,"audio":"<base64>","is_final":false}
```

```json
{"type":"tool_call","turn_id":"turn_1","execution_id":"exec_1","name":"lookup_account","input":{"id":"123"}}
```

```json
{"type":"user_turn_committed","turn_id":"turn_1","audio_bytes":16384}
```

```json
{"type":"turn_complete","turn_id":"turn_1","stop_reason":"end_turn","history":[...]}
```

```json
{"type":"audio_unavailable","turn_id":"turn_1","reason":"tts_failed","message":"TTS synthesis failed: ..."}
```

```json
{"type":"audio_reset","turn_id":"turn_1","reason":"barge_in"}
```

```json
{"type":"turn_cancelled","turn_id":"turn_1","reason":"grace_period"}
```

```json
{"type":"error","fatal":false,"message":"...","code":"protocol_error"}
```

Notes:

- `assistant_text_delta` is the text that is also being spoken to the user.
- `audio_chunk.audio` is base64-encoded PCM payload.
- `audio_unavailable` is terminal for audio for that turn, but text/history processing can still finish.
- `turn_complete.history` is the authoritative synced conversation history for the session.
- `pkg/core/types.UnmarshalLiveServerEvent(...)` and `UnmarshalLiveClientFrame(...)` are the canonical typed JSON decoders for this protocol.

### 13.6 Session Processing and Helper Layer

`LiveSession.Events()` returns typed `LiveEvent` values. `LiveSession.Process(...)` provides callback dispatch similar in spirit to `RunStream.Process(...)`, but for live sessions.

Current live callback surface:

- shared callbacks through embedded `StreamCallbacks`:
  - `OnTextDelta`
  - `OnAudioChunk`
  - `OnAudioUnavailable`
  - `OnToolCallStart`
  - `OnToolResult`
  - `OnError`
- live-only callbacks:
  - `OnSessionStarted`
  - `OnUserTurnCommitted`
  - `OnTurnComplete`
  - `OnTurnCancelled`
  - `OnAudioReset`

Important processing semantics:

- `assistant_text_delta` maps to `OnTextDelta`
- `audio_chunk` maps to `OnAudioChunk` after base64 decode
- `tool_call` is auto-executed when a matching handler is registered in `LiveConnectOptions`
- unknown tools are answered with an error `tool_result` instead of hanging the session
- `turn_complete` updates internal session history before `HistorySnapshot()` is exposed to callers

The reusable helper layer in `sdk/live_helpers.go` is intentionally composable rather than a full runtime:

- `LiveTurnTracker`
  - tracks the current active turn
  - suppresses stale/cancelled/reset streaming events
  - still allows `turn_complete` after `audio_reset`
- `LivePlaybackReporter`
  - tracks one active playback turn at a time
  - schedules periodic `playback_mark` frames
  - sends final `playback_state=finished|stopped`
  - supports `ClearTurn()` for local hard-cut barge-in without claiming playback stopped/finished

These helpers are intended for clients that want `RunStream`-like reuse across live apps without pushing device I/O into the SDK.

### 13.7 Turn Semantics

Current live turn behavior in the gateway:

- Mic audio is streamed continuously to Cartesia STT.
- The gateway commits a user utterance after `900ms` of silence following confirmed speech.
- After commit, the gateway runs a server-side tool loop using the supplied `RunRequest`.
- The assistant speaks by emitting normal assistant text, not by calling `talk_to_user`.
- If the user resumes speaking within the `5s` grace window and the active turn has not progressed too far, the gateway cancels the turn and emits `turn_cancelled`.
- If the user barges in while assistant audio is playing, the gateway emits `audio_reset`, cancels the active turn, and truncates saved assistant history to what was actually played as best it can using playback marks and timestamps.
- If a selected tool name matches a configured gateway server tool, the gateway executes it itself. Other function tools are sent back to the client via `tool_call`.

On the client side, the demo now splits responsibilities this way:

- SDK owns session transport, tool execution, typed event decode, turn-state helpers, and playback reporting helpers.
- The demo owns mic capture, speaker playback, local RMS-based barge-in heuristics, and terminal rendering.

### 13.8 Current Limits and Reference Client

Current implementation limits and defaults:

- Input audio contract is fixed to `pcm_s16le` at `16000 Hz`.
- Output audio format is fixed to `pcm_s16le`; sample rate is negotiated from the requested voice output sample rate.
- Live STT/TTS currently depends on Cartesia credentials supplied via `X-Provider-Key-Cartesia`.

Gateway configuration relevant to live sessions currently comes from the general WebSocket settings in `pkg/gateway/config/config.go`:

- `VAI_PROXY_WS_MAX_DURATION`
- `VAI_PROXY_WS_MAX_SESSIONS_PER_PRINCIPAL`

The in-repo reference client is `cmd/proxy-chatbot/live_mode.go`. It is now an SDK consumer rather than its own private live transport implementation.

That client shows the current expected behavior:

- `Client.Live.Connect(...)` usage with shared `[]vai.Message` history
- binary PCM mic streaming
- `LiveTurnTracker`-backed stale/cancelled/reset turn suppression
- `LivePlaybackReporter`-backed `playback_mark` / `playback_state` reporting
- client-side handling of `audio_reset`, `turn_cancelled`, and `turn_complete`
