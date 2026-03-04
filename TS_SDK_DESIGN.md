# TypeScript SDK Design (VAI Gateway)

This document defines a concrete design for a compelling TypeScript SDK that targets the VAI Gateway (proxy mode). It is intentionally aligned with:
- `/v1/messages` + SSE (provider-shaped message streaming)
- `/v1/runs` + `/v1/runs:stream` + SSE (server-side tool-loop streaming)
- future Live Audio Mode over WebSocket (per `LIVE_AUDIO_MODE_DESIGN.md`)

Related docs:
- `VAI_SDK_STRATEGY.md`
- `RUNS_API_AND_EVENT_SCHEMA.md`
- `API_CONTRACT_HARDENING.md`
- `PROXY_MODE_IMPLEMENTATION_PLAN.md`
- `LIVE_AUDIO_MODE_DESIGN.md`

---

## 0. Product Goals

The TS SDK should be:
- **Thin**: no provider translation logic; minimal client-side state machines.
- **Streaming-first**: great ergonomics for SSE and WebSocket.
- **Portable**: works in Node and modern browsers.
- **Type-safe**: strong TypeScript types with ergonomic constructors and extractors.
- **Operationally friendly**: request IDs, error classification, retry hints, safe logging.

Non-goals (v1):
- Re-implementing the full Go `RunStream` tool loop client-side. (Gateway provides `/v1/runs:stream`.)
- Shipping a full audio capture/playback implementation as required dependency. (Keep optional adapters.)

---

## 1. Package Layout

Recommended structure (single npm package with optional subpaths):
- `@vango-ai/vai`
  - `client` (HTTP/SSE/WS core)
  - `types` (request/response/event types)
  - `helpers` (content constructors, stream extractors)
  - `sse` (SSE parser)
  - `errors` (canonical error model)
  - `auth` (header construction)
  - `live` (WS protocol client)

Optional add-ons (later, separate packages to avoid heavy deps):
- `@vango-ai/vai-node-audio` (mic/speaker helpers for Node)
- `@vango-ai/vai-web-audio` (WebAudio helpers for browsers)

Rationale:
- Keep the core SDK dependency-light and environment-agnostic.
- Provide batteries as opt-in extras.

---

## 2. Core Client Configuration

### 2.1 Constructor

```ts
const vai = new VAI({
  baseURL: "https://api.vai.dev",
  gatewayApiKey: "vai_sk_...",
  providerKeys: {
    anthropic: "sk-ant-...",
    openai: "sk-...",
    "gem-dev": "...",
    "gem-vert": "...",
  },
  defaultModel: "anthropic/claude-sonnet-4",
});
```

### 2.2 Config surface

```ts
type ProviderName =
  | "anthropic"
  | "openai"
  | "oai-resp"
  | "gem-dev"
  | "gem-vert"
  | "groq"
  | "cerebras"
  | "openrouter";

type VAIConfig = {
  baseURL: string;

  // Hosted: usually required. Self-host: optional depending on auth_mode.
  gatewayApiKey?: string;

  // BYOK: headers set per provider.
  providerKeys?: Partial<Record<ProviderName, string>>;

  // Convenience; callers can still pass model per request.
  defaultModel?: string;

  // Supply a fetch implementation if needed (Node polyfills/custom).
  fetch?: typeof fetch;

  // Global default timeout (per request can override).
  timeoutMs?: number;

  // Adds headers to every request (must not include provider keys).
  defaultHeaders?: Record<string, string>;

  // Optional: low-level logger hook. Must receive redacted metadata only.
  logger?: (ev: VAILogEvent) => void;
};
```

### 2.3 Auth header mapping

The SDK sets headers:
- Gateway auth:
  - `Authorization: Bearer <gatewayApiKey>` (if provided)
- BYOK provider headers (if provided and required by request model):
  - `X-Provider-Key-Anthropic`
  - `X-Provider-Key-OpenAI` (used for both `openai/*` and `oai-resp/*`)
  - `X-Provider-Key-Gemini`
  - `X-Provider-Key-Groq`
  - `X-Provider-Key-Cerebras`
  - `X-Provider-Key-OpenRouter`

