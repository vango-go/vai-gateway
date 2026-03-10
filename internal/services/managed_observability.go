package services

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	chainrt "github.com/vango-go/vai-lite/pkg/gateway/chains"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type GatewayManagedRunProjectionInput struct {
	RequestID            string
	OrgID                string
	ChainID              string
	ExternalSessionID    string
	ParentRequestID      string
	GatewayAPIKeyID      string
	GatewayAPIKeyName    string
	GatewayAPIKeyPrefix  string
	EndpointKind         string
	EndpointFamily       string
	Provider             string
	Model                string
	KeySource            KeySource
	AccessCredential     AccessCredential
	System               any
	Messages             []types.Message
	RunConfig            string
	RunResult            *types.RunResult
	ErrorSummary         string
	InputContextFingerprint string
	OutputContextFingerprint string
	StartedAt            time.Time
	CompletedAt          time.Time
}

type ManagedObservabilityRun struct {
	ID                 string           `json:"id"`
	ChainID            string           `json:"chain_id"`
	SessionID          string           `json:"session_id,omitempty"`
	ExternalSessionID  string           `json:"external_session_id,omitempty"`
	Provider           string           `json:"provider,omitempty"`
	Model              string           `json:"model,omitempty"`
	Status             types.RunStatus  `json:"status"`
	StopReason         string           `json:"stop_reason,omitempty"`
	ToolCount          int              `json:"tool_count"`
	DurationMS         int64            `json:"duration_ms"`
	StartedAt          time.Time        `json:"started_at"`
	CompletedAt        *time.Time       `json:"completed_at,omitempty"`
	Transport          string           `json:"transport,omitempty"`
	Protocol           string           `json:"protocol,omitempty"`
	EndpointKind       string           `json:"endpoint_kind,omitempty"`
	KeySource          KeySource        `json:"key_source,omitempty"`
	AccessCredential   AccessCredential `json:"access_credential,omitempty"`
	GatewayAPIKeyID    string           `json:"gateway_api_key_id,omitempty"`
	GatewayAPIKeyName  string           `json:"gateway_api_key_name,omitempty"`
	GatewayAPIKeyPref  string           `json:"gateway_api_key_prefix,omitempty"`
	ErrorSummary       string           `json:"error_summary,omitempty"`
}

type ManagedObservabilityChain struct {
	Chain            types.ChainRecord         `json:"chain"`
	SessionID        string                    `json:"session_id,omitempty"`
	ExternalSessionID string                   `json:"external_session_id,omitempty"`
	RunCount         int                       `json:"run_count"`
	LatestActivity   time.Time                 `json:"latest_activity"`
	Models           []string                  `json:"models,omitempty"`
	Transports       []string                  `json:"transports,omitempty"`
	Runs             []ManagedObservabilityRun `json:"runs"`
}

type ManagedObservabilitySession struct {
	Session        types.SessionRecord          `json:"session"`
	ChainCount     int                          `json:"chain_count"`
	RunCount       int                          `json:"run_count"`
	LatestActivity time.Time                    `json:"latest_activity"`
	Chains         []ManagedObservabilityChain  `json:"chains"`
}

type ManagedObservabilitySnapshot struct {
	Sessions         []ManagedObservabilitySession `json:"sessions"`
	UnsessionedChains []ManagedObservabilityChain  `json:"unsessioned_chains"`
}

type ManagedObservabilityRunDetail struct {
	Run             ManagedObservabilityRun        `json:"run"`
	Record          *types.ChainRunRecord         `json:"record,omitempty"`
	EffectiveRequest *types.EffectiveRequestResponse `json:"effective_request,omitempty"`
	Timeline        []types.RunTimelineItem       `json:"timeline,omitempty"`
}

