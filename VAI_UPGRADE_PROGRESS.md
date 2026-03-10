# VAI Upgrade Progress

Status: In progress
Last updated: 2026-03-10

## Objective

Implement the upgraded VAI gateway design in a production-ready way across the gateway runtime, transport layer, durable storage, SDK surface, and tests.

## Active Plan

### 1. Architecture mapping

- [completed] Read current gateway, SDK, live-mode, and Vango docs.
- [completed] Identify current extension seams to avoid parallel implementations.
- [completed] Record any design deltas needed from the draft spec.

### 2. Core protocol and invariants

- [completed] Add chain runtime/core types for stateful chain transport.
- [completed] Add canonical error taxonomy and transport mapping helpers.
- [completed] Add centralized capability validation for provider/model/tool combinations.
- [completed] Add strict frame/event decoding and fail-closed behavior.

### 3. Chain runtime

- [completed] Add chain manager, attachment lifecycle, replay buffer, and writer lease enforcement.
- [completed] Add in-memory active chain store and resumable pending-step state.
- [completed] Add durable idempotency and resume-token handling.

### 4. Shared execution engine

- [completed] Refactor current run/live execution into a shared chain-aware turn engine.
- [completed] Implement canonical commit-point semantics for history and tool batches.
- [completed] Preserve richer run timeline items for observability.

### 5. Gateway API surface

- [completed] Add `GET /v1/chains/ws`.
- [completed] Add stateful HTTP/SSE parity endpoints under `/v1/chains`.
- [completed] Enforce transport-wide writer lease rules and credential-scope rules.

### 6. Durable persistence and read APIs

- [completed] Extend Postgres schema for sessions/chains/runs/items/tool calls/attachments/idempotency/assets.
- [completed] Add storage services/repositories for write and read paths.
- [completed] Add chain/run/session history and context APIs.

### 7. SDK

- [completed] Add chain-attached SDK APIs.
- [completed] Add transport auto-selection and reconnect/resume handling.
- [completed] Keep stateless `Messages.*` APIs intact.

### 8. Assets

- [completed] Add gateway-managed asset upload/claim/metadata/signing APIs using the Vango S3 lifecycle.
- [completed] Add `asset_id` content references and resolve them into provider-ready request content before execution.
- [completed] Add asset tests for upload/sign flows and runtime content resolution.

### 9. Live runtime unification

- [completed] Move `/v1/live` onto the shared chain manager for chain ownership, attachment persistence, and run execution.
- [completed] Preserve live-specific STT/TTS/barge-in semantics while consuming shared chain runtime events.
- [completed] Expose the backing `chain_id` from live session startup and verify live chain persistence through read APIs.

### 10. Verification

- [completed] Add unit and contract coverage for protocol/runtime/storage/SDK behavior.
- [completed] Run targeted integration tests.
- [completed] Run real-provider checks with low-cost models from repo `.env`.

### 11. Security hardening follow-up

- [completed] Harden chain attach/takeover auth to require same-org plus valid `resume_token`, with strict actor enforcement for actor-scoped chains.
- [completed] Harden `PATCH /v1/chains/{id}` and shared mutation flows to enforce chain ownership before reads or writes.
- [completed] Add canonical authz error coverage and contract tests for WS/live attach, HTTP patch, and wrong-org run starts.
- [completed] Re-run focused verification and record the security pass outcomes.

### 12. Developers page stability follow-up

- [completed] Stabilize `DevelopersPage` API key create/revoke rendering during refetches.
- [completed] Add regression coverage for revoke/create with delayed key-list refetches.
- [completed] Re-run focused `app/components` verification and record the outcome.

### 13. Managed observability and demo-chat migration

- [in_progress] Rework observability around single requests plus managed sessions/chains/runs.
- [in_progress] Add the remaining org-scoped history list/read/fork/regenerate APIs required by the platform.
- [completed] Move the demo chat to managed session/chain history as the source of truth.
- [completed] Add a demo-chat transport toggle for stateful HTTP/SSE versus chain WebSocket mode.
- [pending] Verify that both chat modes feed the same managed observability model.

## Progress Log

### 2026-03-10

