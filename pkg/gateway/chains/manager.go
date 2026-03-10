package chains

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

type ProviderFactory func(providerName, apiKey string) (core.Provider, error)

type AttachmentSink func(types.ChainServerEvent) error

type Principal struct {
	OrgID         string
	PrincipalID   string
	PrincipalType string
	ActorID       string
}

type CredentialScope struct {
	ProviderKeys        map[string]string
	AllowedGatewayTools map[string]struct{}
}

func (s CredentialScope) AllowsModel(model string) bool {
	provider, _, ok := splitModel(model)
	if !ok {
		return false
	}
	return s.providerKeyFor(provider) != ""
}

func (s CredentialScope) providerKeyFor(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "oai-resp":
		if key := strings.TrimSpace(s.ProviderKeys["oai-resp"]); key != "" {
			return key
		}
		return strings.TrimSpace(s.ProviderKeys["openai"])
	default:
		return strings.TrimSpace(s.ProviderKeys[provider])
	}
}

func (s CredentialScope) AllowsGatewayTool(name string) bool {
	_, ok := s.AllowedGatewayTools[strings.TrimSpace(name)]
	return ok
}

func (s CredentialScope) AuthorizedProviders() []string {
	out := make([]string, 0, len(s.ProviderKeys))
	seen := map[string]struct{}{}
	for name, key := range s.ProviderKeys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if name == "openai" {
			seen["oai-resp"] = struct{}{}
		}
		seen[name] = struct{}{}
	}
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s CredentialScope) AuthorizedGatewayTools() []string {
	out := make([]string, 0, len(s.AllowedGatewayTools))
	for name := range s.AllowedGatewayTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type RuntimeEnvironment struct {
	Principal             Principal
	Mode                  types.AttachmentMode
	Protocol              string
	Scope                 CredentialScope
	NewProvider           ProviderFactory
	BuildGatewayTools     func(enabled []string, rawConfig map[string]any) (*servertools.Registry, error)
	ResolveMessageRequest func(ctx context.Context, req *types.MessageRequest) (*types.MessageRequest, error)
	Send                  AttachmentSink
	AllowClientTools      bool
}

type ManagerConfig struct {
	ReplayEventLimit int
	ReplayByteLimit  int
	ToolWaitTimeout  time.Duration
	ChainIdleTTL     time.Duration
}

func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		ReplayEventLimit: 1024,
		ReplayByteLimit:  1 << 20,
		ToolWaitTimeout:  30 * time.Second,
		ChainIdleTTL:     15 * time.Minute,
	}
}

type Manager struct {
	store Store
	cfg   ManagerConfig

	mu     sync.RWMutex
	chains map[string]*hotChain
}

type hotChain struct {
	mu sync.Mutex

	record      *types.ChainRecord
	history     []types.Message
	replay      []replayEntry
	replayBytes int
	nextEventID int64

	orgID                  string
	createdByPrincipalID   string
	createdByPrincipalType string
	actorID                string
	resumeTokenHash        string

	writer    *writerLease
	activeRun *activeRun
}

type replayEntry struct {
	EventID int64
	Data    []byte
}

type writerLease struct {
	ID              string
	Mode            types.AttachmentMode
	Protocol        string
	Principal       Principal
	Scope           CredentialScope
	StartedAt       time.Time
	Sink            AttachmentSink
	ResumeTokenHash string
}

type activeRun struct {
	record *types.ChainRunRecord

	chain        *hotChain
	env          RuntimeEnvironment
	manager      *Manager
	gatewayTools *servertools.Registry

	mu       sync.Mutex
	updateCh chan struct{}
	pending  map[string]*pendingExecution

	done            chan struct{}
	cancel          context.CancelFunc
	attachmentID    string
	ephemeralWriter bool

	result *types.RunResult
	err    error
}

type pendingExecution struct {
	ExecutionID string
	Name        string
	Input       map[string]any
	DeadlineAt  time.Time
	EffectClass types.ToolEffectClass
	CallIndex   int

	Resolved    bool
	Content     []types.ContentBlock
	IsError     bool
	PayloadHash string
}

func NewManager(store Store, cfg ManagerConfig) *Manager {
	if store == nil {
		store = NewMemoryStore()
	}
	if cfg.ReplayEventLimit <= 0 || cfg.ReplayByteLimit <= 0 || cfg.ToolWaitTimeout <= 0 || cfg.ChainIdleTTL <= 0 {
		cfg = DefaultManagerConfig()
	}
	return &Manager{
		store:  store,
		cfg:    cfg,
		chains: make(map[string]*hotChain),
	}
}

