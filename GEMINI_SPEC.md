# GEMINI_SPEC (vai-lite)

This document specifies the **Gemini providers** implementation for `vai-lite`, rebuilt on the **Go GenAI SDK** (`google.golang.org/genai`).

It is intentionally detailed and opinionated so we can:
- implement the provider without ambiguity,
- preserve `vai-lite`'s canonical Anthropic-style API semantics (messages + typed content blocks),
- support `Run` / `RunStream` tool loops correctly, including **streamed function-call arguments** (Vertex-only),
- support multimodal inputs and outputs where Gemini supports them.

Status: [x] Implemented in codebase (2026-03-03), with adaptive Vertex streamed-arg fallback for model/backend variance.

---

## 0. Summary (Decisions)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/*`, `sdk/client.go`, `sdk/proxy_transport.go`, `pkg/gateway/upstream/factory.go`, `pkg/gateway/compat/catalog.go`, `pkg/gateway/handlers/messages.go`, `pkg/gateway/handlers/runs.go`

### 0.1 Provider IDs

- Primary provider IDs:
  - `gem-vert` (Vertex AI backend)
  - `gem-dev` (Gemini Developer API backend)
- **No `gemini` provider**. Any remaining usage is deprecated and should be removed from:
  - SDK client initialization
  - gateway upstream factory
  - compatibility catalog/validation
  - integration tests
  - docs/examples
- **No `gemini-oauth` provider**. Any remaining usage is deprecated and should be removed from:
  - SDK client initialization
  - gateway upstream factory
  - compatibility catalog/validation
  - integration tests
  - docs/examples

Rationale:
- This repo already supports BYOK key routing for proxy mode. Keeping separate `gemini`/`gemini-oauth` providers adds complexity without clear value.
- The Go GenAI SDK supports both Gemini Developer API and Vertex AI via a single client config surface.

### 0.2 Backend preference

Backend choice is made via provider ID:
- `gem-vert` always uses the Vertex AI backend.
- `gem-dev` always uses the Gemini Developer API backend.

Rationale:
- Only Vertex AI supports **streaming function call arguments** (`StreamFunctionCallArguments=true`), which is required for low-latency tool streaming semantics (e.g. `talk_to_user` captions/TTS style behavior in `RunStream`).

### 0.3 Canonical API compatibility

- `vai-lite` canonical request/response format remains Anthropic-style (`types.MessageRequest`, `types.MessageResponse`, `types.StreamEvent`).
- The Gemini providers must translate to/from GenAI SDK types.
- Tool streaming must be normalized into:
  - `content_block_start` + `input_json_delta` + `content_block_stop`
  - `tool_use` blocks for tool invocations

### 0.4 Thought signatures

- Gemini thought signatures (GenAI SDK part field `Part.ThoughtSignature`) must be preserved across tool recursion.
- The canonical history representation stores this as a sentinel input field:
  - `tool_use.input["__thought_signature"] = "<opaque string>"`
- The provider must **strip** `__thought_signature` from function args when calling the provider and instead populate provider-native `Part.ThoughtSignature`.

Rationale:
- Gemini may require thought signatures to be echoed back for repeated tool calls in multi-turn/tool recursion.
- We cannot leak thought signatures into tool argument JSON (see existing SDK tests).

Concrete encoding decision (normative):
- In canonical `tool_use.input["__thought_signature"]`, store the thought signature as **base64 (standard, padded)** encoding of the raw bytes.
- When sending provider history:
  - decode base64 -> bytes
  - populate the SDK part field `Part.ThoughtSignature = []byte{...}`
- When receiving provider output that includes a thought signature:
  - read from `Part.ThoughtSignature` (bytes)
  - base64-encode and store into `tool_use.input["__thought_signature"]`

Notes:
- Thought signatures are opaque; do not attempt to interpret or validate contents beyond base64 decode/encode.
- If base64 decode fails (malformed), treat the signature as absent and continue (do not hard-fail the run).

---