- Started architecture review and repository mapping.
- Confirmed current gateway centers on `Messages`, `Runs`, `/v1/live`, and `pkg/gateway/runloop`.
- Began cross-referencing `DEVELOPER_GUIDE.md`, `VANGO_GUIDE.md`, and the upgraded design doc to align the implementation with the existing server-owned state model.
- Read the older `GATEWAY_WS_SESSION_ARCHITECTURE.md` draft and confirmed it already anticipates a single attachment/session authority model, which reduces migration risk.
- Re-referenced `VANGO_NEON_GUIDE.md` and `VANGO_S3_GUIDE.md` to keep the durable-state and asset decisions aligned with the repo’s blessed storage patterns.
- Identified the main implementation seam: add a shared chain runtime and protocol layer first, then re-route new transport surfaces and SDK behavior through it.
- Added canonical chain types, websocket frames/events, capability registry, and strict fail-closed decoding under `pkg/core/types/`.
- Added the chain runtime manager, in-memory store, replay buffer, idempotency handling, attachment takeover/resume-token flow, and shared chain-aware executor under `pkg/gateway/chains/`.
- Reworked writer-lease handling so stateful HTTP/SSE requests acquire ephemeral writer authority while active and cannot race active websocket attachments.
- Added gateway handlers for `GET /v1/chains/ws`, `POST /v1/chains`, `PATCH /v1/chains/{id}`, `POST /v1/chains/{id}/runs`, `POST /v1/chains/{id}/runs:stream`, and read APIs for sessions/chains/runs/context/timeline/effective-request.
- Wired the new chain routes and shared manager into the gateway server and re-ran gateway/package compilation successfully.
- Added the first chain-attached SDK surface with WS/HTTP/SSE transport selection, client-tool execution, and chain-level run/update helpers while keeping the existing stateless SDK entry points intact.
- Added a Neon-backed `PostgresStore`, a durable schema migration for sessions/chains/messages/runs/items/attachments/idempotency/assets, and wired the proxy/monolith entrypoints to use it when Neon is configured.
- Fixed two real durability/transport bugs discovered during live smoke testing:
  - websocket `chain.start` was persisting attachments before the chain row existed, which violated the attachment foreign key
  - stateful HTTP runs could fail on attachment persistence because `node_id` was being inserted as `NULL` against a `NOT NULL` column
- Added hot-state rollback on pre-run persistence failures so a failed start no longer strands a chain in `running`.
- Added server-side logging for unexpected stateful-chain failures and frame decode failures so transport/runtime issues are diagnosable without adding ad hoc debug code later.
- Added a gated real-provider SDK smoke test in `sdk/chains_integration_test.go` that exercises the chain websocket path end to end against the local gateway.
- Verified manually against the repo `.env` with a real OpenAI Responses model:
  - stateful HTTP chain create + run against `oai-resp/gpt-5-mini`
  - stateful websocket chain connect + run via `VAI_SMOKE_REAL=1 go test ./sdk -run TestChainsConnect_Run_WebsocketRealProvider -v`
- Re-ran `go test ./...` after the persistence and smoke-fix pass; the repository is green.
- Started the asset-storage implementation pass by re-reading `VANGO_S3_GUIDE.md`, confirming the gateway already has the durable `vai_assets` table, and mapping the missing pieces:
  - no gateway asset endpoints exist yet
  - content blocks do not yet support `asset_id` references
  - neither stateless handlers nor the chain runtime resolve asset references before provider execution
- Added a shared blob-store builder under `internal/blobstore/` so the proxy and monolith use the same Vango S3/R2 setup path.
- Added a gateway asset service under `pkg/gateway/assets/` with:
  - upload-intent creation
  - claim/promote into durable `vai_assets` rows
- Investigated the `app/components` failure in `TestDevelopersPageCreateAndRevokeKey` and confirmed the underlying issue was developers-page UI state, not the revoke service path.
- Updated `DevelopersPage` to keep a last-ready API-key snapshot, optimistically apply create/revoke results, and keep rendering stable key data while the keyed resource is refetching or temporarily errors.
- Added a delayed-refetch regression harness in `settings_test.go` so revoke behavior is verified while the list query is intentionally blocked after the action succeeds.
- Hit the expected Vango runtime failure from stale state artifacts after adding the new `lastKeys` signal, then ran the proper workflow:
  - `/tmp/vango state plan --json`
  - `/tmp/vango state apply`
  - `/tmp/vango gen bindings`
