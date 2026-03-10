package types

import "time"

type ReplayStatus string

const (
	ReplayStatusFull    ReplayStatus = "full"
	ReplayStatusPartial ReplayStatus = "partial"
	ReplayStatusNone    ReplayStatus = "none"
)

type AttachmentMode string

const (
	AttachmentModeTurnWS       AttachmentMode = "turn_ws"
	AttachmentModeStatefulHTTP AttachmentMode = "stateful_http"
	AttachmentModeStatefulSSE  AttachmentMode = "stateful_sse"
	AttachmentModeLiveWS       AttachmentMode = "live_ws"
)

type AttachmentRole string

const (
	AttachmentRoleWriter AttachmentRole = "writer"
)

type ChainStatus string

const (
	ChainStatusIdle                 ChainStatus = "idle"
	ChainStatusRunning              ChainStatus = "running"
	ChainStatusWaitingForClientTool ChainStatus = "waiting_for_client_tool"
	ChainStatusClosed               ChainStatus = "closed"
	ChainStatusExpired              ChainStatus = "expired"
)

type RunStatus string

const (
	RunStatusQueued               RunStatus = "queued"
	RunStatusRunning              RunStatus = "running"
	RunStatusWaitingForClientTool RunStatus = "waiting_for_client_tool"
	RunStatusCompleted            RunStatus = "completed"
	RunStatusCancelled            RunStatus = "cancelled"
	RunStatusFailed               RunStatus = "failed"
	RunStatusTimedOut             RunStatus = "timed_out"
)

type ToolEffectClass string

const (
	ToolEffectClassReadOnly           ToolEffectClass = "read_only"
	ToolEffectClassIdempotentWrite    ToolEffectClass = "idempotent_write"
	ToolEffectClassNonIdempotentWrite ToolEffectClass = "non_idempotent_write"
	ToolEffectClassUnknown            ToolEffectClass = "unknown"
)

