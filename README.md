# vai-lite

Minimal Vango AI Go SDK focused on:
- Single-turn methods: `Messages.Create()`, `Messages.Stream()`, `Messages.CreateStream()`
- `Messages.Run()` and `Messages.RunStream()`
- Gateway server-run methods: `Runs.Create()` and `Runs.Stream()`
- Tool execution + tool helpers (native tool normalization, `MakeTool`, `FuncAsTool`, `ToolSet`, etc.)

This repo supports two modes:
- **Direct mode** (default): SDK calls providers directly.
- **Proxy mode**: SDK calls a VAI Gateway (`/v1/messages`, `/v1/runs`, `/v1/runs:stream`) via `WithBaseURL(...)`.

Cartesia non-live audio mode is supported through request `Voice` config:
- Direct mode: SDK performs STT/TTS locally using Cartesia.
- Proxy mode: SDK forwards `Voice` and audio blocks to gateway; gateway performs STT/TTS and streams `audio_chunk` events.

## Install

```bash
go get github.com/vango-go/vai-lite/sdk
```

## Client Modes

### Direct mode (default)

```go
client := vai.NewClient(
  vai.WithProviderKey("anthropic", "sk-ant-..."), // optional; env vars also supported
)
```

### Proxy mode (gateway)

```go
client := vai.NewClient(
  vai.WithBaseURL("https://api.vai.example.com"),
  vai.WithGatewayAPIKey("vai_sk_..."),            // optional for self-host auth_mode=disabled
  vai.WithProviderKey("anthropic", "sk-ant-..."), // BYOK header in proxy mode
  vai.WithProviderKey("cartesia", "sk-cart-..."), // sent when Voice input/output is requested
)
```

Notes:
- In proxy mode, `WithProviderKey(...)` becomes source for `X-Provider-Key-*` BYOK headers.
- Non-streaming proxy calls apply a default timeout if your context has no deadline; pass per-request context deadlines in production.

## Error formatting

For CLI/log output, use `vai.FormatError(err)` to get consistent rich error details
across transport and API errors.

```go
if err != nil {
	fmt.Println(vai.FormatError(err))
}
```

## Cartesia setup

Configure Cartesia using either:
- `CARTESIA_API_KEY`
- `vai.WithProviderKey("cartesia", "...")`

## Quickstart (RunStream)

```go
package main

import (
	"context"
	"fmt"

	vai "github.com/vango-go/vai-lite/sdk"
)

func main() {
	// Direct mode reads provider keys from env:
	//   ANTHROPIC_API_KEY, OPENAI_API_KEY, GROQ_API_KEY,
	//   GEMINI_API_KEY (gem-dev, GOOGLE_API_KEY fallback), VERTEXAI_API_KEY (gem-vert), etc.
	client := vai.NewClient()

	stream, err := client.Messages.RunStream(context.Background(), &vai.MessageRequest{
		Model: "anthropic/claude-sonnet-4",
		Messages: []vai.Message{
			{Role: "user", Content: vai.Text("Search for the latest Go release and summarize what's new.")},
		},
		Tools: []vai.Tool{vai.WebSearch()},
	}, vai.WithMaxToolCalls(5))
	if err != nil {
		panic(err)
	}
	defer stream.Close()

	for event := range stream.Events() {
		if text, ok := vai.TextDeltaFrom(event); ok {
			fmt.Print(text)
		}
	}
	fmt.Println()
}
```

## Streaming Tool Args For TTS/Captions

Use `RunStream.Process` plus per-tool-index `ToolArgStringDecoder` instances to
decode streamed `input_json_delta` fragments into readable text in real time.

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
			fmt.Print(update.Delta) // feed TTS/captions incrementally
		}
	},
	OnToolUseStop: func(index int, id, name string) {
		delete(decoders, index)
	},
})
if err != nil {
	panic(err)
}
```

## Proxy Chatbot Demo

Run the gateway in one terminal and an interactive `RunStream` chatbot in another.

### Terminal 1: start local gateway (proxy mode)

```bash
VAI_PROXY_AUTH_MODE=disabled \
VAI_PROXY_ADDR=127.0.0.1:8080 \
go run ./cmd/vai-proxy
```

### Terminal 2: start chatbot demo

```bash
go run ./cmd/proxy-chatbot \
  --base-url http://127.0.0.1:8080 \
  --model oai-resp/gpt-5-mini \
  --live-voice-id "<your_voice_id>"