- Verified the full fix with:
  - `go test ./app/components -run TestDevelopersPageCreateAndRevokeKey -count=1`
  - `go test ./app/components -run TestDevelopersPage_KeepsLastReadyKeysWhileRevokeRefetchIsPending -count=1`
  - `go test ./app/components -count=1`
  - `go test ./... -count=1`
  - signed read URLs
  - request-scoped asset resolution into provider-ready content
- Added public gateway asset endpoints:
  - `POST /v1/assets:upload-intent`
  - `POST /v1/assets:claim`
  - `GET /v1/assets/{id}`
  - `POST /v1/assets/{id}:sign`
- Extended content blocks and SDK helpers to support gateway asset references:
  - `source.type = "asset"`
  - `source.asset_id`
  - SDK helpers like `ImageAsset`, `AudioAsset`, `AudioSTTAsset`, `VideoAsset`, and `DocumentAsset`
- Wired asset resolution through:
  - stateless `POST /v1/messages`
  - stateless `POST /v1/runs`
  - stateful chain HTTP runs
  - stateful chain websocket runs
- Added focused coverage for:
  - asset API lifecycle
  - stateless message-time asset resolution
  - stateful chain-run asset resolution
  - `oai-resp` document asset URL projection
- Started the `/v1/live` runtime-unification pass by tracing the current live handler against the chain manager and confirming the cleanest seam:
  - keep the existing live STT/TTS/barge-in surface
  - let the shared chain manager own the live chain, runs, tool waits, and durable history
  - translate shared chain events into the existing live websocket event vocabulary
- Finished the `/v1/live` runtime-unification pass:
  - `/v1/live` now creates or attaches to a real shared chain attachment with `AttachmentModeLiveWS`
  - live turns execute through the shared chain manager instead of the old isolated live-only run loop
  - client tool results in live mode now flow back through `client_tool.result` into the same chain run
  - live session startup now returns `chain_id`, `session_id`, and `resume_token`
  - canonical live history now stays aligned with chain commit-point semantics, including interruption handling
- Finished the SDK reconnect pass:
  - chain websocket clients now auto-reattach with `resume_token`, `after_event_id`, and takeover on dropped sockets
  - automatic reconnect works during an in-flight streamed run and rotates the stored `resume_token` on successful reattach
  - live SDK sessions now expose the backing `ChainID`, `SessionID`, and `ResumeToken`
- Added focused coverage for the new work:
  - live handler test proving `/v1/live` creates a backing chain and commits history through the shared runtime
  - live handler test updated to assert interrupted turns preserve canonical history rather than mutating it with truncated partial text
  - SDK chain websocket test proving automatic reconnect/reattach resumes a streamed run successfully
  - SDK live websocket test proving live startup carries chain metadata through the public client
- Verification refresh after the live + reconnect pass:
  - `go test ./pkg/gateway/handlers -count=1`
  - `go test ./sdk -count=1`
  - `go test ./pkg/gateway/server ./cmd/server ./cmd/vai-proxy -count=1`
  - `go test ./... -p 1`
  - `go test ./...`
  - `VAI_SMOKE_REAL=1 go test ./sdk -run TestChainsConnect_Run_WebsocketRealProvider -v`
- Started a focused security-hardening pass for the first two audit findings:
  - chain attach/takeover currently accepts any org if the caller has a valid `resume_token`
  - `PATCH /v1/chains/{id}` currently reads and mutates by raw `chain_id` without ownership enforcement
- Locked the remediation scope for this pass:
  - centralize chain authz in the manager
  - add a dedicated canonical chain-access denial error for non-resume-token mutation failures
  - cover typed WS, `/v1/live`, stateful HTTP patch, and shared `run.start` mutation auth
