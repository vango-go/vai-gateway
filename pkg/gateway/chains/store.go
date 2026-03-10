package chains

import (
	"context"
	"errors"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

var ErrNotFound = errors.New("chain store: not found")

type AttachmentRecord struct {
	ID                  string
	OrgID               string
	ChainID             string
	PrincipalID         string
	PrincipalType       string
	ActorID             string
	AttachmentRole      types.AttachmentRole
	Mode                types.AttachmentMode
	Protocol            string
	CredentialScopeJSON map[string]any
	ResumeTokenHash     string
	NodeID              string
	CloseReason         string
	StartedAt           time.Time
	EndedAt             *time.Time
}

type IdempotencyScope struct {
	OrgID          string
	PrincipalID    string
	ChainID        string
	Operation      string
	IdempotencyKey string
}

type IdempotencyRecord struct {
	ID             string
	OrgID          string
	PrincipalID    string
	ChainID        string
	Operation      string
	IdempotencyKey string
	RequestHash    string
	ResultRef      map[string]any
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type Store interface {
	UpsertSession(ctx context.Context, session *types.SessionRecord) error
	ListSessions(ctx context.Context, orgID string) ([]types.SessionRecord, error)
	GetSession(ctx context.Context, sessionID string) (*types.SessionRecord, error)
	GetSessionByExternal(ctx context.Context, orgID, externalSessionID string) (*types.SessionRecord, error)
	ListSessionChains(ctx context.Context, sessionID string) ([]types.ChainRecord, error)

	SaveChain(ctx context.Context, chain *types.ChainRecord, history []types.Message) error
	UpdateChain(ctx context.Context, chain *types.ChainRecord) error
	ListChains(ctx context.Context, orgID, sessionID string, unsessionedOnly bool) ([]types.ChainRecord, error)
	GetChain(ctx context.Context, chainID string) (*types.ChainRecord, []types.Message, error)
	ListChainMessages(ctx context.Context, chainID string) ([]types.ChainMessageRecord, error)
	AppendChainMessages(ctx context.Context, chainID, runID string, messages []types.Message) error
	SetChainResumeTokenHash(ctx context.Context, chainID, resumeTokenHash string) error
	GetChainResumeTokenHash(ctx context.Context, chainID string) (string, error)

	CreateRun(ctx context.Context, run *types.ChainRunRecord) error
	UpdateRun(ctx context.Context, run *types.ChainRunRecord) error
	GetRun(ctx context.Context, runID string) (*types.ChainRunRecord, error)
	ListChainRuns(ctx context.Context, chainID string) ([]types.ChainRunRecord, error)

	AppendRunItems(ctx context.Context, runID string, items []types.RunTimelineItem) error
	GetRunTimeline(ctx context.Context, runID string) ([]types.RunTimelineItem, error)

	SaveEffectiveRequest(ctx context.Context, req *types.EffectiveRequestResponse) error
	GetEffectiveRequest(ctx context.Context, runID string) (*types.EffectiveRequestResponse, error)

	SaveAttachment(ctx context.Context, attachment *AttachmentRecord) error
	CloseAttachment(ctx context.Context, attachmentID, reason string, endedAt time.Time) error

	SaveIdempotency(ctx context.Context, record *IdempotencyRecord) error
	GetIdempotency(ctx context.Context, scope IdempotencyScope) (*IdempotencyRecord, error)
}