func (s *AppServices) ProjectGatewayRunObservation(ctx context.Context, in GatewayManagedRunProjectionInput) error {
	store, err := s.managedChainStore()
	if err != nil {
		return err
	}
	requestID := strings.TrimSpace(in.RequestID)
	orgID := strings.TrimSpace(in.OrgID)
	chainID := strings.TrimSpace(in.ChainID)
	if requestID == "" || orgID == "" || chainID == "" {
		return errors.New("request_id, org_id, and chain_id are required")
	}

	now := in.CompletedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	startedAt := in.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = now
	}

	sessionRecord, err := s.ensureManagedObservationSession(ctx, store, orgID, in.ExternalSessionID, now)
	if err != nil {
		return err
	}

	chainRecord, history, err := s.ensureManagedObservationChain(ctx, store, in, sessionRecord, now)
	if err != nil {
		return err
	}
	if err := store.SaveChain(ctx, chainRecord, history); err != nil {
		return err
	}

	parentRunID := strings.TrimSpace(in.ParentRequestID)
	if parentRunID == "" {
		runs, listErr := store.ListChainRuns(ctx, chainID)
		if listErr == nil && len(runs) > 0 {
			sort.Slice(runs, func(i, j int) bool {
				return runs[i].StartedAt.Before(runs[j].StartedAt)
			})
			parentRunID = runs[len(runs)-1].ID
		}
	}

	runRecord := &types.ChainRunRecord{
		ID:             requestID,
		OrgID:          orgID,
		ChainID:        chainID,
		SessionID:      chainRecord.SessionID,
		ParentRunID:    parentRunID,
		Provider:       strings.TrimSpace(in.Provider),
		Model:          strings.TrimSpace(in.Model),
		Status:         managedRunStatus(in.RunResult, in.ErrorSummary),
		StopReason:     managedRunStopReason(in.RunResult),
		ToolCount:      managedToolCount(in.RunResult),
		DurationMS:     maxInt64(0, now.Sub(startedAt).Milliseconds()),
		Usage:          managedUsage(in.RunResult),
		EffectiveConfig: types.ChainDefaults{
			Model:  strings.TrimSpace(in.Model),
			System: cloneJSONValue(in.System),
		},
		Metadata:  managedRunMetadata(in),
		StartedAt: startedAt,
	}
	if !now.IsZero() {
		runRecord.CompletedAt = &now
	}
	if err := store.CreateRun(ctx, runRecord); err != nil {
		return err
	}

	if err := store.SaveEffectiveRequest(ctx, &types.EffectiveRequestResponse{
		RunID:           requestID,
		Provider:        strings.TrimSpace(in.Provider),
		Model:           strings.TrimSpace(in.Model),
		EffectiveConfig: runRecord.EffectiveConfig,
		Messages:        cloneMessages(in.Messages),
	}); err != nil {
		return err
	}
	if err := store.AppendRunItems(ctx, requestID, buildManagedRunTimeline(in, startedAt)); err != nil {
		return err
	}
	return nil
}