// ChainDefaults are the persistent defaults held by a chain.
// This intentionally mirrors MessageRequest configuration without the message list.
type ChainDefaults struct {
	Model             string         `json:"model,omitempty"`
	System            any            `json:"system,omitempty"`
	Tools             []Tool         `json:"tools,omitempty"`
	GatewayTools      []string       `json:"gateway_tools,omitempty"`
	GatewayToolConfig map[string]any `json:"gateway_tool_config,omitempty"`
	ToolChoice        *ToolChoice    `json:"tool_choice,omitempty"`
	MaxTokens         int            `json:"max_tokens,omitempty"`
	Temperature       *float64       `json:"temperature,omitempty"`
	TopP              *float64       `json:"top_p,omitempty"`
	TopK              *int           `json:"top_k,omitempty"`
	StopSequences     []string       `json:"stop_sequences,omitempty"`
	STTModel          string         `json:"stt_model,omitempty"`
	TTSModel          string         `json:"tts_model,omitempty"`
	OutputFormat      *OutputFormat  `json:"output_format,omitempty"`
	Output            *OutputConfig  `json:"output,omitempty"`
	Voice             *VoiceConfig   `json:"voice,omitempty"`
	Extensions        map[string]any `json:"extensions,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// RunOverrides are per-run ephemeral overrides applied on top of chain defaults.
type RunOverrides = ChainDefaults

type ClientToolReference struct {
	Name        string          `json:"name"`
	EffectClass ToolEffectClass `json:"effect_class,omitempty"`
}

type GatewayToolReference struct {
	Name        string          `json:"name"`
	EffectClass ToolEffectClass `json:"effect_class,omitempty"`
}

type ActiveRunInfo struct {
	RunID     string    `json:"run_id"`
	Status    RunStatus `json:"status"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type ActiveAttachmentInfo struct {
	Mode      AttachmentMode `json:"mode"`
	StartedAt time.Time      `json:"started_at"`
}

type SessionRecord struct {
	ID                     string         `json:"id"`
	OrgID                  string         `json:"org_id,omitempty"`
	ExternalSessionID      string         `json:"external_session_id,omitempty"`
	CreatedByPrincipalID   string         `json:"created_by_principal_id,omitempty"`
	CreatedByPrincipalType string         `json:"created_by_principal_type,omitempty"`
	ActorID                string         `json:"actor_id,omitempty"`
	Metadata               map[string]any `json:"metadata,omitempty"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
	LatestChainID          string         `json:"latest_chain_id,omitempty"`
}

type SessionChainList struct {
	Items []ChainRecord `json:"items"`
}

type SessionList struct {
	Items []SessionRecord `json:"items"`
}

type ChainRecord struct {
	ID                     string                `json:"id"`
	OrgID                  string                `json:"org_id,omitempty"`
	SessionID              string                `json:"session_id,omitempty"`
	ExternalSessionID      string                `json:"external_session_id,omitempty"`
	CreatedByPrincipalID   string                `json:"created_by_principal_id,omitempty"`
	CreatedByPrincipalType string                `json:"created_by_principal_type,omitempty"`
	ActorID                string                `json:"actor_id,omitempty"`
	Status                 ChainStatus           `json:"status"`
	ChainVersion           int64                 `json:"chain_version"`
	ParentChainID          string                `json:"parent_chain_id,omitempty"`
	ForkedFromRunID        string                `json:"forked_from_run_id,omitempty"`
	MessageCountCached     int                   `json:"message_count_cached"`
	TokenEstimateCached    int                   `json:"token_estimate_cached"`
	Defaults               ChainDefaults         `json:"defaults,omitempty"`
	Metadata               map[string]any        `json:"metadata,omitempty"`
	ActiveAttachment       *ActiveAttachmentInfo `json:"active_attachment,omitempty"`
	CreatedAt              time.Time             `json:"created_at"`
	UpdatedAt              time.Time             `json:"updated_at"`
}

type ChainRunRecord struct {
	ID              string         `json:"id"`
	OrgID           string         `json:"org_id,omitempty"`
	ChainID         string         `json:"chain_id"`
	SessionID       string         `json:"session_id,omitempty"`
	ParentRunID     string         `json:"parent_run_id,omitempty"`
	RerunOfRunID    string         `json:"rerun_of_run_id,omitempty"`
	IdempotencyKey  string         `json:"idempotency_key,omitempty"`
	Provider        string         `json:"provider,omitempty"`
	Model           string         `json:"model,omitempty"`
	Status          RunStatus      `json:"status"`
	StopReason      string         `json:"stop_reason,omitempty"`
	ToolCount       int            `json:"tool_count"`
	DurationMS      int64          `json:"duration_ms"`
	Usage           Usage          `json:"usage"`
	EffectiveConfig ChainDefaults  `json:"effective_config,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty"`
}

type ChainRunList struct {
	Items []ChainRunRecord `json:"items"`
}

type ChainList struct {
	Items []ChainRecord `json:"items"`
}

type ChainRunResultEnvelope struct {
	Run    *ChainRunRecord `json:"run,omitempty"`
	Result *RunResult      `json:"result,omitempty"`
}

type ChainForkRequest struct {
	History         []Message      `json:"history,omitempty"`
	Defaults        ChainDefaults  `json:"defaults,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	ForkedFromRunID string         `json:"forked_from_run_id,omitempty"`
}

type RunRegenerateRequest struct {
	Defaults ChainDefaults  `json:"defaults,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type ChainForkResponse struct {
	ChainID           string        `json:"chain_id"`
	SessionID         string        `json:"session_id,omitempty"`
	ExternalSessionID string        `json:"external_session_id,omitempty"`
	ParentChainID     string        `json:"parent_chain_id,omitempty"`
	ForkedFromRunID   string        `json:"forked_from_run_id,omitempty"`
	Defaults          ChainDefaults `json:"defaults,omitempty"`
	ResumeToken       string        `json:"resume_token,omitempty"`
	Input             []Message     `json:"input,omitempty"`
}

type ChainContextResponse struct {
	ChainID  string            `json:"chain_id"`
	AtRunID  string            `json:"at_run_id,omitempty"`
	Messages []Message         `json:"messages,omitempty"`
	Timeline []RunTimelineItem `json:"timeline,omitempty"`
}

type RunTimelineItem struct {
	ID              string           `json:"id"`
	Kind            string           `json:"kind"`
	StepIndex       int              `json:"step_index,omitempty"`
	StepID          string           `json:"step_id,omitempty"`
	ExecutionID     string           `json:"execution_id,omitempty"`
	SequenceInRun   int              `json:"sequence_in_run,omitempty"`
	SequenceInChain int              `json:"sequence_in_chain,omitempty"`
	Content         []ContentBlock   `json:"content,omitempty"`
	Tool            *RunTimelineTool `json:"tool,omitempty"`
	AssetID         string           `json:"asset_id,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

type RunTimelineTool struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type RunTimelineResponse struct {
	RunID string            `json:"run_id"`
	Items []RunTimelineItem `json:"items"`
}

type EffectiveRequestResponse struct {
	RunID           string        `json:"run_id"`
	Provider        string        `json:"provider"`
	Model           string        `json:"model"`
	EffectiveConfig ChainDefaults `json:"effective_config"`
	Messages        []Message     `json:"messages"`
}

type ChainMessageRecord struct {
	ID              string    `json:"id"`
	ChainID         string    `json:"chain_id"`
	RunID           string    `json:"run_id,omitempty"`
	Role            string    `json:"role"`
	SequenceInChain int       `json:"sequence_in_chain"`
	Content         any       `json:"content,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type AssetRecord struct {
	ID        string    `json:"id"`
	MediaType string    `json:"media_type"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type AssetSignResponse struct {
	AssetID   string    `json:"asset_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}
