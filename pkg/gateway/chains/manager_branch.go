package chains

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type ForkPreview struct {
	SourceChain     *types.ChainRecord
	History         []types.Message
	Input           []types.Message
	Defaults        types.ChainDefaults
	Metadata        map[string]any
	ForkedFromRunID string
}

func (m *Manager) PreviewForkChain(ctx context.Context, principal Principal, sourceChainID string, payload types.ChainForkRequest) (*ForkPreview, error) {
	source, err := m.requireChain(ctx, sourceChainID)
	if err != nil {
		return nil, err
	}
	if authErr := m.authorizeChainMutation(source, principal); authErr != nil {
		return nil, authErr.WithChain(sourceChainID)
	}
	source.mu.Lock()
	record := cloneJSON(*source.record)
	history := cloneMessages(source.history)
	source.mu.Unlock()
	return &ForkPreview{
		SourceChain:     &record,
		History:         valueOrCloneMessages(payload.History, history),
		Defaults:        mergeDefaults(cloneChainDefaults(record.Defaults), payload.Defaults),
		Metadata:        mergeMetadata(record.Metadata, payload.Metadata),
		ForkedFromRunID: strings.TrimSpace(payload.ForkedFromRunID),
	}, nil
}

func (m *Manager) PreviewRegenerateRun(ctx context.Context, principal Principal, runID string, payload types.RunRegenerateRequest) (*ForkPreview, error) {
	run, err := m.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	chain, err := m.requireChain(ctx, run.ChainID)
	if err != nil {
		return nil, err
	}
	if authErr := m.authorizeChainMutation(chain, principal); authErr != nil {
		return nil, authErr.WithChain(run.ChainID).WithRun(runID)
	}
	if run.Status != types.RunStatusCompleted {
		return nil, core.NewInvalidRequestError("only completed runs can be regenerated")
	}
	chainMessages, err := m.store.ListChainMessages(ctx, run.ChainID)
	if err != nil {
		return nil, err
	}
	runMessages := make([]types.ChainMessageRecord, 0)
	firstRunIndex := -1
	for i := range chainMessages {
		if strings.TrimSpace(chainMessages[i].RunID) != run.ID {
			continue
		}
		if firstRunIndex < 0 {
			firstRunIndex = i
		}
		runMessages = append(runMessages, chainMessages[i])
	}
	if firstRunIndex < 0 || len(runMessages) == 0 {
		return nil, errors.New("run history is incomplete")
	}
	inputCount := runInputMessageCount(run)
	if inputCount <= 0 || inputCount > len(runMessages) {
		inputCount = inferredRunInputCount(runMessages)
	}
	if inputCount <= 0 || inputCount > len(runMessages) {
		return nil, errors.New("run input is unavailable for regeneration")
	}
	chain.mu.Lock()
	record := cloneJSON(*chain.record)
	chain.mu.Unlock()
	return &ForkPreview{
		SourceChain:     &record,
		History:         messagesFromRecords(chainMessages[:firstRunIndex]),
		Input:           messagesFromRecords(runMessages[:inputCount]),
		Defaults:        mergeDefaults(cloneChainDefaults(run.EffectiveConfig), payload.Defaults),
		Metadata:        mergeMetadata(record.Metadata, payload.Metadata),
		ForkedFromRunID: run.ID,
	}, nil
}

func (m *Manager) ForkChain(ctx context.Context, principal Principal, sourceChainID string, payload types.ChainForkRequest, idempotencyKey string) (*types.ChainForkResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "idempotency_key is required").WithChain(sourceChainID)
	}
	preview, err := m.PreviewForkChain(ctx, principal, sourceChainID, payload)
	if err != nil {
		return nil, err
	}
	requestHash := payloadHash(payload)
	if existing, err := m.store.GetIdempotency(ctx, IdempotencyScope{
		OrgID:          principal.OrgID,
		PrincipalID:    principal.PrincipalID,
		ChainID:        sourceChainID,
		Operation:      "chain.fork",
		IdempotencyKey: idempotencyKey,
	}); err == nil && existing != nil {
		if existing.RequestHash != requestHash {
			return nil, types.NewCanonicalError(types.ErrorCodeToolResultConflict, "idempotent request payload conflict").WithChain(sourceChainID)
		}
		if chainID, _ := existing.ResultRef["chain_id"].(string); chainID != "" {
			return m.forkResponse(ctx, chainID, preview.Input)
		}
	}
	return m.createForkedChain(ctx, principal, preview, "chain.fork", sourceChainID, idempotencyKey, requestHash)
}

func (m *Manager) RegenerateRun(ctx context.Context, principal Principal, runID string, payload types.RunRegenerateRequest, idempotencyKey string) (*types.ChainForkResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "idempotency_key is required").WithRun(runID)
	}
	preview, err := m.PreviewRegenerateRun(ctx, principal, runID, payload)
	if err != nil {
		return nil, err
	}
	sourceChainID := ""
	if preview.SourceChain != nil {
		sourceChainID = preview.SourceChain.ID
	}
	requestHash := payloadHash(payload)
	if existing, err := m.store.GetIdempotency(ctx, IdempotencyScope{
		OrgID:          principal.OrgID,
		PrincipalID:    principal.PrincipalID,
		ChainID:        sourceChainID,
		Operation:      "run.regenerate",
		IdempotencyKey: idempotencyKey,
	}); err == nil && existing != nil {
		if existing.RequestHash != requestHash {
			return nil, types.NewCanonicalError(types.ErrorCodeToolResultConflict, "idempotent request payload conflict").WithChain(sourceChainID).WithRun(runID)
		}
		if chainID, _ := existing.ResultRef["chain_id"].(string); chainID != "" {
			return m.forkResponse(ctx, chainID, preview.Input)
		}
	}
	return m.createForkedChain(ctx, principal, preview, "run.regenerate", sourceChainID, idempotencyKey, requestHash)
}