Rules:
- Never log these header values.
- Prefer per-request provider key overrides for multi-tenant apps:
  - `vai.messages.create(req, { providerKeys: { openai: "..." } })`

---

## 3. Types and Constructors (Ergonomics)

### 3.1 Canonical content block constructors

Expose helpers mirroring the Go SDK:

```ts
import { text, imageURL, audioBase64, toolResult, contentBlocks } from "@vango-ai/vai";

const req: MessageRequest = {
  model: "openai/gpt-4o",
  messages: [{
    role: "user",
    content: contentBlocks(
      text("What's in this image?"),
      imageURL("https://example.com/cat.jpg"),
    ),
  }],
};
```

Goals:
- avoid callers manually constructing JSON objects for common cases
- keep raw types available for advanced users

### 3.2 Strictness expectations

The gateway will enforce strict decoding (per `API_CONTRACT_HARDENING.md`), so the SDK should:
- build shapes that round-trip correctly
- optionally validate shapes client-side in dev mode (behind a flag), but not as a hard runtime dependency

---

## 4. `/v1/messages` API

### 4.1 Non-streaming

```ts
const resp = await vai.messages.create({
  model: "anthropic/claude-sonnet-4",
  messages: [{ role: "user", content: "Hello" }],
});
console.log(resp.content);
```

Return type:
- `MessageResponse` (canonical shape)
- include response metadata:
  - `requestId`
  - `durationMs`
  - token headers if present

### 4.2 Streaming (SSE)

API design: AsyncIterable with convenience collectors.

```ts
const stream = await vai.messages.stream({
  model: "anthropic/claude-sonnet-4",
  messages: [{ role: "user", content: "Stream a haiku." }],
});

for await (const ev of stream.events()) {
  const delta = textDeltaFrom(ev);
  if (delta) process.stdout.write(delta);
}

const final = await stream.finalResponse();
```

Recommended stream object:
```ts
type MessageStream = {
  events(): AsyncIterable<StreamEvent>;
  finalResponse(): Promise<MessageResponse>;
  abort(reason?: string): void;
  requestId?: string;
};
```

Helper extractors:
- `textDeltaFrom(event): string | null`
- `thinkingDeltaFrom(event): string | null`
- `toolInputJSONDeltaFrom(event): string | null`

---

## 5. `/v1/runs` and `/v1/runs:stream` API

The main promise of VAI for non-Go languages is that SDKs stay thin by delegating tool-loop semantics to the gateway.

### 5.1 Blocking run

```ts
const result = await vai.runs.create({
  request: {
    model: "anthropic/claude-sonnet-4",
    messages: [{ role: "user", content: "Search and summarize X." }],
  },
  run: { maxTurns: 8, maxToolCalls: 10, timeoutMs: 60000 },
  builtins: ["vai_web_search"],
});

console.log(result.response);
```

### 5.2 Streaming run (SSE)

```ts
const run = await vai.runs.stream({
  request: { model: "...", messages: [...] },
  run: { maxTurns: 8, maxToolCalls: 10 },
  builtins: ["vai_web_search"],
});

for await (const ev of run.events()) {
  switch (ev.type) {
    case "stream_event": {
      const delta = textDeltaFrom(ev.event);
      if (delta) process.stdout.write(delta);
      break;
    }
    case "tool_call_start":
      // show tool activity
      break;
    case "run_complete":
      // ev.result
      break;
  }
}
```

Run events follow `RUNS_API_AND_EVENT_SCHEMA.md`.

SDK design goals:
- Typed discriminated unions: `type` is the discriminator.
- Provide convenience extractors for nested provider stream events.
- Provide collectors:
  - `finalResult()` that resolves on `run_complete` or rejects on `error`.

---

## 6. SSE Implementation Details (Node + Browser)