func (m *Manager) StartChain(ctx context.Context, principal Principal, env RuntimeEnvironment, payload types.ChainStartPayload, idempotencyKey string) (*types.ChainStartedEvent, string, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, "", types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "idempotency_key is required")
	}
	requestHash := payloadHash(payload)
	if existing, err := m.store.GetIdempotency(ctx, IdempotencyScope{
		OrgID:          principal.OrgID,
		PrincipalID:    principal.PrincipalID,
		Operation:      "chain.start",
		IdempotencyKey: idempotencyKey,
	}); err == nil && existing != nil {
		if existing.RequestHash != requestHash {
			return nil, "", types.NewCanonicalError(types.ErrorCodeToolResultConflict, "idempotent request payload conflict")
		}
		if chainID, _ := existing.ResultRef["chain_id"].(string); chainID != "" {
			if chain, err := m.requireChain(ctx, chainID); err == nil {
				return m.buildStartedEvent(ctx, chain, env.Scope, payload.ExternalSessionID), "", nil
			}
		}
	}

	now := time.Now().UTC()
	session := m.resolveOrCreateSession(ctx, principal, payload.ExternalSessionID, now)
	chainID := newID("chain")
	record := &types.ChainRecord{
		ID:                     chainID,
		OrgID:                  principal.OrgID,
		SessionID:              session.ID,
		ExternalSessionID:      payload.ExternalSessionID,
		CreatedByPrincipalID:   principal.PrincipalID,
		CreatedByPrincipalType: principal.PrincipalType,
		ActorID:                principal.ActorID,
		Status:                 types.ChainStatusIdle,
		ChainVersion:           1,
		Defaults:               cloneChainDefaults(payload.Defaults),
		Metadata:               cloneJSON(payload.Metadata),
		MessageCountCached:     len(payload.History),
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	hot := &hotChain{
		record:                 record,
		history:                cloneMessages(payload.History),
		orgID:                  principal.OrgID,
		createdByPrincipalID:   principal.PrincipalID,
		createdByPrincipalType: principal.PrincipalType,
		actorID:                principal.ActorID,
	}
	resumeToken := issueResumeToken()
	hot.resumeTokenHash = hashToken(resumeToken)
	if env.Send != nil {
		lease := &writerLease{
			ID:              newID("attach"),
			Mode:            env.Mode,
			Protocol:        env.Protocol,
			Principal:       principal,
			Scope:           env.Scope,
			StartedAt:       now,
			Sink:            env.Send,
			ResumeTokenHash: hot.resumeTokenHash,
		}
		record.ActiveAttachment = &types.ActiveAttachmentInfo{Mode: env.Mode, StartedAt: now}
		hot.writer = lease
	}
	if err := m.store.UpsertSession(ctx, session); err != nil {
		return nil, "", err
	}
	if err := m.store.SaveChain(ctx, record, hot.history); err != nil {
		return nil, "", err
	}
	if err := m.store.SetChainResumeTokenHash(ctx, chainID, hot.resumeTokenHash); err != nil {
		return nil, "", err
	}
	if hot.writer != nil {
		if err := m.store.SaveAttachment(ctx, &AttachmentRecord{
			ID:              hot.writer.ID,
			OrgID:           principal.OrgID,
			ChainID:         chainID,
			PrincipalID:     principal.PrincipalID,
			PrincipalType:   principal.PrincipalType,
			ActorID:         principal.ActorID,
			AttachmentRole:  types.AttachmentRoleWriter,
			Mode:            env.Mode,
			Protocol:        env.Protocol,
			ResumeTokenHash: hot.resumeTokenHash,
			StartedAt:       now,
		}); err != nil {
			return nil, "", err
		}
	}

	m.mu.Lock()
	m.chains[chainID] = hot
	m.mu.Unlock()

	event := m.buildStartedEvent(ctx, hot, env.Scope, payload.ExternalSessionID)
	event.ResumeToken = resumeToken
	event.EventID = hot.nextEvent()
	if err := hot.bufferAndEmit(m.cfg, event); err != nil {
		return nil, "", err
	}
	if err := m.store.SaveIdempotency(ctx, &IdempotencyRecord{
		ID:             newID("idem"),
		OrgID:          principal.OrgID,
		PrincipalID:    principal.PrincipalID,
		Operation:      "chain.start",
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash,
		ResultRef: map[string]any{
			"chain_id":   chainID,
			"session_id": session.ID,
		},
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		return nil, "", err
	}
	attachmentID := ""
	if hot.writer != nil {
		attachmentID = hot.writer.ID
	}
	return event, attachmentID, nil
}

func (m *Manager) AttachChain(ctx context.Context, principal Principal, env RuntimeEnvironment, frame types.ChainAttachFrame) (*types.ChainAttachedEvent, []types.ChainServerEvent, string, error) {
	chain, err := m.requireChain(ctx, frame.ChainID)
	if err != nil {
		return nil, nil, "", types.NewCanonicalError(types.ErrorCodeAuthResumeTokenInvalid, "chain is not resumable").WithChain(frame.ChainID)
	}

	chain.mu.Lock()

	attachmentActorID, authErr := m.authorizeAttach(chain, principal, frame.ResumeToken)
	if authErr != nil {
		chain.mu.Unlock()
		return nil, nil, "", authErr.WithChain(frame.ChainID)
	}
	if chain.record.Status == types.ChainStatusExpired {
		chain.mu.Unlock()
		return nil, nil, "", types.NewCanonicalError(types.ErrorCodeChainExpired, "chain is expired").WithChain(frame.ChainID)
	}
	if chain.writer != nil && !frame.Takeover {
		chain.mu.Unlock()
		return nil, nil, "", types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain already has an active attachment").WithChain(frame.ChainID)
	}
	if chain.writer != nil && frame.Takeover {
		_ = m.store.CloseAttachment(ctx, chain.writer.ID, string(types.ErrorCodeChainAttachConflict), time.Now().UTC())
	}

	replayStatus, replayEvents, replayErr := chain.replayAfter(frame.AfterEventID, frame.RequireExactReplay)
	if replayErr != nil {
		chain.mu.Unlock()
		return nil, nil, "", replayErr.WithChain(frame.ChainID)
	}
	resumeToken := issueResumeToken()
	chain.resumeTokenHash = hashToken(resumeToken)
	lease := &writerLease{
		ID:              newID("attach"),
		Mode:            env.Mode,
		Protocol:        env.Protocol,
		Principal:       principal,
		Scope:           env.Scope,
		StartedAt:       time.Now().UTC(),
		Sink:            env.Send,
		ResumeTokenHash: chain.resumeTokenHash,
	}
	chain.writer = lease
	chain.record.ActiveAttachment = &types.ActiveAttachmentInfo{Mode: env.Mode, StartedAt: lease.StartedAt}
	chain.record.UpdatedAt = lease.StartedAt
	if err := m.store.UpdateChain(ctx, chain.record); err != nil {
		chain.mu.Unlock()
		return nil, nil, "", err
	}
	if err := m.store.SetChainResumeTokenHash(ctx, chain.record.ID, chain.resumeTokenHash); err != nil {
		chain.mu.Unlock()
		return nil, nil, "", err
	}
	if err := m.store.SaveAttachment(ctx, &AttachmentRecord{
		ID:              lease.ID,
		OrgID:           principal.OrgID,
		ChainID:         chain.record.ID,
		PrincipalID:     principal.PrincipalID,
		PrincipalType:   principal.PrincipalType,
		ActorID:         attachmentActorID,
		AttachmentRole:  types.AttachmentRoleWriter,
		Mode:            env.Mode,
		Protocol:        env.Protocol,
		ResumeTokenHash: chain.resumeTokenHash,
		StartedAt:       lease.StartedAt,
	}); err != nil {
		chain.mu.Unlock()
		return nil, nil, "", err
	}
	chain.nextEventID++
	event := types.ChainAttachedEvent{
		Type:                   "chain.attached",
		EventID:                chain.nextEventID,
		ChainVersion:           chain.record.ChainVersion,
		ChainID:                chain.record.ID,
		SessionID:              chain.record.SessionID,
		ResumeToken:            resumeToken,
		ActorID:                attachmentActorID,
		AuthorizedProviders:    env.Scope.AuthorizedProviders(),
		AuthorizedGatewayTools: env.Scope.AuthorizedGatewayTools(),
		ReplayStatus:           replayStatus,
	}
	if chain.activeRun != nil && chain.activeRun.record != nil {
		event.ActiveRun = &types.ActiveRunInfo{
			RunID:     chain.activeRun.record.ID,
			Status:    chain.activeRun.record.Status,
			StartedAt: chain.activeRun.record.StartedAt,
		}
	}
	chain.mu.Unlock()
	if err := chain.bufferAndEmit(m.cfg, event); err != nil {
		return nil, nil, "", err
	}
	return &event, replayEvents, lease.ID, nil
}

func (m *Manager) UpdateChain(ctx context.Context, chainID string, env RuntimeEnvironment, payload types.ChainUpdatePayload, idempotencyKey string) (*types.ChainUpdatedEvent, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "idempotency_key is required").WithChain(chainID)
	}
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return nil, ErrNotFound
	}
	if authErr := m.authorizeChainMutation(chain, env.Principal); authErr != nil {
		return nil, authErr.WithChain(chainID)
	}
	requestHash := payloadHash(payload)
	if existing, err := m.store.GetIdempotency(ctx, IdempotencyScope{
		OrgID:          env.Principal.OrgID,
		PrincipalID:    env.Principal.PrincipalID,
		ChainID:        chainID,
		Operation:      "chain.update",
		IdempotencyKey: idempotencyKey,
	}); err == nil && existing != nil {
		if existing.RequestHash != requestHash {
			return nil, types.NewCanonicalError(types.ErrorCodeToolResultConflict, "idempotent request payload conflict").WithChain(chainID)
		}
		record, getErr := m.GetChain(ctx, chainID)
		if getErr != nil {
			return nil, getErr
		}
		return &types.ChainUpdatedEvent{
			Type:         "chain.updated",
			ChainID:      record.ID,
			ChainVersion: record.ChainVersion,
			Defaults:     cloneChainDefaults(record.Defaults),
		}, nil
	}
	chain.mu.Lock()
	attachmentID := ""
	closeAttachment := false
	if env.Mode == types.AttachmentModeStatefulHTTP || env.Mode == types.AttachmentModeStatefulSSE {
		if chain.writer != nil {
			chain.mu.Unlock()
			return nil, types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain already has an active attachment").WithChain(chainID)
		}
		now := time.Now().UTC()
		lease := &writerLease{
			ID:       newID("attach"),
			Mode:     env.Mode,
			Protocol: env.Protocol,
			Principal: Principal{
				OrgID:         env.Principal.OrgID,
				PrincipalID:   env.Principal.PrincipalID,
				PrincipalType: env.Principal.PrincipalType,
				ActorID:       env.Principal.ActorID,
			},
			Scope:     env.Scope,
			StartedAt: now,
			Sink:      env.Send,
		}
		chain.writer = lease
		chain.record.ActiveAttachment = &types.ActiveAttachmentInfo{Mode: env.Mode, StartedAt: now}
		attachmentID = lease.ID
		closeAttachment = true
	} else if chain.writer == nil || chain.writer.Mode != env.Mode {
		chain.mu.Unlock()
		return nil, types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain is not attached on this transport").WithChain(chainID)
	}
	chain.record.Defaults = mergeDefaults(chain.record.Defaults, payload.Defaults)
	chain.record.ChainVersion++
	chain.record.UpdatedAt = time.Now().UTC()
	record := cloneJSON(chain.record)
	if err := m.store.UpdateChain(ctx, chain.record); err != nil {
		chain.mu.Unlock()
		return nil, err
	}
	chain.nextEventID++
	event := types.ChainUpdatedEvent{
		Type:         "chain.updated",
		EventID:      chain.nextEventID,
		ChainVersion: chain.record.ChainVersion,
		ChainID:      chain.record.ID,
		Defaults:     cloneChainDefaults(chain.record.Defaults),
	}
	chain.mu.Unlock()
	if attachmentID != "" {
		if err := m.store.SaveAttachment(ctx, &AttachmentRecord{
			ID:             attachmentID,
			OrgID:          env.Principal.OrgID,
			ChainID:        chainID,
			PrincipalID:    env.Principal.PrincipalID,
			PrincipalType:  env.Principal.PrincipalType,
			ActorID:        env.Principal.ActorID,
			AttachmentRole: types.AttachmentRoleWriter,
			Mode:           env.Mode,
			Protocol:       env.Protocol,
			StartedAt:      record.UpdatedAt,
		}); err != nil {
			return nil, err
		}
	}
	if err := chain.bufferAndEmit(m.cfg, event); err != nil {
		return nil, err
	}
	if err := m.store.SaveIdempotency(ctx, &IdempotencyRecord{
		ID:             newID("idem"),
		OrgID:          env.Principal.OrgID,
		PrincipalID:    env.Principal.PrincipalID,
		ChainID:        chainID,
		Operation:      "chain.update",
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash,
		ResultRef:      map[string]any{"chain_id": chainID},
		CreatedAt:      record.UpdatedAt,
		ExpiresAt:      record.UpdatedAt.Add(time.Hour),
	}); err != nil {
		return nil, err
	}
	if closeAttachment {
		_ = m.Detach(ctx, chainID, attachmentID, "request_complete")
	}
	return &event, nil
}