func (m *Manager) createForkedChain(ctx context.Context, principal Principal, preview *ForkPreview, operation, sourceChainID, idempotencyKey, requestHash string) (*types.ChainForkResponse, error) {
	if preview == nil || preview.SourceChain == nil {
		return nil, errors.New("fork preview is required")
	}
	now := time.Now().UTC()
	actorID := strings.TrimSpace(preview.SourceChain.ActorID)
	if actorID == "" {
		actorID = strings.TrimSpace(principal.ActorID)
	}
	record := &types.ChainRecord{
		ID:                     newID("chain"),
		OrgID:                  preview.SourceChain.OrgID,
		SessionID:              preview.SourceChain.SessionID,
		ExternalSessionID:      preview.SourceChain.ExternalSessionID,
		CreatedByPrincipalID:   principal.PrincipalID,
		CreatedByPrincipalType: principal.PrincipalType,
		ActorID:                actorID,
		Status:                 types.ChainStatusIdle,
		ChainVersion:           1,
		ParentChainID:          preview.SourceChain.ID,
		ForkedFromRunID:        strings.TrimSpace(preview.ForkedFromRunID),
		MessageCountCached:     len(preview.History),
		Defaults:               cloneChainDefaults(preview.Defaults),
		Metadata:               cloneJSON(preview.Metadata),
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	hot := &hotChain{
		record:                 record,
		history:                cloneMessages(preview.History),
		orgID:                  record.OrgID,
		createdByPrincipalID:   principal.PrincipalID,
		createdByPrincipalType: principal.PrincipalType,
		actorID:                actorID,
	}
	resumeToken := issueResumeToken()
	hot.resumeTokenHash = hashToken(resumeToken)
	if record.SessionID != "" {
		if session, err := m.store.GetSession(ctx, record.SessionID); err == nil && session != nil {
			session.UpdatedAt = now
			session.LatestChainID = record.ID
			if err := m.store.UpsertSession(ctx, session); err != nil {
				return nil, err
			}
		}
	}
	if err := m.store.SaveChain(ctx, record, hot.history); err != nil {
		return nil, err
	}
	if err := m.store.SetChainResumeTokenHash(ctx, record.ID, hot.resumeTokenHash); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.chains[record.ID] = hot
	m.mu.Unlock()
	if err := m.store.SaveIdempotency(ctx, &IdempotencyRecord{
		ID:             newID("idem"),
		OrgID:          principal.OrgID,
		PrincipalID:    principal.PrincipalID,
		ChainID:        sourceChainID,
		Operation:      operation,
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash,
		ResultRef: map[string]any{
			"chain_id": record.ID,
		},
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		return nil, err
	}
	return &types.ChainForkResponse{
		ChainID:           record.ID,
		SessionID:         record.SessionID,
		ExternalSessionID: record.ExternalSessionID,
		ParentChainID:     record.ParentChainID,
		ForkedFromRunID:   record.ForkedFromRunID,
		Defaults:          cloneChainDefaults(record.Defaults),
		ResumeToken:       resumeToken,
		Input:             cloneMessages(preview.Input),
	}, nil
}

func (m *Manager) forkResponse(ctx context.Context, chainID string, input []types.Message) (*types.ChainForkResponse, error) {
	record, err := m.GetChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	return &types.ChainForkResponse{
		ChainID:           record.ID,
		SessionID:         record.SessionID,
		ExternalSessionID: record.ExternalSessionID,
		ParentChainID:     record.ParentChainID,
		ForkedFromRunID:   record.ForkedFromRunID,
		Defaults:          cloneChainDefaults(record.Defaults),
		Input:             cloneMessages(input),
	}, nil
}

func valueOrCloneMessages(value, fallback []types.Message) []types.Message {
	if value != nil {
		return cloneMessages(value)
	}
	return cloneMessages(fallback)
}

func mergeMetadata(base, override map[string]any) map[string]any {
	if base == nil && override == nil {
		return nil
	}
	merged := cloneJSON(base)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range cloneJSON(override) {
		merged[key] = value
	}
	return merged
}

func messagesFromRecords(records []types.ChainMessageRecord) []types.Message {
	if len(records) == 0 {
		return nil
	}
	out := make([]types.Message, 0, len(records))
	for i := range records {
		out = append(out, types.Message{
			Role:    records[i].Role,
			Content: normalizedMessageContent(records[i].Content),
		})
	}
	return out
}

func runInputMessageCount(run *types.ChainRunRecord) int {
	if run == nil {
		return 0
	}
	meta := cloneJSON(run.Metadata)
	runtimeMeta, _ := meta["chain_runtime"].(map[string]any)
	switch value := runtimeMeta["input_message_count"].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func inferredRunInputCount(messages []types.ChainMessageRecord) int {
	if len(messages) == 0 {
		return 0
	}
	firstCreatedAt := messages[0].CreatedAt
	count := 0
	for i := range messages {
		if !messages[i].CreatedAt.Equal(firstCreatedAt) {
			break
		}
		count++
	}
	return count
}