- Finished the security hardening pass for the first two findings:
  - added manager-owned `authorizeAttach` and `authorizeChainMutation` helpers
  - enforced same-org plus valid `resume_token` on attach/takeover
  - made actor-scoped attach fail when `X-VAI-Actor-ID` is missing or mismatched
  - ensured successful attach/takeover emits and persists the authoritative chain actor scope
  - added canonical `auth.chain_access_denied` for non-resume-token chain ownership failures
  - switched `PATCH /v1/chains/{id}` pre-validation to an auth-aware `GetChainForMutation(...)` read path
  - aligned `run.start` wrong-org failures with the same shared mutation auth helper
  - updated live fatal error emission so canonical chain auth errors surface their actual codes on `/v1/live`
- Added focused contract coverage for:
  - typed WS cross-org attach rejection
  - typed WS missing/wrong actor attach rejection
  - typed WS authorized takeover with resume-token rotation
  - live cross-org attach rejection
  - live missing/wrong actor attach rejection
  - HTTP patch wrong-org rejection without default leakage
  - HTTP patch owner success on idle chains
  - HTTP patch attach-conflict behavior while a writer lease is active
  - wrong-org stateful `run.start` rejection with `auth.chain_access_denied`
- Verification after the security pass:
  - `go test ./pkg/gateway/handlers -count=1`
  - `go test ./pkg/gateway/chains ./pkg/gateway/handlers ./sdk -count=1`
  - `go test ./... -count=1` completed except for an unrelated existing failure in `app/components`:
    - `TestDevelopersPageCreateAndRevokeKey`
- Investigated the `app/components` failure:
  - `TestDevelopersPageCreateAndRevokeKey` passes in isolation and under repetition
- Continued the managed observability and demo-chat migration:
  - moved the platform chat and home surfaces onto managed session/chain history
  - added the per-chat stateful SSE vs WebSocket transport toggle
  - routed new demo-chat sends through public chain APIs instead of legacy conversation persistence
  - wired platform chat run metadata so managed observability can surface transport, endpoint kind, key source, and access credential for both modes
- Current gap being closed now:
  - add public `POST /v1/chains/{id}:fork` and `POST /v1/runs/{id}:regenerate` so branching/regeneration exists as first-class managed-history API surface rather than only internal chat composition logic
- Finished the public branch/regenerate API pass:
  - added managed history types for chain fork and run regenerate responses
  - added durable chain-message record listing so regenerate can reconstruct the exact pre-run fork point and resend input safely
  - added manager preview/create paths for `chain.fork` and `run.regenerate` with shared auth, idempotency, resume-token issuance, and session continuity
  - added `POST /v1/chains/{id}:fork` and `POST /v1/runs/{id}:regenerate`
  - made first runs on regenerated forks carry `rerun_of_run_id`
  - added SDK helpers for `client.Chains.Fork(...)` and `client.Chains.Regenerate(...)`
  - tightened stateful run pre-validation to use auth-aware chain reads instead of unauthenticated raw chain lookups
  - fixed history read endpoints so canonical chain auth failures are preserved on HTTP reads instead of being flattened into generic core errors
- Added focused verification for the branch/history work:
  - chain fork handler contract test
  - run regenerate contract test covering the follow-up rerun and `rerun_of_run_id`
  - org-scoped history list/read contract test for sessions and chains
  - SDK HTTP helper tests for fork and regenerate
- Refreshed Vango app state artifacts after the managed chat / observability component changes:
  - `vango state apply`
  - `vango gen bindings`
- Final verification after the branch/history completion:
  - `go test ./pkg/gateway/chains ./pkg/gateway/handlers -count=1`
  - `go test ./sdk -run 'TestChains(Fork_UsesHTTPEndpoint|Regenerate_UsesHTTPEndpoint|RunStreamCancel_SendsRunCancelFrame)' -count=1`
  - `go test ./app/components -count=1`
  - `go test ./sdk ./pkg/gateway/... ./cmd/server ./internal/services ./internal/chatruntime -count=1`
  - `go test ./... -count=1`
  - `VAI_SMOKE_REAL=1 go test ./sdk -run TestChainsConnect_Run_WebsocketRealProvider -count=1 -v`

### 2026-03-10 follow-up bugfixes