func (m *Manager) Detach(ctx context.Context, chainID, attachmentID, reason string) error {
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return err
	}
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.writer == nil {
		return nil
	}
	if attachmentID != "" && chain.writer.ID != attachmentID {
		return nil
	}
	actualAttachmentID := chain.writer.ID
	chain.writer = nil
	chain.record.ActiveAttachment = nil
	chain.record.UpdatedAt = time.Now().UTC()
	_ = m.store.UpdateChain(ctx, chain.record)
	_ = m.store.CloseAttachment(ctx, actualAttachmentID, reason, time.Now().UTC())
	return nil
}

func (m *Manager) GetSession(ctx context.Context, sessionID string) (*types.SessionRecord, error) {
	return m.store.GetSession(ctx, sessionID)
}

func (m *Manager) ListSessions(ctx context.Context, principal Principal) ([]types.SessionRecord, error) {
	if authErr := authorizeOrgAccess(principal.OrgID, principal.OrgID); authErr != nil {
		return nil, authErr
	}
	return m.store.ListSessions(ctx, principal.OrgID)
}

func (m *Manager) GetSessionAuthorized(ctx context.Context, principal Principal, sessionID string) (*types.SessionRecord, error) {
	record, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if authErr := authorizeOrgAccess(record.OrgID, principal.OrgID); authErr != nil {
		return nil, authErr.WithChain(record.LatestChainID)
	}
	return record, nil
}