func (s *AppServices) ListManagedObservability(ctx context.Context, orgID string, filter GatewayRequestFilter) (*ManagedObservabilitySnapshot, error) {
	store, err := s.managedChainStore()
	if err != nil {
		return nil, err
	}
	sessions, err := store.ListSessions(ctx, orgID)
	if err != nil {
		return nil, err
	}
	chains, err := store.ListChains(ctx, orgID, "", false)
	if err != nil {
		return nil, err
	}
	sessionByID := make(map[string]types.SessionRecord, len(sessions))
	for i := range sessions {
		sessionByID[sessions[i].ID] = sessions[i]
	}

	result := &ManagedObservabilitySnapshot{
		Sessions:          make([]ManagedObservabilitySession, 0),
		UnsessionedChains: make([]ManagedObservabilityChain, 0),
	}
	sessionsByID := make(map[string]*ManagedObservabilitySession)

	for i := range chains {
		chain := chains[i]
		session := sessionByID[chain.SessionID]
		if !managedChainMatchesSessionFilter(chain, session, filter.SessionID) {
			continue
		}
		if strings.TrimSpace(filter.ChainID) != "" && strings.TrimSpace(filter.ChainID) != chain.ID {
			continue
		}
		runs, listErr := store.ListChainRuns(ctx, chain.ID)
		if listErr != nil {
			return nil, listErr
		}
		observedRuns := make([]ManagedObservabilityRun, 0, len(runs))
		modelSet := make(map[string]struct{})
		transportSet := make(map[string]struct{})
		latestActivity := chain.UpdatedAt
		for j := range runs {
			observedRun := managedObservabilityRunFromRecord(runs[j], session)
			if !managedRunMatchesFilter(observedRun, filter) {
				continue
			}
			if observedRun.CompletedAt != nil && observedRun.CompletedAt.After(latestActivity) {
				latestActivity = observedRun.CompletedAt.UTC()
			}
			if observedRun.Model != "" {
				modelSet[observedRun.Model] = struct{}{}
			}
			if observedRun.Transport != "" {
				transportSet[observedRun.Transport] = struct{}{}
			}
			observedRuns = append(observedRuns, observedRun)
		}
		if len(observedRuns) == 0 {
			continue
		}
		sort.Slice(observedRuns, func(i, j int) bool {
			return observedRuns[i].StartedAt.After(observedRuns[j].StartedAt)
		})
		chainEntry := ManagedObservabilityChain{
			Chain:             chain,
			SessionID:         chain.SessionID,
			ExternalSessionID: chain.ExternalSessionID,
			RunCount:          len(observedRuns),
			LatestActivity:    latestActivity,
			Models:            sortedStringSet(modelSet),
			Transports:        sortedStringSet(transportSet),
			Runs:              observedRuns,
		}
		if chain.SessionID != "" {
			group := sessionsByID[chain.SessionID]
			if group == nil {
				group = &ManagedObservabilitySession{
					Session:        session,
					LatestActivity: latestActivity,
					Chains:         make([]ManagedObservabilityChain, 0, 4),
				}
				sessionsByID[chain.SessionID] = group
			}
			group.Chains = append(group.Chains, chainEntry)
			group.ChainCount++
			group.RunCount += len(observedRuns)
			if latestActivity.After(group.LatestActivity) {
				group.LatestActivity = latestActivity
			}
			continue
		}
		result.UnsessionedChains = append(result.UnsessionedChains, chainEntry)
	}

	for _, group := range sessionsByID {
		sort.Slice(group.Chains, func(i, j int) bool {
			return group.Chains[i].LatestActivity.After(group.Chains[j].LatestActivity)
		})
		result.Sessions = append(result.Sessions, *group)
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		return result.Sessions[i].LatestActivity.After(result.Sessions[j].LatestActivity)
	})
	sort.Slice(result.UnsessionedChains, func(i, j int) bool {
		return result.UnsessionedChains[i].LatestActivity.After(result.UnsessionedChains[j].LatestActivity)
	})
	return result, nil
}

func (s *AppServices) ManagedRunDetail(ctx context.Context, orgID, runID string) (*ManagedObservabilityRunDetail, error) {
	store, err := s.managedChainStore()
	if err != nil {
		return nil, err
	}
	runRecord, err := store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if runRecord.OrgID != orgID {
		return nil, pgx.ErrNoRows
	}
	var session types.SessionRecord
	if runRecord.SessionID != "" {
		session, _ = s.managedSessionByID(ctx, store, runRecord.SessionID)
	}
	timeline, err := store.GetRunTimeline(ctx, runID)
	if err != nil {
		return nil, err
	}
	effectiveRequest, err := store.GetEffectiveRequest(ctx, runID)
	if err != nil {
		return nil, err
	}
	run := managedObservabilityRunFromRecord(*runRecord, session)
	return &ManagedObservabilityRunDetail{
		Run:              run,
		Record:           runRecord,
		EffectiveRequest: effectiveRequest,
		Timeline:         timeline,
	}, nil
}

func (s *AppServices) managedChainStore() (*chainrt.PostgresStore, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("managed history store is not configured")
	}
	return chainrt.NewPostgresStore(s.DB), nil
}