- Investigated the platform chat failure on stateful SSE with `oai-resp`:
  - real root cause was not the provider adapter or chat handler
  - the chain SDK was duplicating `Metadata` into both gateway-managed chain/run metadata and provider-bound `defaults.metadata` / `overrides.metadata`
  - platform demo chat uses nested `observability` objects in `Metadata`, which `oai-resp` rejects because OpenAI Responses metadata values must be scalar/string-like
- Fixed the chain SDK contract so metadata roles are explicit:
  - `Metadata` now maps only to top-level chain/run metadata for gateway-managed storage and observability
  - new `ProviderMetadata` fields on `ChainRequest`, `ChainUpdateRequest`, and `ChainRunRequest` map to provider-bound `defaults.metadata` / `overrides.metadata`
  - this prevents internal observability payloads from leaking into upstream provider requests while still preserving a way to send explicit provider metadata when needed
- Added regression coverage for both transports:
  - websocket chain connect/run test now asserts internal `Metadata` stays top-level while `ProviderMetadata` is the only metadata forwarded to provider-bound defaults/overrides
  - added a dedicated stateful SSE chain test asserting the same separation on `/v1/chains` + `/v1/chains/{id}/runs:stream`
- Verification for the metadata-separation fix:
  - `go test ./sdk ./app/components ./internal/chatruntime ./cmd/server -count=1`
  - the failing HTML captured during the broader run shows the revoke action in-flight while the list still renders stale resource data
  - `DevelopersPage` currently renders directly from the `keys` resource and only calls `keys.Refetch()` after create/revoke success, unlike the chat page which preserves a last-ready snapshot during refetches
- Locked the remediation approach for the developers page:
  - keep a last-ready API key snapshot in component state
  - optimistically update it on create/revoke success
  - continue background refetch for canonical reconciliation
  - add a delayed-refetch regression test so the UI no longer depends on synchronous resource refresh timing
- Investigated the observability page crash on navigation:
  - root cause was not the observability page logic itself; `setup.URLParam(...)` allocations in `ObservabilityPage` were missing from generated bindings
  - the underlying Vango bug was in the state scanner, which classified `setup.Signal`, `setup.Resource`, etc., but did not classify `setup.URLParam` even though it allocates a local signal via `vango.SetupSignal`
  - that caused `vango state plan` / runtime anchor allocation to disagree with `vango gen bindings`, producing the panic:
    - `missing binding for anchor "github.com/vango-go/vai-lite/app/components.ObservabilityPage#setup0.local.signal@0"`
- Fixed the Vango-side state tooling:
  - added `setup.URLParam` primitive classification in `vango/internal/state/scanner/primitive.go`
  - added scanner regression coverage for URL params in `vango/internal/state/scanner/scanner_test.go`
  - added apply/manifest regression coverage in `vango/internal/state/plan/apply_test.go`
- Added a platform-level regression for the concrete page failure:
  - `app/components/settings_test.go` now mounts `ObservabilityPage` with stubbed request-log data and verifies the page loads without a binding mismatch panic
- Found and resolved an environment wrinkle during verification:
  - a previously running `vango dev` process was still using the old in-memory scanner and kept regenerating stale state artifacts
  - after stopping that stale watcher, re-running `vango state apply` and `vango gen bindings` produced the correct ObservabilityPage signal anchors in both:
    - `vango_state_manifest.json`
    - `app/components/vango_state_bindings_gen.go`
- Verification after the observability fix:
  - `go test ./internal/state/scanner ./internal/state/plan ./internal/state/bindings -count=1` in `vango`
  - `go test ./app/components -run TestObservabilityPageLoadsWithoutBindingMismatch -count=1`
  - `go test ./... -count=1` in `vai-gateway`
  - restarted `vango dev` with the updated code and confirmed the observability route no longer triggers a server panic during a live request
- Started the managed observability + demo-chat migration pass:
  - confirmed the platform observability page still only reads `gateway_request_logs` / `gateway_run_traces`
  - confirmed the platform chat still uses legacy `conversations` / `conversation_messages` plus stateless `/v1/runs:stream`
  - traced the existing managed-history gateway surface and verified the clean migration seam is:
    - add missing session/chain list and branch endpoints with org-scoped auth
    - use managed chain/session/run records as the platform’s history source of truth
    - keep `gateway_request_logs` only for single-request `/v1/messages*` observability