func (m *Manager) ListSessionChains(ctx context.Context, sessionID string) ([]types.ChainRecord, error) {
	chains, err := m.store.ListSessionChains(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range chains {
		if hot := m.hotChain(chains[i].ID); hot != nil {
			hot.mu.Lock()
			if hot.writer != nil {
				chains[i].ActiveAttachment = &types.ActiveAttachmentInfo{
					Mode:      hot.writer.Mode,
					StartedAt: hot.writer.StartedAt,
				}
			}
			chains[i].Status = hot.record.Status
			chains[i].ChainVersion = hot.record.ChainVersion
			hot.mu.Unlock()
		}
	}
	return chains, nil
}

func (m *Manager) ListSessionChainsAuthorized(ctx context.Context, principal Principal, sessionID string) ([]types.ChainRecord, error) {
	session, err := m.GetSessionAuthorized(ctx, principal, sessionID)
	if err != nil {
		return nil, err
	}
	return m.ListSessionChains(ctx, session.ID)
}

func (m *Manager) ListChains(ctx context.Context, principal Principal, sessionID string, unsessionedOnly bool) ([]types.ChainRecord, error) {
	if authErr := authorizeOrgAccess(principal.OrgID, principal.OrgID); authErr != nil {
		return nil, authErr
	}
	chains, err := m.store.ListChains(ctx, principal.OrgID, sessionID, unsessionedOnly)
	if err != nil {
		return nil, err
	}
	for i := range chains {
		if hot := m.hotChain(chains[i].ID); hot != nil {
			hot.mu.Lock()
			chains[i].Status = hot.record.Status
			chains[i].ChainVersion = hot.record.ChainVersion
			chains[i].UpdatedAt = hot.record.UpdatedAt
			chains[i].Defaults = cloneChainDefaults(hot.record.Defaults)
			if hot.writer != nil {
				chains[i].ActiveAttachment = &types.ActiveAttachmentInfo{
					Mode:      hot.writer.Mode,
					StartedAt: hot.writer.StartedAt,
				}
			} else {
				chains[i].ActiveAttachment = nil
			}
			hot.mu.Unlock()
		}
	}
	return chains, nil
}

func (m *Manager) GetChain(ctx context.Context, chainID string) (*types.ChainRecord, error) {
	record, _, err := m.store.GetChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if hot := m.hotChain(chainID); hot != nil {
		hot.mu.Lock()
		record.Status = hot.record.Status
		record.ChainVersion = hot.record.ChainVersion
		record.UpdatedAt = hot.record.UpdatedAt
		record.Defaults = cloneChainDefaults(hot.record.Defaults)
		if hot.writer != nil {
			record.ActiveAttachment = &types.ActiveAttachmentInfo{
				Mode:      hot.writer.Mode,
				StartedAt: hot.writer.StartedAt,
			}
		} else {
			record.ActiveAttachment = nil
		}
		hot.mu.Unlock()
	}
	return record, nil
}

func (m *Manager) GetChainAuthorized(ctx context.Context, principal Principal, chainID string) (*types.ChainRecord, error) {
	record, err := m.GetChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if authErr := authorizeOrgAccess(record.OrgID, principal.OrgID); authErr != nil {
		return nil, authErr.WithChain(chainID)
	}
	return record, nil
}

func (m *Manager) GetChainForMutation(ctx context.Context, principal Principal, chainID string) (*types.ChainRecord, error) {
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if authErr := m.authorizeChainMutation(chain, principal); authErr != nil {
		return nil, authErr.WithChain(chainID)
	}
	return m.GetChain(ctx, chainID)
}

func (m *Manager) GetChainContext(ctx context.Context, chainID string) (*types.ChainContextResponse, error) {
	record, history, err := m.store.GetChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	return &types.ChainContextResponse{
		ChainID:  record.ID,
		Messages: history,
	}, nil
}

func (m *Manager) GetChainContextAuthorized(ctx context.Context, principal Principal, chainID string) (*types.ChainContextResponse, error) {
	record, err := m.GetChainAuthorized(ctx, principal, chainID)
	if err != nil {
		return nil, err
	}
	resp, err := m.GetChainContext(ctx, chainID)
	if err != nil {
		return nil, err
	}
	resp.ChainID = record.ID
	return resp, nil
}

func (m *Manager) ListRuns(ctx context.Context, chainID string) ([]types.ChainRunRecord, error) {
	return m.store.ListChainRuns(ctx, chainID)
}

func (m *Manager) ListRunsAuthorized(ctx context.Context, principal Principal, chainID string) ([]types.ChainRunRecord, error) {
	if _, err := m.GetChainAuthorized(ctx, principal, chainID); err != nil {
		return nil, err
	}
	return m.ListRuns(ctx, chainID)
}

func (m *Manager) GetRun(ctx context.Context, runID string) (*types.ChainRunRecord, error) {
	return m.store.GetRun(ctx, runID)
}

func (m *Manager) GetRunAuthorized(ctx context.Context, principal Principal, runID string) (*types.ChainRunRecord, error) {
	run, err := m.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if authErr := authorizeOrgAccess(run.OrgID, principal.OrgID); authErr != nil {
		return nil, authErr.WithChain(run.ChainID).WithRun(run.ID)
	}
	return run, nil
}

func (m *Manager) GetRunTimeline(ctx context.Context, runID string) (*types.RunTimelineResponse, error) {
	items, err := m.store.GetRunTimeline(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &types.RunTimelineResponse{RunID: runID, Items: items}, nil
}

func (m *Manager) GetRunTimelineAuthorized(ctx context.Context, principal Principal, runID string) (*types.RunTimelineResponse, error) {
	run, err := m.GetRunAuthorized(ctx, principal, runID)
	if err != nil {
		return nil, err
	}
	resp, err := m.GetRunTimeline(ctx, runID)
	if err != nil {
		return nil, err
	}
	resp.RunID = run.ID
	return resp, nil
}

func (m *Manager) GetEffectiveRequest(ctx context.Context, runID string) (*types.EffectiveRequestResponse, error) {
	return m.store.GetEffectiveRequest(ctx, runID)
}

func (m *Manager) GetEffectiveRequestAuthorized(ctx context.Context, principal Principal, runID string) (*types.EffectiveRequestResponse, error) {
	run, err := m.GetRunAuthorized(ctx, principal, runID)
	if err != nil {
		return nil, err
	}
	resp, err := m.GetEffectiveRequest(ctx, runID)
	if err != nil {
		return nil, err
	}
	resp.RunID = run.ID
	return resp, nil
}

func (m *Manager) OwnsAttachment(ctx context.Context, chainID, attachmentID string) bool {
	if strings.TrimSpace(chainID) == "" || strings.TrimSpace(attachmentID) == "" {
		return false
	}
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return false
	}
	chain.mu.Lock()
	defer chain.mu.Unlock()
	return chain.writer != nil && chain.writer.ID == attachmentID
}

func (m *Manager) hotChain(chainID string) *hotChain {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.chains[chainID]
}

func (m *Manager) requireChain(ctx context.Context, chainID string) (*hotChain, error) {
	if hot := m.hotChain(chainID); hot != nil {
		return hot, nil
	}
	record, history, err := m.store.GetChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	resumeTokenHash, err := m.store.GetChainResumeTokenHash(ctx, chainID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	hot := &hotChain{
		record:                 record,
		history:                history,
		orgID:                  record.OrgID,
		createdByPrincipalID:   record.CreatedByPrincipalID,
		createdByPrincipalType: record.CreatedByPrincipalType,
		actorID:                record.ActorID,
		resumeTokenHash:        resumeTokenHash,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.chains[chainID]; existing != nil {
		return existing, nil
	}
	m.chains[chainID] = hot
	return hot, nil
}

func (m *Manager) resolveOrCreateSession(ctx context.Context, principal Principal, externalSessionID string, now time.Time) *types.SessionRecord {
	if externalSessionID != "" {
		if session, err := m.store.GetSessionByExternal(ctx, principal.OrgID, externalSessionID); err == nil && session != nil {
			session.UpdatedAt = now
			return session
		}
	}
	return &types.SessionRecord{
		ID:                     newID("sess"),
		OrgID:                  principal.OrgID,
		ExternalSessionID:      externalSessionID,
		CreatedByPrincipalID:   principal.PrincipalID,
		CreatedByPrincipalType: principal.PrincipalType,
		ActorID:                principal.ActorID,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
}

func (m *Manager) buildStartedEvent(_ context.Context, chain *hotChain, scope CredentialScope, externalSessionID string) *types.ChainStartedEvent {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	return &types.ChainStartedEvent{
		Type:                   "chain.started",
		ChainVersion:           chain.record.ChainVersion,
		ChainID:                chain.record.ID,
		SessionID:              chain.record.SessionID,
		ExternalSessionID:      externalSessionID,
		ActorID:                chain.actorID,
		AuthorizedProviders:    scope.AuthorizedProviders(),
		AuthorizedGatewayTools: scope.AuthorizedGatewayTools(),
		Defaults:               cloneChainDefaults(chain.record.Defaults),
	}
}

func (m *Manager) authorizeAttach(chain *hotChain, principal Principal, resumeToken string) (string, *types.CanonicalError) {
	chainID := ""
	if chain != nil && chain.record != nil {
		chainID = chain.record.ID
	}
	if strings.TrimSpace(principal.OrgID) == "" || strings.TrimSpace(chain.orgID) == "" || strings.TrimSpace(principal.OrgID) != strings.TrimSpace(chain.orgID) {
		return "", types.NewCanonicalError(types.ErrorCodeAuthResumeTokenInvalid, "resume token is invalid").WithChain(chainID)
	}
	if chain.resumeTokenHash == "" || hashToken(resumeToken) != chain.resumeTokenHash {
		return "", types.NewCanonicalError(types.ErrorCodeAuthResumeTokenInvalid, "resume token is invalid").WithChain(chainID)
	}
	if chain.actorID != "" {
		if strings.TrimSpace(principal.ActorID) == "" || strings.TrimSpace(principal.ActorID) != strings.TrimSpace(chain.actorID) {
			return "", types.NewCanonicalError(types.ErrorCodeAuthActorScopeDenied, "actor is not authorized for this chain").WithChain(chainID)
		}
		return chain.actorID, nil
	}
	return strings.TrimSpace(principal.ActorID), nil
}

func (m *Manager) authorizeChainMutation(chain *hotChain, principal Principal) *types.CanonicalError {
	chainID := ""
	if chain != nil && chain.record != nil {
		chainID = chain.record.ID
	}
	if authErr := authorizeOrgAccess(chain.orgID, principal.OrgID); authErr != nil {
		return authErr.WithChain(chainID)
	}
	return nil
}

func authorizeOrgAccess(resourceOrgID, callerOrgID string) *types.CanonicalError {
	if strings.TrimSpace(callerOrgID) == "" || strings.TrimSpace(resourceOrgID) == "" || strings.TrimSpace(callerOrgID) != strings.TrimSpace(resourceOrgID) {
		return types.NewCanonicalError(types.ErrorCodeAuthChainAccessDenied, "principal is not authorized for this chain")
	}
	return nil
}

func (c *hotChain) nextEvent() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextEventID++
	return c.nextEventID
}

func (c *hotChain) bufferAndEmit(cfg ManagerConfig, event types.ChainServerEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.replay = append(c.replay, replayEntry{
		EventID: eventID(event),
		Data:    data,
	})
	c.replayBytes += len(data)
	for len(c.replay) > cfg.ReplayEventLimit || c.replayBytes > cfg.ReplayByteLimit {
		c.replayBytes -= len(c.replay[0].Data)
		c.replay = c.replay[1:]
	}
	sink := c.writer
	c.mu.Unlock()
	if sink == nil || sink.Sink == nil {
		return nil
	}
	return sink.Sink(event)
}

func (c *hotChain) replayAfter(afterEventID int64, requireExact bool) (types.ReplayStatus, []types.ChainServerEvent, *types.CanonicalError) {
	if afterEventID <= 0 {
		return types.ReplayStatusNone, nil, nil
	}
	if len(c.replay) == 0 {
		if requireExact {
			return "", nil, types.NewCanonicalError(types.ErrorCodeChainReplayUnavailable, "exact replay is unavailable")
		}
		return types.ReplayStatusNone, nil, nil
	}
	start := -1
	for i := range c.replay {
		if c.replay[i].EventID > afterEventID {
			start = i
			break
		}
	}
	if start == -1 {
		return types.ReplayStatusNone, nil, nil
	}
	if c.replay[0].EventID > afterEventID+1 {
		if requireExact {
			return "", nil, types.NewCanonicalError(types.ErrorCodeChainReplayUnavailable, "exact replay is unavailable")
		}
		return types.ReplayStatusPartial, nil, nil
	}
	out := make([]types.ChainServerEvent, 0, len(c.replay)-start)
	for i := start; i < len(c.replay); i++ {
		event, err := types.UnmarshalChainServerEventStrict(c.replay[i].Data)
		if err != nil {
			continue
		}
		out = append(out, event)
	}
	return types.ReplayStatusFull, out, nil
}

func mergeDefaults(current, patch types.ChainDefaults) types.ChainDefaults {
	if strings.TrimSpace(patch.Model) != "" {
		current.Model = patch.Model
	}
	if patch.System != nil {
		current.System = normalizedMessageContent(patch.System)
	}
	if patch.Tools != nil {
		current.Tools = cloneJSON(patch.Tools)
	}
	if patch.GatewayTools != nil {
		current.GatewayTools = append([]string(nil), patch.GatewayTools...)
	}
	if patch.GatewayToolConfig != nil {
		current.GatewayToolConfig = cloneJSON(patch.GatewayToolConfig)
	}
	if patch.ToolChoice != nil {
		current.ToolChoice = cloneJSON(patch.ToolChoice)
	}
	if patch.MaxTokens != 0 {
		current.MaxTokens = patch.MaxTokens
	}
	if patch.Temperature != nil {
		current.Temperature = cloneJSON(patch.Temperature)
	}
	if patch.TopP != nil {
		current.TopP = cloneJSON(patch.TopP)
	}
	if patch.TopK != nil {
		current.TopK = cloneJSON(patch.TopK)
	}
	if patch.StopSequences != nil {
		current.StopSequences = append([]string(nil), patch.StopSequences...)
	}
	if patch.STTModel != "" {
		current.STTModel = patch.STTModel
	}
	if patch.TTSModel != "" {
		current.TTSModel = patch.TTSModel
	}
	if patch.OutputFormat != nil {
		current.OutputFormat = cloneJSON(patch.OutputFormat)
	}
	if patch.Output != nil {
		current.Output = cloneJSON(patch.Output)
	}
	if patch.Voice != nil {
		current.Voice = cloneJSON(patch.Voice)
	}
	if patch.Extensions != nil {
		current.Extensions = cloneJSON(patch.Extensions)
	}
	if patch.Metadata != nil {
		current.Metadata = cloneJSON(patch.Metadata)
	}
	return current
}

func splitModel(model string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func issueResumeToken() string {
	return "chain_rt_" + randomHex(18)
}

func payloadHash(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) string {
	return prefix + "_" + randomHex(10)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func eventID(event types.ChainServerEvent) int64 {
	switch value := event.(type) {
	case types.ChainStartedEvent:
		return value.EventID
	case *types.ChainStartedEvent:
		if value != nil {
			return value.EventID
		}
	case types.ChainAttachedEvent:
		return value.EventID
	case *types.ChainAttachedEvent:
		if value != nil {
			return value.EventID
		}
	case types.ChainUpdatedEvent:
		return value.EventID
	case *types.ChainUpdatedEvent:
		if value != nil {
			return value.EventID
		}
	case types.RunEnvelopeEvent:
		return value.EventID
	case *types.RunEnvelopeEvent:
		if value != nil {
			return value.EventID
		}
	case types.ClientToolCallEvent:
		return value.EventID
	case *types.ClientToolCallEvent:
		if value != nil {
			return value.EventID
		}
	case types.ChainErrorEvent:
		return value.EventID
	case *types.ChainErrorEvent:
		if value != nil {
			return value.EventID
		}
	}
	return 0
}

func waitForRun(run *activeRun) (*types.RunResult, error) {
	if run == nil {
		return nil, errors.New("run is nil")
	}
	<-run.done
	return run.result, run.err
}