SSE parser requirements:
- parse `event:` and `data:` frames
- support multi-line `data:` aggregation
- tolerate `\r\n` and `\n`
- handle empty comment/keepalive lines

Transport approach:
- Use `fetch()` with `AbortController`.
- For Node:
  - rely on Node 18+ global fetch or inject Undici fetch.
- For browsers:
  - use native fetch + ReadableStream.

Do not use EventSource:
- it is GET-only and awkward for POST bodies; we need POST with JSON.

---

## 7. Error Model

Define a canonical TS error:

```ts
type VAIErrorType =
  | "invalid_request_error"
  | "authentication_error"
  | "permission_error"
  | "not_found_error"
  | "rate_limit_error"
  | "overloaded_error"
  | "api_error"
  | "provider_error";

class VAIError extends Error {
  type: VAIErrorType;
  param?: string;
  code?: string;
  requestId?: string;
  retryAfter?: number;
  providerError?: unknown;
  httpStatus?: number;
}
```

Rules:
- Non-2xx JSON responses throw `VAIError`.
- SSE terminal `error` events terminate the stream and throw `VAIError`.
- Network failures throw a distinct `VAITransportError` with `cause`.

---

## 8. Live Audio Mode (WebSocket) SDK Design

The Live Audio Mode SDK should wrap the WebSocket protocol and provide:
- a typed event stream
- helpers for audio frame transport (binary vs base64-json)
- backpressure and playback mark APIs
- reconnection hooks (even if resume is v1.1+)

### 8.1 High-level API

```ts
const session = await vai.live.connect({
  model: "anthropic/claude-sonnet-4",
  audioIn: { encoding: "pcm_s16le", sampleRateHz: 16000, channels: 1 },
  audioOut: { encoding: "pcm_s16le", sampleRateHz: 24000, channels: 1 },
  features: { audioTransport: "binary", sendPlaybackMarks: true },
});

// Send mic audio frames (caller controls capture).
session.sendAudioFrame(pcmBytes, { timestampMs, seq });

// Report playback progress.
session.sendPlaybackMark({ assistantAudioId, playedMs, state: "playing" });

for await (const ev of session.events()) {
  switch (ev.type) {
    case "assistant_audio_chunk":
      // ev.data (Uint8Array) + format
      break;
    case "partial_transcript":
      break;
    case "audio_reset":
      break;
  }
}
```

### 8.2 Typed events and strict versioning

The SDK should enforce:
- first message is `hello` with `protocol_version`
- server responds `hello_ack` with negotiated features
- the SDK exposes `protocolVersion` and negotiated settings

### 8.3 Audio adapters are optional

Core SDK does not require WebAudio or native audio deps.

Optional packages can implement:
- browser mic capture -> PCM16 frames
- browser speaker playback -> PCM16
- node mic capture (platform-specific)

### 8.4 `talk_to_user` strictness

For live mode:
- assume strict `talk_to_user` semantics server-side (preferred)
- the SDK should not try to “invent” tool calls; it is purely transport + UX helpers.

---

## 9. Versioning and Compatibility

Compatibility contract:
- Gateway endpoints and event schemas are the source of truth.
- SDK versions should track gateway protocol versions:
  - e.g. `live.protocol_version` surfaced as a constant

Recommended:
- Gate new features behind capability flags returned by the server (`/v1/models` and live `hello_ack`).

---

## 10. Reference Examples (Ship With SDK)

To make the SDK compelling, ship examples:
1. Node CLI: streaming `/v1/messages`
2. Node CLI: `/v1/runs:stream` with `vai_web_search` builtin
3. Browser demo: streaming `/v1/messages` with incremental UI updates
4. Live audio reference client (minimal): connect, stream mic frames, play assistant audio, send playback marks

---

## 11. Testing Strategy

1. SSE parser unit tests:
   - event/data parsing
   - multi-line data
   - pings/keepalives
2. Contract tests:
   - run against a mock server that emits known event sequences
3. Live WS tests:
   - handshake, binary audio frame path, backpressure signaling