- Current implementation focus for the demo-chat migration:
  - treat `/chat/{id}` as an `external_session_id` draft instead of a durable `conversations.id`
  - load sidebar/home/detail state from managed `vai_sessions` / `vai_chains` / `vai_runs`
  - use stateful chain SSE as the normal chat mode and `/v1/chains/ws` as the websocket mode
  - preserve regenerate/edit by starting a new chain seeded from rewritten managed history instead of mutating legacy conversation rows
- Implementation note before editing:
  - chat continuation on SSE can reuse an existing `chain_id` without a `resume_token`
  - chat continuation on websocket requires a live `resume_token`; if the page loses it, the fallback is to start a new chain in the same external session seeded from managed history
  - that keeps the demo aligned with the managed session/chain model even across reloads or transport changes
- Investigated the next platform chat failure in stateful SSE mode after the metadata fix:
  - upstream error was `oai-resp: invalid_request_error: Missing required parameter: 'input[5].output'`
  - the visible tool trace already showed the real clue: the gateway emitted a `tool_call_start` for `vai_web_search`, then persisted a `tool_result` with `content: []`
  - this was not a web-search provider failure; it was a chain-runtime serialization bug
- Root cause:
  - the chain runtime was using a generic JSON round-trip clone helper on values that contain `[]types.ContentBlock`
  - Go cannot unmarshal JSON objects back into a slice of non-empty interfaces without explicit decoding, so those clone calls silently zeroed tool-result content
  - the broken clones existed in the hot execution path (`resolveToolBatch`, `SubmitClientToolResult`, partial-response buffering, initial timeline items) and in managed-history cloning/storage helpers
  - once the empty tool result reached the OpenAI Responses adapter, `function_call_output.output` became an empty string and was omitted by `omitempty`, which produced the upstream validation error
- Fixed the chain-runtime and managed-history serialization path properly:
  - added shared typed helpers for `cloneContentBlocks`, `cloneMessages`, normalized flexible message content, and explicit empty tool-result normalization
  - replaced all direct `cloneJSON([]ContentBlock)` usage in the chain executor with typed content cloning
  - normalized empty successful tool results into an explicit empty text block so the runtime records a real resolved tool output instead of an ambiguous empty slice
  - updated chain/default cloning to preserve `system` content when it is block-based rather than plain string
  - moved content-bearing chain-message record construction onto the same normalized content path
- Hardened managed-history decode paths so persisted content round-trips correctly:
  - added custom JSON unmarshal logic for `types.ChainDefaults` so `system` survives as string or `[]ContentBlock`
  - added custom JSON unmarshal logic for `types.RunTimelineItem` so timeline content survives Postgres/memory-store decode correctly
  - this fixed not only the SSE runtime failure but also adjacent risks in timeline/effective-request/history reads for content-bearing chain data
- Added a provider-side safety belt for OpenAI Responses:
  - `function_call_output.output` is now always serialized, even when the tool result is logically empty
  - this prevents future empty-result regressions from reappearing as missing-parameter provider errors
- Added focused regressions:
  - `pkg/gateway/chains/executor_test.go` now proves a gateway tool result survives into the second model request and the persisted run timeline
  - added a normalization regression for explicit empty tool results
  - `pkg/core/providers/oai_resp/request_response_test.go` now proves empty tool results still serialize an explicit `output` field for `function_call_output`
- Verification after the tool-result serialization fix:
  - `go test ./pkg/core/types ./pkg/core/providers/oai_resp ./pkg/gateway/chains ./pkg/gateway/handlers ./internal/chatruntime ./app/components ./sdk -count=1`
  - `go test ./... -count=1`
- Follow-up after the first tool-result adapter hardening:
  - the initial fix removed `omitempty` from `oai_resp.inputItem.output`, which accidentally serialized `output` on ordinary `message` items too
  - OpenAI Responses then rejected simple requests with `Unknown parameter: 'input[0].output'`
  - corrected the adapter by making `inputItem.Output` a pointer so only actual `function_call_output` items include the field, while empty tool results still serialize `"output":""`
  - added a regression proving regular message items do not emit `output`