```

Notes:
- Both commands auto-load `./.env` if present (existing exported env vars still win).
- The chatbot requires a provider key for the active model:
  - `OPENAI_API_KEY` (`openai/*` and `oai-resp/*`)
  - `ANTHROPIC_API_KEY`
  - `GROQ_API_KEY`
  - `CEREBRAS_API_KEY`
  - `OPENROUTER_API_KEY`
  - `GEMINI_API_KEY` (or `GOOGLE_API_KEY`) for `gem-dev/*`
  - `VERTEXAI_API_KEY` for `gem-vert/*`
- `TAVILY_API_KEY` is required; the chatbot enables gateway-native `vai_web_search` and `vai_web_fetch` tools via Tavily.
- Text mode uses `Messages.RunStream()` (client-side loop), not `Runs.Stream()`.
- Text mode web tools are gateway-native function tools invoked by the SDK loop and executed by the gateway (`/v1/server-tools:execute`), so they work across model providers and after `/model` switches.
- The default model is `oai-resp/gpt-5-mini` so `gpt-5-mini` routes through the OpenAI Responses provider.
- Live mode (`/live`) requirements:
  - `CARTESIA_API_KEY` (required for Live STT)
  - `VAI_LIVE_VOICE_ID` or `--live-voice-id` (required)
  - `VAI_LIVE_VOICE_PROVIDER` or `--live-voice-provider` (`cartesia` default, `elevenlabs` optional)
  - `ELEVENLABS_API_KEY` (required only when provider is `elevenlabs`)
  - `ffmpeg` and `ffplay` available on `PATH` (demo live mic + playback)
  - Optional mic selection envs:
    - `VAI_LIVE_MIC_NAME` (preferred on macOS; resolves AVFoundation input by device name, e.g. `ULT WEAR`)
    - `VAI_LIVE_MIC_INDEX` (macOS AVFoundation audio index, defaults to `0`)
    - `VAI_LIVE_FFMPEG_INPUT` (explicit ffmpeg input string, overrides `VAI_LIVE_MIC_INDEX`)
  - Optional playback envs:
    - `VAI_LIVE_FFPLAY_AUDIO_DEVICE` (sets ffplay output device via `SDL_AUDIO_DEVICE_NAME`)
    - `VAI_LIVE_FFPLAY_VOLUME` (0-100, default `100`)
- In live mode, the chatbot uses gateway `/v1/live` with server tools enabled:
  - `vai_web_search` (provider: Tavily)
  - `vai_web_fetch` (provider: Tavily extract)
- Runtime commands:
  - `/model` shows the active model
  - `/model:{provider}/{model}` switches models without resetting chat history
  - `/live` enters live audio mode
  - `/text` exits live audio mode back to text chat
  - `/interrupt` requests live turn interruption (live mode)
  - `/end` ends the active live session (live mode)
  - `/exit` or `/quit` exits

## Single-turn calls

```go
resp, err := client.Messages.Create(ctx, &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Summarize this in one sentence.")},
	},
})
if err != nil {
	panic(err)
}
fmt.Println(resp.TextContent())
```

```go
stream, err := client.Messages.CreateStream(ctx, &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Stream a short haiku.")},
	},
})
if err != nil {
	panic(err)
}
defer stream.Close()
for ev := range stream.Events() {
	if delta, ok := vai.TextDeltaFrom(ev); ok {
		fmt.Print(delta)
	}
}
```

## Cartesia Audio Mode (non-live)

### `Create` with audio input + final audio output

```go
req := &vai.MessageRequest{
	Model: "openai/gpt-4o",
	Messages: []vai.Message{
		{
			Role: "user",
			Content: vai.ContentBlocks(
				vai.Audio(wavBytes, "audio/wav"),
			),
		},
	},
	Voice: vai.VoiceFull(
		"a0e99841-438c-4a64-b679-ae501e7d6091",
		vai.WithLanguage("en"),
		vai.WithAudioFormat(vai.AudioFormatWAV),
	),
}

resp, err := client.Messages.Create(ctx, req)
if err != nil {
	panic(err)
}
fmt.Println("Transcript:", resp.UserTranscript())
if audio := resp.AudioContent(); audio != nil {
	fmt.Println("Synthesized media type:", audio.Source.MediaType)
}
```

### `Stream` with text events + audio side channel

```go
stream, err := client.Messages.Stream(ctx, &vai.MessageRequest{
	Model: "anthropic/claude-sonnet-4",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Tell me a short story.")},
	},
	Voice: vai.VoiceOutput("a0e99841-438c-4a64-b679-ae501e7d6091"),
})
if err != nil {
	panic(err)
}
defer stream.Close()

go func() {
	for chunk := range stream.AudioEvents() {
		play(chunk.Data, chunk.Format)
	}
}()

for ev := range stream.Events() {
	if delta, ok := ev.(vai.ContentBlockDeltaEvent); ok {
		if t, ok := delta.Delta.(vai.TextDelta); ok {
			fmt.Print(t.Text)
		}
	}
}
```

`Run`/`RunStream` remain the primary agent APIs; voice mode is additive when `req.Voice` is set.

## Client-side vs Server-side Runs

- `Messages.Run()` / `Messages.RunStream()`:
  - Client-side tool loop orchestration (SDK executes function tool handlers you register).
  - Works in direct and proxy modes.
- `Runs.Create()` / `Runs.Stream()`:
  - Gateway server-side run loop (`/v1/runs` and `/v1/runs:stream`).
  - Intended for provider-native tools and gateway-managed builtins.
  - Caller function tools are rejected for server-run requests.

## Structured output

```go
type Contact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

var out Contact
_, err = client.Messages.Extract(ctx, &vai.MessageRequest{
	Model: "openai/gpt-4o",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Jane Doe <jane@example.com>")},
	},
}, &out)
if err != nil {
	panic(err)
}
```

```go
contact, _, err := vai.ExtractTyped[Contact](ctx, client.Messages, &vai.MessageRequest{
	Model: "openai/gpt-4o",
	Messages: []vai.Message{
		{Role: "user", Content: vai.Text("Jane Doe <jane@example.com>")},
	},
})
```

## History + context management

`RunStream` emits `HistoryDeltaEvent` events that describe exactly which messages to append to a caller-owned history slice:

```go
ctx := context.Background()
history := []vai.Message{{Role: "user", Content: vai.Text("Hello!")}}

stream, err := client.Messages.RunStream(ctx, &vai.MessageRequest{
	Model:    "anthropic/claude-sonnet-4",
	Messages: history,
})
if err != nil {
	panic(err)
}
defer stream.Close()

apply := vai.DefaultHistoryHandler(&history)
for ev := range stream.Events() {
	apply(ev)
}
```

If you want mismatch detection (e.g. to catch accidental history divergence), use `DefaultHistoryHandlerStrict`.

For advanced context management (pinned memory, trimming, reordering), build per-turn messages at turn boundaries:

```go
req := &vai.MessageRequest{
	Model:    "anthropic/claude-sonnet-4",
	Messages: history,
}

stream, _ := client.Messages.RunStream(ctx, req,
	vai.WithBuildTurnMessages(func(info vai.TurnInfo) []vai.Message {
		// info.History is an append-only history snapshot for this turn boundary.
		return info.History
	}),
)
```

## Function tools (client-executed)

```go
type WeatherInput struct {
	Location string `json:"location" desc:"City name"`
}

tool := vai.MakeTool("get_weather", "Get current weather for a location.",
	func(ctx context.Context, in WeatherInput) (string, error) {
		return "72F and sunny in " + in.Location, nil
	},
)

// WithTools(tool) registers the handler and attaches the tool definition for this run.
result, err := client.Messages.Run(ctx, &vai.MessageRequest{
	Model: "openai/gpt-4o",
	Messages: []vai.Message{{Role: "user", Content: vai.Text("What's the weather in Austin?")}},
}, vai.WithTools(tool))
```