func (s *AppServices) ensureManagedObservationSession(ctx context.Context, store *chainrt.PostgresStore, orgID, externalSessionID string, now time.Time) (*types.SessionRecord, error) {
	externalSessionID = strings.TrimSpace(externalSessionID)
	if externalSessionID == "" {
		return nil, nil
	}
	existing, err := store.GetSessionByExternal(ctx, orgID, externalSessionID)
	if err == nil && existing != nil {
		existing.UpdatedAt = now
		return existing, store.UpsertSession(ctx, existing)
	}
	if err != nil && !errors.Is(err, chainrt.ErrNotFound) {
		return nil, err
	}
	record := &types.SessionRecord{
		ID:                     newID("sess"),
		OrgID:                  orgID,
		ExternalSessionID:      externalSessionID,
		CreatedByPrincipalID:   "gateway_api_key",
		CreatedByPrincipalType: "gateway_api_key",
		Metadata: map[string]any{
			"observability": map[string]any{
				"request_origin": "raw_gateway",
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	return record, store.UpsertSession(ctx, record)
}

func (s *AppServices) ensureManagedObservationChain(ctx context.Context, store *chainrt.PostgresStore, in GatewayManagedRunProjectionInput, session *types.SessionRecord, now time.Time) (*types.ChainRecord, []types.Message, error) {
	history := cloneMessages(in.Messages)
	if in.RunResult != nil {
		switch {
		case len(in.RunResult.Messages) > 0:
			history = cloneMessages(in.RunResult.Messages)
		case in.RunResult.Response != nil:
			history = append(history, types.Message{
				Role:    in.RunResult.Response.Role,
				Content: cloneJSONValue(in.RunResult.Response.Content),
			})
		}
	}
	chain, _, err := store.GetChain(ctx, in.ChainID)
	if err != nil && !errors.Is(err, chainrt.ErrNotFound) {
		return nil, nil, err
	}
	if chain == nil {
		chain = &types.ChainRecord{
			ID:                     strings.TrimSpace(in.ChainID),
			OrgID:                  strings.TrimSpace(in.OrgID),
			SessionID:              sessionID(session),
			ExternalSessionID:      strings.TrimSpace(in.ExternalSessionID),
			CreatedByPrincipalID:   "gateway_api_key",
			CreatedByPrincipalType: "gateway_api_key",
			Status:                 types.ChainStatusIdle,
			ChainVersion:           1,
			CreatedAt:              now,
		}
	} else {
		chain.ChainVersion++
	}
	chain.UpdatedAt = now
	chain.Status = types.ChainStatusIdle
	chain.MessageCountCached = len(history)
	chain.Defaults = types.ChainDefaults{
		Model:  strings.TrimSpace(in.Model),
		System: cloneJSONValue(in.System),
	}
	chain.Metadata = mergeManagedMetadata(chain.Metadata, map[string]any{
		"observability": map[string]any{
			"request_origin": "raw_gateway",
			"endpoint_kind":  strings.TrimSpace(in.EndpointKind),
			"key_source":     string(in.KeySource),
		},
	})
	if session != nil {
		chain.SessionID = session.ID
		chain.ExternalSessionID = session.ExternalSessionID
	}
	return chain, history, nil
}

func buildManagedRunTimeline(in GatewayManagedRunProjectionInput, now time.Time) []types.RunTimelineItem {
	items := make([]types.RunTimelineItem, 0, len(in.Messages)+1)
	seq := 0
	appendItem := func(item types.RunTimelineItem) {
		seq++
		item.SequenceInRun = seq
		item.SequenceInChain = seq
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		items = append(items, item)
	}
	if in.System != nil {
		appendItem(types.RunTimelineItem{
			ID:      newID("item"),
			Kind:    "system",
			Content: contentBlocksFromAny(in.System),
		})
	}
	for _, msg := range in.Messages {
		appendItem(types.RunTimelineItem{
			ID:      newID("item"),
			Kind:    msg.Role,
			Content: cloneJSONValue(msg.ContentBlocks()),
		})
	}
	if in.RunResult == nil {
		return items
	}
	for _, step := range in.RunResult.Steps {
		for _, call := range step.ToolCalls {
			appendItem(types.RunTimelineItem{
				ID:          newID("item"),
				Kind:        "tool_call",
				StepIndex:   step.Index,
				ExecutionID: call.ID,
				Tool: &types.RunTimelineTool{
					Name: call.Name,
					Args: cloneJSONValue(call.Input),
				},
			})
			for _, result := range step.ToolResults {
				if result.ToolUseID != call.ID {
					continue
				}
				appendItem(types.RunTimelineItem{
					ID:          newID("item"),
					Kind:        "tool_result",
					StepIndex:   step.Index,
					ExecutionID: result.ToolUseID,
					Content:     cloneJSONValue(result.Content),
				})
			}
		}
		if step.Response != nil {
			appendItem(types.RunTimelineItem{
				ID:        newID("item"),
				Kind:      "assistant",
				StepIndex: step.Index,
				Content:   cloneJSONValue(step.Response.Content),
			})
		}
	}
	return items
}

func managedRunStatus(result *types.RunResult, errorSummary string) types.RunStatus {
	if result == nil {
		if strings.TrimSpace(errorSummary) != "" {
			return types.RunStatusFailed
		}
		return types.RunStatusCompleted
	}
	switch result.StopReason {
	case types.RunStopReasonCancelled:
		return types.RunStatusCancelled
	case types.RunStopReasonTimeout:
		return types.RunStatusTimedOut
	case types.RunStopReasonError:
		return types.RunStatusFailed
	default:
		return types.RunStatusCompleted
	}
}

func managedRunStopReason(result *types.RunResult) string {
	if result == nil {
		return ""
	}
	return string(result.StopReason)
}

func managedToolCount(result *types.RunResult) int {
	if result == nil {
		return 0
	}
	return result.ToolCallCount
}

func managedUsage(result *types.RunResult) types.Usage {
	if result == nil {
		return types.Usage{}
	}
	return result.Usage
}

func managedRunMetadata(in GatewayManagedRunProjectionInput) map[string]any {
	return map[string]any{
		"observability": map[string]any{
			"request_origin":        "raw_gateway",
			"endpoint_kind":         strings.TrimSpace(in.EndpointKind),
			"endpoint_family":       strings.TrimSpace(in.EndpointFamily),
			"transport":             transportForEndpointKind(in.EndpointKind),
			"protocol":              transportForEndpointKind(in.EndpointKind),
			"key_source":            string(in.KeySource),
			"access_credential":     string(in.AccessCredential),
			"gateway_api_key_id":    strings.TrimSpace(in.GatewayAPIKeyID),
			"gateway_api_key_name":  strings.TrimSpace(in.GatewayAPIKeyName),
			"gateway_api_key_prefix": strings.TrimSpace(in.GatewayAPIKeyPrefix),
			"run_config":            normalizeJSONText(in.RunConfig),
			"error_summary":         strings.TrimSpace(in.ErrorSummary),
			"input_context_fingerprint": strings.TrimSpace(in.InputContextFingerprint),
			"output_context_fingerprint": strings.TrimSpace(in.OutputContextFingerprint),
		},
	}
}

func managedObservabilityRunFromRecord(run types.ChainRunRecord, session types.SessionRecord) ManagedObservabilityRun {
	obs := metadataMap(run.Metadata, "observability")
	transport := firstNonEmptyString(metadataString(obs, "transport"), transportForAttachmentModeString(metadataString(obs, "attachment_mode")))
	return ManagedObservabilityRun{
		ID:                run.ID,
		ChainID:           run.ChainID,
		SessionID:         run.SessionID,
		ExternalSessionID: firstNonEmptyString(session.ExternalSessionID, metadataString(obs, "external_session_id")),
		Provider:          run.Provider,
		Model:             run.Model,
		Status:            run.Status,
		StopReason:        run.StopReason,
		ToolCount:         run.ToolCount,
		DurationMS:        run.DurationMS,
		StartedAt:         run.StartedAt,
		CompletedAt:       run.CompletedAt,
		Transport:         transport,
		Protocol:          firstNonEmptyString(metadataString(obs, "protocol"), transport),
		EndpointKind:      metadataString(obs, "endpoint_kind"),
		KeySource:         KeySource(metadataString(obs, "key_source")),
		AccessCredential:  AccessCredential(metadataString(obs, "access_credential")),
		GatewayAPIKeyID:   metadataString(obs, "gateway_api_key_id"),
		GatewayAPIKeyName: metadataString(obs, "gateway_api_key_name"),
		GatewayAPIKeyPref: metadataString(obs, "gateway_api_key_prefix"),
		ErrorSummary:      metadataString(obs, "error_summary"),
	}
}

func managedRunMatchesFilter(run ManagedObservabilityRun, filter GatewayRequestFilter) bool {
	if strings.TrimSpace(filter.Model) != "" && !strings.EqualFold(strings.TrimSpace(filter.Model), strings.TrimSpace(run.Model)) {
		return false
	}
	if strings.TrimSpace(filter.APIKeyID) != "" && strings.TrimSpace(filter.APIKeyID) != strings.TrimSpace(run.GatewayAPIKeyID) {
		return false
	}
	if strings.TrimSpace(filter.ChainID) != "" && strings.TrimSpace(filter.ChainID) != strings.TrimSpace(run.ChainID) {
		return false
	}
	if strings.TrimSpace(filter.Status) != "" {
		switch strings.ToLower(strings.TrimSpace(filter.Status)) {
		case "success":
			if run.Status != types.RunStatusCompleted {
				return false
			}
		case "error":
			if run.Status == types.RunStatusCompleted {
				return false
			}
		}
	}
	if filter.Hours > 0 {
		since := time.Now().UTC().Add(-time.Duration(filter.Hours) * time.Hour)
		if run.CompletedAt != nil {
			if run.CompletedAt.Before(since) {
				return false
			}
		} else if run.StartedAt.Before(since) {
			return false
		}
	}
	return true
}

func managedChainMatchesSessionFilter(chain types.ChainRecord, session types.SessionRecord, filterValue string) bool {
	filterValue = strings.TrimSpace(filterValue)
	if filterValue == "" {
		return true
	}
	return filterValue == chain.SessionID ||
		filterValue == chain.ExternalSessionID ||
		filterValue == session.ID ||
		filterValue == session.ExternalSessionID
}

func (s *AppServices) managedSessionByID(ctx context.Context, store *chainrt.PostgresStore, sessionID string) (types.SessionRecord, error) {
	record, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return types.SessionRecord{}, err
	}
	return *record, nil
}

func mergeManagedMetadata(current, patch map[string]any) map[string]any {
	merged := cloneJSONValue(current)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range patch {
		if key == "observability" {
			base := metadataMap(merged, key)
			for nestedKey, nestedValue := range metadataMap(map[string]any{"observability": value}, "observability") {
				base[nestedKey] = nestedValue
			}
			merged[key] = base
			continue
		}
		merged[key] = value
	}
	return merged
}

func metadataMap(src map[string]any, key string) map[string]any {
	if src == nil {
		return nil
	}
	value, _ := src[key].(map[string]any)
	return cloneJSONValue(value)
}

func metadataString(src map[string]any, key string) string {
	if src == nil {
		return ""
	}
	value, _ := src[key].(string)
	return strings.TrimSpace(value)
}

func contentBlocksFromAny(value any) []types.ContentBlock {
	switch typed := value.(type) {
	case string:
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: typed}}
	case []types.ContentBlock:
		return cloneJSONValue(typed)
	default:
		return nil
	}
}

func cloneMessages(in []types.Message) []types.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.Message, len(in))
	copy(out, in)
	return out
}

func cloneJSONValue[T any](value T) T {
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return value
	}
	return out
}

func transportForEndpointKind(endpointKind string) string {
	switch strings.TrimSpace(endpointKind) {
	case "runs_stream":
		return "sse"
	default:
		return "http"
	}
}

func transportForAttachmentModeString(mode string) string {
	switch strings.TrimSpace(mode) {
	case string(types.AttachmentModeTurnWS), string(types.AttachmentModeLiveWS):
		return "websocket"
	case string(types.AttachmentModeStatefulSSE):
		return "sse"
	default:
		return "http"
	}
}

func sessionID(session *types.SessionRecord) string {
	if session == nil {
		return ""
	}
	return session.ID
}

func sortedStringSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