## 1. Goals / Non-Goals
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/*`, `pkg/core/providers/gem/*_test.go`, `sdk/run_gemini_thought_signature_test.go`

### 1.1 Goals

- Implement a shared Gemini core using `google.golang.org/genai`, with two provider IDs:
  - `gem-vert` (Vertex)
  - `gem-dev` (Developer API)
- Support:
  - inputs: `text`, `image`, `audio`, `video`, `document` (PDF)
  - outputs: `text`, `image`, `audio`, `video` (see video constraints)
  - function tool calling
  - **streamed tool arguments** (Vertex)
  - native tools normalization:
    - web search (Gemini "Google Search" grounding tool)
    - code execution (Gemini code execution tool)
- Provide correct and stable behavior for `Messages.Create`, `Messages.Stream`, `Messages.Run`, `Messages.RunStream`.
- Match existing `vai-lite` stream semantics:
  - stable content block indexing
  - tool argument reconstruction via `input_json_delta`
  - consistent `stop_reason` and `usage` where available
- Provide good errors mapped into `core.Error` / provider error types.

### 1.2 Non-Goals

- Implementing gateway live `/v1/live` behavior inside the Gemini providers (live mode is gateway-centric).
- Provider-side execution of user-defined function tools (SDK will execute tools locally in the tool loop).
- Advanced file upload management outside what the GenAI SDK supports for inline media.
- Perfect structured output compliance on all Gemini models (model compliance varies).

---

## 2. Terminology
Status: [x] Complete (2026-03-03)
Touched files: `GEMINI_SPEC.md`

- **Developer API**: Gemini Developer API (API key) backend in Go GenAI SDK (`BackendGeminiAPI`).
- **Vertex backend**: Vertex AI backend in Go GenAI SDK (`BackendVertexAI`), supports streamed function-call args.
- **Canonical types**: `pkg/core/types` objects used by `vai-lite` (`MessageRequest`, `ContentBlock`, `StreamEvent`).
- **Provider-native tools**: tools executed by Gemini itself (e.g. Google Search grounding).
- **Function tools**: `type="function"` tools whose handlers execute in the local SDK process.

---

## 3. Public Surface Area
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/core/providers/gem/provider_dev.go`, `pkg/core/providers/gem/provider_vert.go`, `sdk/adapters.go`, `pkg/gateway/upstream/adapters.go`

### 3.1 Provider package contract

The Gemini providers must follow the same structural contract as other providers:

- `type Provider struct { ... }`
- `func New(...) *Provider`
- `func (p *Provider) Name() string` returns `"gem-vert"` or `"gem-dev"`
- `func (p *Provider) Capabilities() ProviderCapabilities`
- `func (p *Provider) CreateMessage(ctx, req) (*types.MessageResponse, error)`
- `func (p *Provider) StreamMessage(ctx, req) (EventStream, error)`
- `type EventStream interface { Next() (types.StreamEvent, error); Close() error }`

Note: provider packages define their own local `ProviderCapabilities`/`EventStream` to avoid import cycles,
and the SDK/gateway wrap them via adapters (see `sdk/adapters.go`, `pkg/gateway/upstream/adapters.go`).

### 3.2 Model strings and routing

- External callers use:
  - `req.Model = "gem-vert/<model-name>"` for Vertex
  - `req.Model = "gem-dev/<model-name>"` for Developer API
- `core.Engine` strips the provider prefix before calling the provider; the provider sees `req.Model == "<model-name>"`.
- The provider must re-add the provider prefix on the normalized response `MessageResponse.Model`:
  - `gem-vert/<model-name>` or `gem-dev/<model-name>`

---

## 4. Authentication
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/client.go`, `sdk/client.go`, `sdk/proxy_transport.go`, `cmd/proxy-chatbot/main.go`, `pkg/gateway/compat/catalog.go`

### 4.1 Required inputs

The provider must support both:

1. `gem-vert` (Vertex AI backend):
   - Vertex AI API key (BYOK) OR project/location config (optional support)
2. `gem-dev` (Gemini Developer API backend):
   - Gemini API key (BYOK)

### 4.2 Environment variables

Target behavior in this repo (post-migration):
- `gem-dev`:
  - `GEMINI_API_KEY` is used for the provider key.
  - `GOOGLE_API_KEY` is accepted as a fallback to populate `GEMINI_API_KEY`.
- `gem-vert`:
  - `VERTEXAI_API_KEY` is used for the provider key.

We intentionally introduce `VERTEXAI_API_KEY` for `gem-vert` so callers can keep both keys configured simultaneously.
Beyond that, avoid additional new env vars unless necessary; prefer `req.Extensions` for non-secret configuration.

### 4.3 Backend choice rules (normative)

- Backend selection is made by provider ID (`gem-vert` vs `gem-dev`).
- Do not provide a per-request backend switch via `req.Extensions`. This avoids surprising behavior when callers assume tool streaming and get a silent downgrade.

### 4.4 Vertex configuration rules (normative)

Minimum for Vertex API key usage:
- `genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendVertexAI})`

Optional project/location (if we choose to support in v1):
- `ClientConfig{Project: "...", Location: "...", Backend: BackendVertexAI}`

Decision point (implementation detail):
- If we only have an API key, omit `Project/Location` unless required by the SDK/backend.
- If project/location is required, accept via extensions:
  - `req.Extensions["gem"]["vertex_project"]`
  - `req.Extensions["gem"]["vertex_location"]`

### 4.5 Developer API configuration rules (normative)

Developer API usage:
- `genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})`
or equivalent SDK defaults.

Implementation note:
- Do not hardcode environment variable access inside the provider package; the SDK engine already injects provider keys.

---

## 5. Capabilities
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/gateway/compat/catalog.go`, `pkg/gateway/compat/validate.go`

### 5.1 ProviderCapabilities fields

Set `ProviderCapabilities` for `gem-vert` / `gem-dev` to reflect current intended support in `vai-lite`:

- `Vision: true`
- `AudioInput: true`
- `AudioOutput: true` (Gemini can output audio via response modalities; may be model-dependent)
- `Video: true` (Gemini supports video input; video output is separate API path)
- `Tools: true`
- `ToolStreaming: true` for `gem-vert`, `false` for `gem-dev`
- `Thinking: true` (Gemini supports thought-like metadata; canonical output uses `thinking` blocks)
- `StructuredOutput: true` (feature exists; model compliance varies)
- `NativeTools: []string{"google_search", "code_execution", "image_generation", ...}` (see §8)

Important distinction:
- `Capabilities` describes what the provider *can* support; model catalog/compat validation can be stricter.

---

## 6. Canonical Request Translation
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_request_test.go`

### 6.1 Messages

Canonical request: `types.MessageRequest`:
- `Messages []types.Message` with `Role: "user"|"assistant"`
- `System` may be `string` or `[]types.ContentBlock`

Gemini translation (normative):
- Convert canonical messages into GenAI contents in the original order.
- `system` prompt:
  - Use GenAI `GenerateContentConfig.SystemInstruction` (preferred) rather than injecting a synthetic message.
  - Mapping:
    - if `req.System` is a `string`: set `SystemInstruction` to a single text part.
    - if `req.System` is `[]types.ContentBlock`:
      - allow only `text` blocks; concatenate with `\n`
      - if non-text blocks are present, fail fast with `invalid_request_error` (Gemini system instructions are text-oriented; do not silently drop).
  - Only if the SDK/backend does not support `SystemInstruction` (unexpected), fall back to injecting a leading system-as-user content (last resort).

Role mapping:
- `user` -> `user`
- `assistant` -> `model` (Gemini naming varies; use SDK idioms)

### 6.2 Content blocks (inputs)

Canonical supported input blocks:
- `text` -> text part
- `image` -> inline or URI part
- `audio` -> inline part
- `video` -> inline part
- `document` -> inline part (PDF)
- `tool_result` -> becomes a message item that provides tool output to the model

#### 6.2.1 TextBlock

`types.TextBlock{Text: ...}` -> `genai.Text(...)` part.

#### 6.2.2 ImageBlock

If `ImageSource.Type == "url"`:
- Use a provider-native URI/file part (`genai.NewPartFromURI`) when possible.
- Require `ImageSource.MediaType` to be set when using URL input, because Gemini requires `mimeType` for URI parts.
  - Exception: if `req.Extensions["gem"]["infer_media_type"]=true`, the provider may best-effort infer a MIME type from the URL path extension.

If `ImageSource.Type == "base64"`:
- Decode base64 bytes and use `genai.NewPartFromBytes(data, mediaType)`.

Decision:
- Provider should accept base64 directly (do not force data URLs).
- Provider should also support URL images (unlike audio/video/PDF, which are base64-only in canonical types today).

#### 6.2.3 AudioBlock (input)

Audio input uses `AudioSource{MediaType, Data}`.
- Decode base64 bytes
- Provide inline media part via `genai.NewPartFromBytes(data, mediaType)`

#### 6.2.4 VideoBlock (input)

Video input uses `VideoSource{MediaType, Data}`.
- Decode base64 bytes
- Provide inline media part via `genai.NewPartFromBytes(data, mediaType)`

#### 6.2.5 DocumentBlock (PDF input)

Document input uses `DocumentSource{MediaType, Data}`.
- Decode base64 bytes
- Provide inline media part via `genai.NewPartFromBytes(data, mediaType)`

Constraints:
- Prefer `application/pdf`.
- For other document media types, pass through as-is and let provider validate.

#### 6.2.6 ToolResultBlock

Canonical: `types.ToolResultBlock{ToolUseID, Content, IsError}`

Gemini tool result translation (normative):
- Represent tool results as a **function response part** (`genai.FunctionResponse`) matching the tool call `id` and function `name`.
- Prefer constructing the part via `genai.NewPartFromFunctionResponse(name, response)` and then setting:
  - `part.FunctionResponse.ID = <tool_use.id>`
- Use the GenAI SDK function response shape:
  - `FunctionResponse.ID = <tool_use.id>`
  - `FunctionResponse.Name = <tool_use.name>`
  - `FunctionResponse.Response = map[string]any{...}`
- Preserve `IsError` in the response map using the conventional keys:
  - `output`: tool output payload
  - `error`: tool error payload

Normative payload format:
- Convert canonical `tool_result.content` blocks into a JSON-friendly structure:
  - `blocks`: array of objects, each the JSON form of a canonical `ContentBlock`
  - `text`: concatenation of any `text` blocks (in order), joined with `\n` (best-effort)
- Set:
  - `payload := map[string]any{"blocks": blocks, "text": text}`
  - if `IsError==true`: `response := map[string]any{"output": payload, "error": payload}`
  - else: `response := map[string]any{"output": payload}`

Important:
- Tool results are model-visible; never include secrets (API keys).
- Tool results should avoid including large base64 blobs. If a tool returns non-text blocks (image/audio/video/document), keep them in `blocks` but also include a concise `text` summary so the model can proceed even if it ignores non-text payloads.

#### 6.2.7 Supported MIME types (normative)

This provider should be strict about MIME types because Gemini/Vertex require correct `mimeType` for media parts.

Images (commonly supported by Gemini API):
- `image/png`
- `image/jpeg`
- `image/webp`
- `image/heic`
- `image/heif`

Audio (Vertex AI audio understanding docs list, and aligns with Gemini API usage patterns):
- `audio/x-aac`
- `audio/flac`
- `audio/mp3`
- `audio/m4a`
- `audio/mpeg`
- `audio/mpga`
- `audio/mp4`
- `audio/ogg`
- `audio/pcm`
- `audio/wav`
- `audio/webm`

Video (Gemini API video understanding docs list):
- `video/mp4`
- `video/mpeg`
- `video/mov`
- `video/avi`
- `video/x-flv`
- `video/mpg`
- `video/webm`
- `video/wmv`
- `video/3gpp`

Documents:
- `application/pdf`

Provider validation policy:
- If `MediaType` is empty, fail fast with `invalid_request_error`.
- If `MediaType` is present but not in the allowlist above:
  - do not fail by default (models/backends evolve); pass through and let the backend validate
  - attach a warning into `MessageResponse.Metadata["gem"]["warnings"]` (non-fatal) when possible

#### 6.2.8 Media size limits and transport strategy (normative)

We need deterministic behavior across:
- Gemini Developer API
- Vertex AI

Key constraints (documented by Google; details vary by model and evolve):
- Gemini API guidance historically recommends using the Files API when total request payload exceeds ~20 MB. Recent updates also mention larger payload support (up to ~100 MB) for some workflows.
- Vertex AI `generateContent` request payloads are commonly limited around ~20 MB for inline parts; large media should be provided via `fileUri` (GCS) when supported by the model.

`vai-lite` constraints:
- Canonical input blocks carry inline base64 for audio/video/pdf (no URI form).
- Image blocks support `url` and `base64`.

Normative provider behavior:
- For `image` with URL source:
  - use a URI part (`genai.NewPartFromURI`) and do not enforce the inline size limits.
- For inline base64 (`image/audio/video/document`):
  - decode base64 and enforce limits on the **decoded byte size** (not the base64 string length)
  - if above limit: fail fast with `invalid_request_error` and recommend:
    - `image` URL (for images), or
    - (future) gateway upload / Gemini Files API / GCS URIs (for audio/video/pdf; not implemented in `vai-lite` today)

Suggested default limits (implementation constants; tune via integration tests):
- Per inline part: 20 MiB decoded bytes
- Total decoded bytes across all inline parts in a single request: 20 MiB

Rationale:
- Base64 inflation is ~4/3 (about 33% overhead) before JSON framing, which makes “payload size” limits easy to hit unexpectedly.
- Approximate sizing:
  - decoded bytes: `n`
  - base64 string length: `4 * ceil(n/3)`
  - plus JSON overhead (quotes, commas, field names)
- Having deterministic SDK-side limits produces clearer errors than provider-side 400s with vague messages.

Extension override:
- Allow `req.Extensions["gem"]["inline_media_max_bytes"]` and `["inline_media_total_max_bytes"]` to override these limits (see §13).

### 6.3 Output configuration (modalities)

Canonical request:
- `req.Output.Modalities []string` with values like `text`, `image`, `audio`
- `req.Output.Image.Size` etc

Gemini translation:
- Use GenAI `GenerateContentConfig.ResponseModalities` (string modalities).
- Map:
  - `text` -> `"TEXT"`
  - `image` -> `"IMAGE"`
  - `audio` -> `"AUDIO"`

Image output config (best-effort):
- If `req.Output.Image.Size` is set, map it into `GenerateContentConfig.ImageConfig` when supported by the selected model.
- If a requested size is not supported, fail fast with a clear `invalid_request_error` explaining the supported values for that model/backend (determined via integration tests).

Audio output config (best-effort):
- Map requested audio output options (if we add canonical knobs later) into `GenerateContentConfig.SpeechConfig`.
- For now, treat audio output as:
  - enabled by including `"audio"` in `req.Output.Modalities`
  - configured via `req.Extensions["gem"]["speech_config"]` (see §13)

Video output:
- Gemini video generation is typically not part of `GenerateContent`; it is a separate API (e.g. `GenerateVideos`).
- If `req.Output.Modalities` includes `"video"`, the provider must use the video-generation endpoint/path (see §10).

### 6.3.1 Generation parameters mapping (normative)

Map canonical sampling/length fields into `GenerateContentConfig`:
- `req.MaxTokens` -> `GenerateContentConfig.MaxOutputTokens`
  - if `req.MaxTokens==0`, do not set `MaxOutputTokens` (let provider defaults apply)
- `req.Temperature` -> `GenerateContentConfig.Temperature`
- `req.TopP` -> `GenerateContentConfig.TopP`
- `req.TopK` -> `GenerateContentConfig.TopK`
- `req.StopSequences` -> `GenerateContentConfig.StopSequences`

Candidate policy (normative):
- Default to a single candidate (`CandidateCount` unset or `1`), and return only candidate 0 as canonical output.
- If `req.Extensions["gem"]["candidate_count"]` is set to `n>1`, set `GenerateContentConfig.CandidateCount=n`, but:
  - still return candidate 0 as canonical output
  - attach other candidates (raw or minimally translated) to `MessageResponse.Metadata["gem"]["candidates"]`.

### 6.3.2 Structured output mapping (`OutputFormat`) (normative)

Canonical structured output:
- `req.OutputFormat.Type == "json_schema"`
- `req.OutputFormat.JSONSchema` contains a JSON Schema object

Gemini mapping:
- Set:
  - `GenerateContentConfig.ResponseMIMEType = "application/json"`
  - `GenerateContentConfig.ResponseJsonSchema = <a JSON-serializable object>`

Schema conversion rule (normative):
- Convert `*types.JSONSchema` to a plain `map[string]any` / `any` tree preserving:
  - `type`, `properties`, `required`, `description`, `enum`, `items`, `additionalProperties`
- Do not attempt lossy conversion into a bespoke GenAI schema type; use the SDK’s `ResponseJsonSchema any` field.

Failure behavior:
- If `OutputFormat` is set but the model returns non-JSON text, `vai-lite` higher-level `Extract` helpers will handle parsing and erroring. The provider should not try to “repair” JSON in-core (avoid provider-side heuristics).

### 6.4 Stop sequences and stop reason mapping

Canonical request:
- `StopSequences []string`

Gemini behavior:
- Some Gemini APIs report a generic stop reason for both natural and stop-sequence stop.

Decision:
- The provider may accept stop sequences but should:
  - either omit them for models/backends where ambiguity breaks `StopReasonStopSequence`,
  - or accept but map stop reason conservatively (often `end_turn`).

Spec requirement:
- Document exact backend/model behavior once confirmed by integration testing.

---

## 7. Function Tools (SDK-executed)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_response.go`, `pkg/core/providers/gem/stream.go`, `pkg/core/providers/gem/translate_request_test.go`

### 7.1 Tool schema mapping

Canonical function tool:
`types.Tool{Type:"function", Name, Description, InputSchema}`

Gemini translation:
- Translate to `genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{...}}`

Schema conversion strategy (normative):
- Use `FunctionDeclaration.ParametersJsonSchema` (type `any`) and pass our JSON schema through as a JSON-serializable object.
- Do not attempt to map into older/alternate parameter schema types unless required by the SDK.

Name constraints (normative, enforced early):
- Tool/function names must:
  - start with a letter `[A-Za-z]` or underscore `_`
  - contain only letters/digits/underscore/dot/dash: `[A-Za-z0-9_.-]`
  - be max length 64
- If a tool name violates this, fail fast with `invalid_request_error` naming the tool and the rule violated.

Schema requirements:
- Must be deterministic and stable for tool calling to work across turns.
- Ensure `type: "object"` and `properties` exist even when empty.

### 7.2 ToolChoice mapping

Canonical:
- `tool_choice.auto | any | none | tool(name)`

Gemini translation:
- Map to SDK `FunctionCallingConfig.Mode`:
  - `auto` -> auto
  - `any` -> any/require (backend-specific)
  - `none` -> none
  - `tool(name)` -> validated/forced (if supported) else approximate with guidance

Concrete mapping to GenAI SDK (normative):
- `auto`:
  - `FunctionCallingConfig.Mode = "AUTO"`
- `any`:
  - `FunctionCallingConfig.Mode = "ANY"`
- `none`:
  - `FunctionCallingConfig.Mode = "NONE"`
- `tool(name)`:
  - `FunctionCallingConfig.Mode = "VALIDATED"`
  - `FunctionCallingConfig.AllowedFunctionNames = []string{name}`

If exact forcing is not supported:
- Fail fast with `core.ErrInvalidRequest` and a clear message, OR
- best-effort by setting tool config and adding a system hint.

Decision:
- Prefer fail-fast for `tool_choice.tool(name)` if backend cannot enforce it, to keep deterministic tool-loop behavior.

### 7.3 Thought signatures for function calls (normative)

When translating assistant history that contains `tool_use` blocks:

Canonical assistant history block:
`types.ToolUseBlock{ID, Name, Input}` where `Input` may include:
- tool args
- `__thought_signature` sentinel

Outbound translation to provider:
- Extract `sig := input["__thought_signature"]` (string)
- Remove `__thought_signature` from args before serializing
- Decode base64 signature and populate provider-native `Part.ThoughtSignature = []byte{...}` on the function call part (if present)

Inbound provider response:
- If provider includes `Part.ThoughtSignature` bytes on a function-call part:
  - base64-encode and emit canonical `tool_use` block where `Input["__thought_signature"] = <base64>`

Security:
- Thought signatures are opaque and should be treated as provider-controlled bytes.
- Never log them at info level; debug-only if needed.

---

## 8. Native Tools (Provider-executed)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_request.go`, `pkg/gateway/compat/validate.go`

`vai-lite` normalizes native tools via `types.Tool.Type` values:
- `web_search`
- `code_execution`
- etc

Gemini-specific mapping (normative):

### 8.1 Web search

Canonical native tool:
- `types.Tool{Type:"web_search", Config:*types.WebSearchConfig}`

Gemini translation (normative):
- Enable Gemini’s Google Search grounding capability via GenAI SDK tool fields:
  - Prefer `Tool.GoogleSearchRetrieval` when supported by the selected backend/model.
  - Otherwise, use `Tool.GoogleSearch` with `SearchTypes.WebSearch` enabled.

Config mapping (best-effort):
- `WebSearchConfig.MaxUses`:
  - Gemini does not expose a direct “max uses” knob; ignore (tool loop safety should enforce max tool calls at SDK layer).
- `AllowedDomains` / `BlockedDomains`:
  - Not supported in Gemini Developer API for Google Search tool fields.
  - For Vertex: apply only if the GenAI SDK exposes these fields for the selected tool type; otherwise ignore.
- `UserLocation`:
  - If the SDK supports location biasing in Google Search tool config, map it; otherwise ignore.

Response shaping:
- Gemini may return grounding metadata/citations rather than explicit tool-use blocks.
- `vai-lite` canonical content has server-tool blocks only for Anthropic (`server_tool_use`, `web_search_tool_result`).

Decision (normative):
- For Gemini native web search:
  - Do **not** emit `server_tool_use` blocks (reserved for Anthropic format).
  - Attach grounding metadata (if present) into `MessageResponse.Metadata["gem"]["grounding"]` as a JSON-serializable object.

### 8.2 Code execution

Canonical native tool:
- `types.Tool{Type:"code_execution", Config:*types.CodeExecutionConfig}`

Gemini translation:
- Enable Gemini code execution tool if supported by the selected backend/model (`Tool.CodeExecution`).

Backend constraints (normative):
- Gemini Developer API backend:
  - Code execution is supported, but some advanced configuration types/fields are documented as not supported.
  - Enable `Tool.CodeExecution` but ignore unsupported configuration knobs rather than failing.
- Vertex backend:
  - Enable `Tool.CodeExecution` (no per-language enforcement unless the SDK provides it).

Config mapping:
- `CodeExecutionConfig.Languages` / `TimeoutSeconds`:
  - If the SDK exposes equivalent knobs, map them.
  - Otherwise ignore and rely on provider defaults.

Decision:
- Do not attempt to fake `server_tool_use` blocks.
- Emit any tool-like evidence into metadata, and let the model's own text describe results.

### 8.3 Unsupported native tools

Canonical tools that should be rejected or ignored for Gemini:
- `web_fetch` (Anthropic-only native tool)
- `computer_use` / `text_editor` (Anthropic/OpenAI specific)
- `file_search` (OpenAI-specific in this repo)

Provider behavior should align with `pkg/gateway/compat/validate.go` once updated.

---

## 9. Streaming Normalization (`Messages.Stream`)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/core/providers/gem/stream.go`, `pkg/core/providers/gem/jsonpath_apply.go`, `pkg/core/providers/gem/stream_test.go`, `pkg/core/providers/gem/provider_stream_fallback_test.go`, `pkg/core/providers/gem/jsonpath_apply_test.go`

### 9.1 Canonical stream events

The provider must emit canonical `types.StreamEvent` events, primarily:
- `message_start`
- `content_block_start`
- `content_block_delta`
- `content_block_stop`
- `message_delta` (usage + stop reason)
- `message_stop` (optional but preferred if possible)
- `error` (if streaming fails)

### 9.2 Content block indexing rules (normative)

Canonical stream accumulation (`sdk/stream.go`) assumes:
- `ContentBlockStartEvent.Index` may be sparse.
- SDK compacts nil holes in final response for history validity.

Provider must:
- Use stable indices per logical block:
  - block 0 typically text/thinking (if present)
  - subsequent blocks tool uses and/or additional outputs

Do not reorder block indices mid-stream.

### 9.3 Text streaming

Emit:
- `content_block_start` with a `types.TextBlock{Text:""}`
- subsequent `content_block_delta` with `types.TextDelta{Text:<delta>}`
- optionally `content_block_stop` at completion

### 9.4 Tool streaming (function calls)

For streamed function calls, provider must emit:
1. `content_block_start` with `types.ToolUseBlock{ID, Name, Input:{}}`
2. one or more `content_block_delta` with `types.InputJSONDelta{PartialJSON:<delta>}`
3. `content_block_stop`

#### 9.4.1 Streamed args requirements

To stream args incrementally:
- `gem-vert` must enable:
  - `ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments = true`

`gem-dev`:
- `StreamFunctionCallArguments` is not supported.
- Emit a single `InputJSONDelta` containing the final args JSON string (best-effort) and still emit start/stop around it.

Implementation delta (2026-03-03):
- In `gem-vert` API-key mode, support may vary by model/backend path. The provider now uses adaptive behavior:
  - default auto mode tries `StreamFunctionCallArguments=true`,
  - on Vertex `INVALID_ARGUMENT` at stream-open it retries once with streaming args disabled,
  - explicit `extensions.gem.stream_function_call_arguments=true` remains strict (no auto fallback).

Rationale:
- `sdk/ToolArgStringDecoder` and tool-loop `RunStream.Process` depend on `input_json_delta` deltas.

#### 9.4.2 Vertex `PartialArgs` -> canonical `input_json_delta` (normative)

Vertex streamed function-call args arrive as typed partial updates (`PartialArg`) with:
- `JsonPath` (RFC 9535 JSONPath string, typically like `$.field` for root fields)
- one typed value set (`StringValue`, `NumberValue`, `BoolValue`, `NULLValue`, etc.)

Canonical `vai-lite` requires `InputJSONDelta.PartialJSON` to be concatenatable into a single valid JSON object at tool-use stop.

Typed value extraction (normative):
- Convert a `PartialArg` into a Go value by selecting the first non-nil typed field in this priority order:
  - `StringValue` -> `string`
  - `BoolValue` -> `bool`
  - `IntValue` -> `int64`
  - `DoubleValue` -> `float64`
  - `NumberValue` (string) -> parse as `json.Number` if possible, else keep as string
  - `NULLValue` -> `nil`
  - `StructValue` -> `map[string]any` (as provided)
  - `ListValue` -> `[]any` (as provided)
- If multiple fields are present (unexpected), prefer the first match and attach a warning in `MessageResponse.Metadata["gem"]["warnings"]`.

Normalization algorithm (implementable):

State per active tool call (keyed by tool block index):
- `id`, `name`
- `mode`:
  - `"stream_root_string"` (preferred low-latency path)
  - `"buffer_full_object"` (fallback)
- `rootKey` (for `"stream_root_string"`)
- `started bool` (whether we already emitted `{"<rootKey>":"`)
- `argsObject map[string]any` (for buffered object reconstruction)

Detection:
- If we observe partial arg updates that match:
  - `JsonPath` equals one of: `$.text`, `$.content`, `$.message`
  - and the value is a string fragment (`StringValue`)
  - and no other distinct root keys are observed for this tool call
  then use `"stream_root_string"` with `rootKey` set to the field name (without `$.`).
- Otherwise use `"buffer_full_object"`.

Emission rules:
- Always emit `content_block_start` for the tool use as soon as the tool call is discovered.
- For `"stream_root_string"`:
  1. On first string fragment, emit `input_json_delta` with `PartialJSON` equal to `{"<rootKey>":"` (opening brace, key, colon, opening quote).
  2. For each subsequent string fragment:
     - JSON-escape the fragment (so concatenation remains valid JSON)
       - recommended implementation: `b, _ := json.Marshal(fragment)` then strip the surrounding quotes from `string(b)` and emit the interior
     - emit it as an `input_json_delta` with `PartialJSON=<escaped-fragment>`
  3. On tool call completion, emit a final `input_json_delta` with `PartialJSON` equal to `"}` (closing quote and brace).
- For `"buffer_full_object"`:
  - Update `argsObject` as partial args arrive.
    - If `JsonPath` is a simple root path `$.k`, set `argsObject[k] = <typed value>`.
    - If `JsonPath` is more complex (nested paths/arrays):
      - attempt limited JSONPath application into `argsObject` (normative):
        - support the common subset:
          - `$` root
          - dot segments: `$.a.b.c`
          - bracketed string keys: `$['a']['b']`
          - bracketed integer indices for arrays: `$.a[0].b`
        - if a segment is unsupported (filters, wildcards, unions, recursive descent, slices, etc.):
          - do not fail; instead set a warning and skip applying that partial
      - also rely on the SDK’s final `FunctionCall.Args` when available at completion.
  - Emit no `input_json_delta` until completion.
  - On completion:
    - if `FunctionCall.Args` is non-nil, prefer it as the final args object
    - else, use the reconstructed `argsObject` (best-effort)
    - JSON-marshal the chosen final args object and emit it as a **single** `input_json_delta`.

Edge case (normative):
- If `FunctionCall.Args` is nil at completion and `argsObject` is empty due to unsupported JSONPath segments:
  - emit `{}` as args
  - attach a warning in `MessageResponse.Metadata["gem"]["warnings"]` indicating args reconstruction was incomplete

Tool completion detection (normative):
- Prefer provider-native boundaries when available:
  - Vertex tool-call updates include `FunctionCall.WillContinue`:
    - while `WillContinue==true`, do not emit `content_block_stop` for that tool call
    - when `WillContinue==false`, treat the tool call args as complete and close the tool block
- If `WillContinue` is absent/unreliable, treat the tool call as complete at:
  - end-of-candidate (finish reason `TOOL_CALL`), or
  - end-of-stream for that turn (last chunk)

Correctness requirement:
- The concatenation of all `PartialJSON` emitted for a tool block must parse as a JSON object at `content_block_stop`.

Latency note:
- This algorithm intentionally optimizes for the most common live-style tool signature:
  - a single root string field (e.g. `talk_to_user({text:"..."})`)
- Complex args still work, but may not stream incrementally; they will appear at completion.

#### 9.4.3 Tool call IDs (normative)

Canonical tool calls require stable IDs so `tool_result.tool_use_id` can correlate.

Rules:
- If the GenAI SDK provides a function call ID (string), use it.
- Otherwise synthesize:
  - `id := fmt.Sprintf("gem_tool_%d_%d", streamID, toolSeq)`
  - where:
    - `streamID` is a monotonically increasing `uint64` generated once per provider call/stream (per provider instance)
    - `toolSeq` is a per-call incrementing integer in discovery order

Deterministic generation (recommended):
- Provider holds `streamSeq uint64` (atomic).
- At the start of each `CreateMessage`/`StreamMessage`, do:
  - `streamID := atomic.AddUint64(&p.streamSeq, 1)`
- Initialize `toolSeq := 0` for that request/stream, and increment in tool discovery order.

The synthesized ID must be:
- stable for the duration of the streaming turn
- used consistently in:
  - emitted `tool_use` block (`ToolUseBlock.ID`)
  - constructed tool result messages (`ToolResultBlock.ToolUseID`)
  - function response parts (`FunctionResponse.ID`)

### 9.5 Thinking streaming

Canonical "thinking" blocks exist (`types.ThinkingBlock`, `types.ThinkingDelta`).

Gemini may expose reasoning/thought-like output differently than Anthropic.

Decision:
- If Gemini yields explicit thought parts:
  - GenAI SDK `Part.Thought == true` indicates a thought segment.
  - Map these into canonical `thinking` blocks/deltas:
    - `content_block_start` with `types.ThinkingBlock{Thinking:""}`
    - `content_block_delta` with `types.ThinkingDelta{Thinking:<delta>}`
    - `content_block_stop` at completion
- If Gemini only yields plain text:
  - do not invent `thinking` blocks

If Gemini returns "thought signatures" for tool calls, those are handled in tool blocks (not `thinking` blocks).

---

## 10. Outputs: Text / Image / Audio / Video
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_response.go`, `pkg/core/providers/gem/translate_response_test.go`

### 10.1 Text output

- Map to canonical `TextBlock`.

### 10.2 Image output

Gemini output parts may include either:
- inline bytes (inlineData) with a `mimeType`, or
- file/URI references (fileData / fileUri)

Normative mapping:
- If the SDK yields inline bytes for an image part:
  - base64-encode and emit canonical `types.ImageBlock{Source:{Type:"base64", MediaType:<mime>, Data:<base64>}}`
- If the SDK yields only a URI for an image part:
  - emit canonical `types.ImageBlock{Source:{Type:"url", MediaType:<mime if known>, URL:<uri>}}`
  - do not fetch/resolve the URI in provider-direct mode

### 10.3 Audio output

Gemini audio output is model-dependent and may be returned as inline bytes.

Normative mapping:
- If the SDK yields inline bytes for an audio part:
  - base64-encode and emit canonical `types.AudioBlock{Source:{Type:"base64", MediaType:<mime>, Data:<base64>}}`
  - if the SDK provides an audio transcript or captions, set `Transcript`

Streaming constraint (normative for v1):
- `vai-lite` canonical stream events do not have an “audio delta” type for provider-native audio output.
- Therefore:
  - `StreamMessage` should treat Gemini audio output as **final-only**: emit the audio block once the provider stream is complete (or as a single block with no deltas).
  - Do not attempt to emit incremental base64 deltas for audio output.

Note:
- `vai-lite` also supports voice output via Cartesia TTS in SDK mode; Gemini audio output is distinct and should not interfere.

### 10.4 Video output (separate path)

Gemini video generation typically uses a separate endpoint/operation (e.g. `GenerateVideos`).

Provider behavior:
- If `req.Output.Modalities` includes `"video"`:
  - the provider should route to video generation API path
  - return canonical `VideoBlock` with base64 bytes (or a URL if only URL is available)

Streaming:
- Expect video generation to be non-token-streaming (operation/poll style).
- `StreamMessage` may:
  - either not support video generation streaming,
  - or emit high-level progress as text deltas (not recommended for v1).

Decision (v1):
- Treat video generation as **not implemented** in `vai-lite` for now.
- If `req.Output.Modalities` includes `"video"`, fail fast with `invalid_request_error` explaining that video output is deferred and callers should use a dedicated video-generation surface when it is introduced.

Rationale:
- Video generation is a distinct API surface (operation/poll) and would significantly expand provider complexity and test burden.

---

## 11. Stop Reasons and Usage
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_response.go`, `pkg/core/providers/gem/stream.go`, `pkg/core/providers/gem/translate_response_test.go`

### 11.1 StopReason mapping (normative)

Canonical `types.StopReason` values:
- `end_turn`
- `max_tokens`
- `stop_sequence`
- `tool_use`

Gemini providers must map provider finish reasons to canonical stop reasons:
- Tool call -> `tool_use`
- Token limit -> `max_tokens`
- Otherwise -> `end_turn`

When ambiguous (Gemini STOP reason conflates cases):
- Prefer `end_turn` unless we can confidently attribute `stop_sequence`.

Finish reason metadata (normative):
- Always attach the raw Gemini finish reason (if available) into:
  - `MessageResponse.Metadata["gem"]["finish_reason"] = "<FINISH_REASON_ENUM>"`
- If the finish reason is safety/recitation/prohibited/spii/etc and content is empty:
  - return an empty assistant message with `stop_reason=end_turn`
  - include any provider safety ratings/feedback in metadata so callers can decide whether to surface an error.

### 11.2 Usage mapping

Canonical `types.Usage`:
- `InputTokens`, `OutputTokens`, `TotalTokens`, optional cache tokens

Gemini mapping (best-effort):
- If `GenerateContentResponse.UsageMetadata` is present:
  - `InputTokens = PromptTokenCount`
  - `OutputTokens = CandidatesTokenCount`
  - `TotalTokens = TotalTokenCount`
- If `TotalTokens` is missing/zero:
  - set `TotalTokens = InputTokens + OutputTokens` (matches SDK defensive behavior elsewhere).

Attach extra Gemini-only counters into metadata:
- `MessageResponse.Metadata["gem"]["usage"]`:
  - `cached_content_token_count`
  - `thoughts_token_count`
  - `tool_use_prompt_token_count`
  - `prompt_tokens_details` / `candidates_tokens_details` (modalities), if present

Backend note:
- Some usage fields are documented as not supported in Gemini Developer API. The provider must tolerate missing usage fields.

---

## 12. Errors
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/errors.go`, `pkg/core/providers/gem/errors_test.go`, `pkg/gateway/apierror/apierror.go`, `pkg/gateway/apierror/apierror_test.go`

### 12.1 Provider error type

Implement `pkg/core/providers/gem/errors.go` similar to other providers:
- `type Error struct { Type ErrorType; Message string; Code string; ProviderError any; RetryAfter *int }`
- `func (e *Error) IsRetryable() bool` for rate limit/overload/api errors

Map HTTP/status/provider error fields into:
- `invalid_request_error`
- `authentication_error`
- `permission_error`
- `not_found_error`
- `rate_limit_error`
- `api_error`
- `overloaded_error`
- `provider_error`

Concrete classification rules (normative):
- If the underlying error is `context.Canceled` or `context.DeadlineExceeded`, do not wrap as provider error; propagate as-is (SDK maps to `cancelled` / `timeout` stop reasons).
- If an HTTP response status is available:
  - `400` -> `invalid_request_error`
  - `401` -> `authentication_error`
  - `403` -> `permission_error`
  - `404` -> `not_found_error`
  - `409` -> `api_error` (treat as retryable only if provider indicates transient conflict)
  - `429` -> `rate_limit_error` (retryable; respect `Retry-After` if present)
  - `5xx` -> `overloaded_error` if the provider indicates overload/temporarily unavailable, else `api_error` (retryable)
- If only a Google RPC status code is available (common for Vertex):
  - `INVALID_ARGUMENT` -> `invalid_request_error`
  - `UNAUTHENTICATED` -> `authentication_error`
  - `PERMISSION_DENIED` -> `permission_error`
  - `NOT_FOUND` -> `not_found_error`
  - `RESOURCE_EXHAUSTED` -> `rate_limit_error`
  - `UNAVAILABLE` / `DEADLINE_EXCEEDED` -> `overloaded_error` (retryable)
  - otherwise -> `provider_error`

Retry hints:
- Populate `RetryAfter` (seconds) if the provider/transport surfaces `Retry-After` or an equivalent field.
- Mark retryable:
  - `rate_limit_error`, `overloaded_error`, most `api_error` with 5xx/UNAVAILABLE
- Mark non-retryable:
  - `invalid_request_error`, `authentication_error`, `permission_error`, most `not_found_error`

Observability:
- Attach provider request IDs (if present) into `ProviderError` for debugging.

### 12.2 Stream errors

On streaming failures:
- Prefer emitting a canonical `types.ErrorEvent` if the stream is still readable.
- Ensure `Next()` ultimately returns `io.EOF` or the underlying error consistently with other providers.

---

## 13. Extensions (`req.Extensions`)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_request_test.go`

Gemini-specific extensions should be namespaced:
- `req.Extensions["gem"]` is a `map[string]any`

### 13.1 Vertex config (optional)

- `vertex_project`: string
- `vertex_location`: string

### 13.2 Tool streaming (override)

- `stream_function_call_arguments`: bool
  - default: true for `gem-vert`
  - not supported for `gem-dev` (ignored)

### 13.2.1 Candidate count (optional)

- `candidate_count`: int
  - if set to `n>1`, maps to `GenerateContentConfig.CandidateCount=n`
  - canonical output remains candidate 0; additional candidates go to metadata (see §6.3.1)

### 13.3 Thinking controls (model-dependent)

Thinking controls map to `GenerateContentConfig.ThinkingConfig` (when supported by the model/backend):
- `include_thoughts`: bool
  - default: false (do not request thinking unless explicitly asked)
- `thinking_budget`: int
  - mapped to `ThinkingConfig.ThinkingBudget` (tokens)
- `thinking_level`: string
  - one of: `"LOW"|"MEDIUM"|"HIGH"|"MINIMAL"`
  - mapped to `ThinkingConfig.ThinkingLevel`

The provider must:
- validate types for known keys
- ignore unknown keys (do not fail) unless they break request validation

Type errors (normative):
- If a known key is present with the wrong type (e.g. `thinking_budget: "not_a_number"`), fail fast with `invalid_request_error` naming the key and the expected type.

### 13.4 Media and grounding (model-dependent)

If Gemini supports media resolution and grounding options:
- `media_resolution`: string
  - one of: `"LOW"|"MEDIUM"|"HIGH"` (preferred), or `"low"|"medium"|"high"` (accepted as alias)
  - maps to `GenerateContentConfig.MediaResolution`
- `infer_media_type`: bool
  - if true, allow best-effort MIME inference for `image` URL inputs when `MediaType` is missing (see §6.2.2)
- `inline_media_max_bytes`: int
  - overrides the per-inline-part decoded size limit (see §6.2.8)
- `inline_media_total_max_bytes`: int
  - overrides the total decoded size limit across all inline parts (see §6.2.8)
- `grounding`: map
  - provider-native knobs for Google Search grounding (dynamic retrieval thresholds, etc.)
  - shape is backend/model dependent; treat as passthrough and validate only for basic JSON types

Provider should attach returned grounding metadata into `MessageResponse.Metadata["gem"]["grounding"]` if present.

Type errors (normative):
- If a known key is present with the wrong type (e.g. `inline_media_max_bytes: "20mb"`), fail fast with `invalid_request_error`.

Grounding metadata storage (normative):
- Store grounding metadata as JSON-serializable data (no SDK structs) so it survives:
  - gateway JSON encoding
  - SDK history persistence
- Recommended implementation: `json.Marshal(sdkGroundingMetadata)` then `json.Unmarshal` into `map[string]any`, and store that map.

### 13.5 Safety settings (model-dependent)

Gemini/Vertex safety settings map to `GenerateContentConfig.SafetySettings` (list of `SafetySetting`).

Extension:
- `safety_settings`: array of objects like:
  - `{"category":"HARM_CATEGORY_...", "threshold":"BLOCK_..." }`
  - optional: `method`, `severity_threshold` (not supported in Gemini Developer API per SDK docs)

Provider behavior:
- Validate that `category` and `threshold` are strings.
- Pass through to SDK types where possible.
- For Gemini Developer API backend: ignore `method` and `severity_threshold` rather than failing.

Type errors (normative):
- If `safety_settings` is present but not an array of objects, fail fast with `invalid_request_error`.

### 13.6 Audio output configuration (model-dependent)

Audio output is enabled by including `"audio"` in `req.Output.Modalities`.

Extension:
- `speech_config`: map that maps to `GenerateContentConfig.SpeechConfig`:
  - `voice_name`: string (maps to `PrebuiltVoiceConfig.VoiceName`)
  - `language_code`: string (optional; if supported by model)
  - additional provider-native fields may be passed through as-is

---

## 14. Deterministic Tool Loop Requirements (`Run` / `RunStream`)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/core/providers/gem/stream.go`, `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_response.go`, `pkg/core/providers/gem/provider_stream_fallback_test.go`, `sdk/run.go`, `sdk/run_gemini_thought_signature_test.go`, `sdk/run_voice_test.go`

`sdk/run.go` and `sdk/run_stream_loop` depend on provider behavior.

### 14.1 Tool call reconstruction

In streaming:
- tool args arrive as incremental `input_json_delta` strings
- SDK accumulates them and, at `content_block_stop`, parses the full JSON into `tool_use.input`

Therefore, Gemini providers must:
- ensure the streamed deltas form a valid JSON object when concatenated
- emit `content_block_stop` for tool blocks whenever possible

If the upstream SDK does not provide an explicit "args done" boundary:
- the provider must synthesize a stop when it can infer tool call completion (end-of-stream candidate boundary).

### 14.2 No leakage of thought signature into args

The provider must ensure:
- `__thought_signature` never appears inside the serialized function args sent to Gemini.
- Thought signature is carried only in provider-native `Part.ThoughtSignature` field.

This is tested by `sdk/run_gemini_thought_signature_test.go`.

### 14.3 Interrupt friendliness

`RunStream.Interrupt` stops the current model stream and may resume with new user input.

Provider should:
- respect context cancellation in streaming calls
- close underlying iterators/readers promptly

---

## 15. Compatibility Catalog / Validation (Repo-wide updates required)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/gateway/compat/catalog.go`, `pkg/gateway/compat/validate.go`, `sdk/proxy_transport.go`, `pkg/gateway/handlers/messages.go`, `pkg/gateway/handlers/runs.go`

This spec implies the following catalog/validation changes (implementation tasks):

- Remove `gemini`, `gemini-oauth` from:
  - `pkg/gateway/compat/catalog.go`
  - `pkg/gateway/compat/validate.go` policies
  - `sdk/proxy_transport.go` provider header map (if present)
  - integration suite provider list
- Ensure the gateway `ProviderKeyHeader` includes `gem-vert` and `gem-dev` with appropriate key header mapping:
  - `gem-vert` -> `X-Provider-Key-VertexAI` (proposed)
  - `gem-dev` -> `X-Provider-Key-Gemini` (proposed)

Behavior for requests with:
- `model="gemini/..."`:
  - fail with `invalid_request_error` explaining the provider is removed and to use `gem-vert/...` or `gem-dev/...`.
- `model="gemini-oauth/..."`:
  - fail with `invalid_request_error` explaining the provider is removed and to use `gem-vert/...` or `gem-dev/...`.

---

## 16. Implementation Sketch (Files)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/core/providers/gem/provider_vert.go`, `pkg/core/providers/gem/provider_dev.go`, `pkg/core/providers/gem/options.go`, `pkg/core/providers/gem/client.go`, `pkg/core/providers/gem/translate_request.go`, `pkg/core/providers/gem/translate_response.go`, `pkg/core/providers/gem/stream.go`, `pkg/core/providers/gem/errors.go`, `pkg/core/providers/gem/jsonpath_apply.go`, `go.mod`, `go.sum`

Planned file structure:

- `pkg/core/providers/gem/`
  - shared core implementation (request translation, response translation, stream normalization, errors)
- `pkg/core/providers/gem/provider_vert.go`
  - `Name()=="gem-vert"`, Vertex defaults, env var mapping
- `pkg/core/providers/gem/provider_dev.go`
  - `Name()=="gem-dev"`, Developer API defaults, env var mapping
- `pkg/core/providers/gem/options.go`
  - WithHTTPClient, WithBaseURL (testing)
- `pkg/core/providers/gem/translate_request.go`
  - canonical -> genai request builder (contents/tools/config)
- `pkg/core/providers/gem/translate_response.go`
  - genai response -> canonical MessageResponse content blocks
- `pkg/core/providers/gem/stream.go`
  - genai stream iterator -> canonical StreamEvent iterator
- `pkg/core/providers/gem/errors.go`
  - provider error types and mapping

Dependency pinning (normative):
- Implementation must add `google.golang.org/genai` to `go.mod` and pin it to a specific version.
- That pinned version must include (compile-time) the SDK fields/features referenced by this spec:
  - streamed function call arguments on Vertex (`StreamFunctionCallArguments`)
  - `FunctionCall.PartialArgs` and `PartialArg.JsonPath`
  - `FunctionCall.WillContinue`
  - `Part.ThoughtSignature`
- If any of these are missing in the pinned version, the provider implementation must either:
  - update the pin, or
  - revise this spec and explicitly drop/replace the feature.

Testing:
- `pkg/core/providers/gem/*_test.go` for request/response/stream
- Update existing SDK test `sdk/run_gemini_thought_signature_test.go` to use new provider behavior without legacy HTTP stub expectations (if needed).

---

## 17. Test Plan (Acceptance Criteria)
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/*_test.go`, `pkg/core/providers/gem/provider_stream_fallback_test.go`, `sdk/run_gemini_thought_signature_test.go`, `sdk/run_voice_test.go`, `cmd/proxy-chatbot/main_test.go`, `pkg/gateway/*_test.go`, `integration/suite_test.go`, `integration_proxy/suite_test.go`, `integration/reliability_helpers_test.go`, `integration/voice_test.go`

### 17.1 Compile gate

- `go test ./...` must pass (unit tests) with `gem-vert`/`gem-dev` present and `gemini`/`gemini-oauth` removed everywhere.

### 17.2 Provider unit tests

- Multimodal inputs:
  - text only
  - image base64
  - audio base64
  - video base64
  - PDF base64
  - mixed blocks in one message
- Function tools:
  - schema translation
  - tool_choice mapping
  - thought signature round-trip (strip from args, preserve in signature)
- Native tools mapping:
  - web_search -> grounding enabled
  - code_execution -> tool enabled
- Outputs:
  - text output parsing
  - image/audio blocks mapping if returned by SDK
- Errors:
  - map 401/403/429/5xx properly into provider error types

### 17.3 Streaming tests (critical)

- Vertex streamed args:
  - tool call emits `content_block_start`
  - multiple `input_json_delta` chunks
  - `content_block_stop`
  - final `message_delta` has `stop_reason=tool_use` when tool call is the finish reason
- Fallback path:
  - Developer API backend emits single `input_json_delta` with full args
- Text streaming:
  - stable indices and correct delta concatenation

### 17.4 Tool loop tests

- `Messages.Run`:
  - tool recursion preserves thought signatures across turns
- `Messages.RunStream`:
  - tool arg deltas observable via `ToolInputDeltaFrom` and `ToolArgStringDecoder`

---

## 18. Operational Notes
Status: [x] Complete (2026-03-03)
Touched files: `pkg/core/providers/gem/provider.go`, `pkg/core/providers/gem/client.go`, `pkg/core/providers/gem/stream.go`

### 18.1 Logging

- Avoid logging raw tool args at info-level (may contain user data).
- Never log API keys.
- Thought signatures should not be logged except possibly in debug logs with redaction.

### 18.2 Timeouts and cancellation

- All provider calls must honor `ctx`.
- Streaming must stop promptly on `ctx.Done()`.

### 18.3 Deterministic output for history

- Ensure final `MessageResponse.Content` has no nil holes (SDK compacts, but provider should also avoid pathological index jumps).

### 18.4 Client lifecycle and concurrency (normative)

- The provider should create and reuse a single GenAI SDK client per provider instance:
  - client construction is not free (auth setup, transport)
  - reuse reduces per-request overhead and centralizes HTTP configuration
- Concurrency expectations:
  - Treat the GenAI client as safe for concurrent use unless we discover otherwise.
  - If the SDK proves non-thread-safe in practice, guard client calls with a mutex (last resort; may reduce throughput).
- Streaming iterators must not be shared across goroutines; each `StreamMessage` call owns its iterator and must close it.

Key binding / rotation (normative):
- A provider instance is bound to exactly one API key (the key passed to `New(...)`).
- The provider must not reuse a single GenAI SDK client across different API keys.
- In gateway proxy mode, the upstream factory constructs a provider per request using the BYOK key header, so key rotation is naturally handled by creating a new provider instance.
- In direct SDK mode, if the caller wants to change keys, they should create a new `vai.Client` (or otherwise reinitialize providers) rather than expecting per-request key switching.

### 18.5 HTTP transport (recommended defaults)

- Use an `http.Client` with sane timeouts (or respect upstream default if `vai-lite` centrally manages it):
  - request timeout should be controlled primarily via `ctx` deadlines
  - but an HTTP transport idle timeout should still be set to prevent leaked connections
- Allow provider options to inject a custom `http.Client` for:
  - tests (fake transport)
  - enterprise proxies / mTLS
  - custom retry middleware

---

## 19. Migration Notes (Repository)
Status: [x] Complete (2026-03-03)
Touched files: `sdk/client.go`, `pkg/gateway/upstream/factory.go`, `sdk/proxy_transport.go`, `pkg/gateway/compat/catalog.go`, `pkg/gateway/compat/validate.go`, `integration/suite_test.go`, `integration_proxy/suite_test.go`, `README.md`, `DEVELOPER_GUIDE.md`, `GATEWAY_SPEC.md`, `TS_SDK_DESIGN.md`, `LIVE_AUDIO_MODE_DESIGN.md`, `api/openapi.yaml`

Removing `gemini` and `gemini-oauth` requires coordinated updates:
- SDK initialization (`sdk/client.go`) should initialize `gem-vert` and `gem-dev`, and remove `gemini` / `gemini-oauth`.
- Gateway upstream factory (`pkg/gateway/upstream/factory.go`) should support `gem-vert` and `gem-dev`.
- Proxy transport header mapping should include `gem-vert` and `gem-dev` with distinct BYOK header names (or a shared header with backend discrimination).
- Compatibility catalog should remove `gemini` and `gemini-oauth` models and introduce `gem-vert` / `gem-dev`.
- Integration test provider matrix should remove `gemini` / `gemini-oauth`, and add `gem-vert` / `gem-dev`.
- Documentation updates:
  - `DEVELOPER_GUIDE.md` examples and env var documentation must be updated to reference `gem-vert` / `gem-dev` and `VERTEXAI_API_KEY`.
  - Any lingering mentions of `gemini` / `gemini-oauth` should be removed rather than deprecated.

All references should be deleted rather than aliased to keep the surface simple.

---

## 20. References (Upstream Docs)
Status: [x] Complete (2026-03-03)
Touched files: `go.mod`, `go.sum`, `GEMINI_SPEC.md`

Implementation deltas observed with `google.golang.org/genai v1.48.0`:
- `PartialArg` exposes `StringValue`, `BoolValue`, `NumberValue`, `NULLValue`, `JsonPath`, `WillContinue`; fields such as `IntValue`, `DoubleValue`, `StructValue`, `ListValue` are not present in this SDK version, so provider mapping uses the available field set.
- `genai.APIError` does not expose response headers, so `RetryAfter` cannot be populated directly from SDK error values in the current transport path.

Primary sources used to define concrete behaviors in this spec:
- Go GenAI SDK reference: `google.golang.org/genai` (pkg.go.dev)
- Vertex AI function calling (streamed args): Vertex AI Generative AI docs
- Vertex AI multimodal/file data input guidance and limits: Vertex AI Generative AI docs
- Gemini API media docs:
  - image understanding
  - video understanding
  - document processing
- Gemini API payload size update (context window / payload guidance): Google Developers Blog (Jan 2026)

These URLs change over time; keep this section updated as we pin a specific `google.golang.org/genai` version in `go.mod`.
