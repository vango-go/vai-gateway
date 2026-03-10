package chains

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/runloop"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

func (m *Manager) StartRunAsync(ctx context.Context, chainID string, env RuntimeEnvironment, payload types.RunStartPayload, idempotencyKey string) (*types.ChainRunRecord, error) {
	run, _, err := m.startRun(ctx, chainID, env, payload, idempotencyKey, false)
	return run, err
}

func (m *Manager) RunBlocking(ctx context.Context, chainID string, env RuntimeEnvironment, payload types.RunStartPayload, idempotencyKey string) (*types.ChainRunRecord, *types.RunResult, error) {
	return m.startRun(ctx, chainID, env, payload, idempotencyKey, true)
}

func (m *Manager) startRun(ctx context.Context, chainID string, env RuntimeEnvironment, payload types.RunStartPayload, idempotencyKey string, wait bool) (*types.ChainRunRecord, *types.RunResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, nil, types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "idempotency_key is required")
	}
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return nil, nil, types.NewCanonicalError(types.ErrorCodeAuthResumeTokenInvalid, "chain is not available").WithChain(chainID)
	}
	if authErr := m.authorizeChainMutation(chain, env.Principal); authErr != nil {
		return nil, nil, authErr.WithChain(chainID)
	}
	requestHash := payloadHash(payload)
	if existing, err := m.store.GetIdempotency(ctx, IdempotencyScope{
		OrgID:          env.Principal.OrgID,
		PrincipalID:    env.Principal.PrincipalID,
		ChainID:        chainID,
		Operation:      "run.start",
		IdempotencyKey: idempotencyKey,
	}); err == nil && existing != nil {
		if existing.RequestHash != requestHash {
			return nil, nil, types.NewCanonicalError(types.ErrorCodeToolResultConflict, "idempotent request payload conflict").WithChain(chainID)
		}
		runID, _ := existing.ResultRef["run_id"].(string)
		if runID != "" {
			runRecord, err := m.store.GetRun(ctx, runID)
			if err == nil {
				return runRecord, nil, nil
			}
		}
	}

	chain.mu.Lock()
	var attachmentID string
	var attachmentStartedAt time.Time
	if chain.record.Status == types.ChainStatusClosed {
		chain.mu.Unlock()
		return nil, nil, types.NewCanonicalError(types.ErrorCodeChainExpired, "chain is closed").WithChain(chainID)
	}
	if env.Mode == types.AttachmentModeStatefulHTTP || env.Mode == types.AttachmentModeStatefulSSE {
		if chain.writer != nil {
			chain.mu.Unlock()
			return nil, nil, types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain already has an active attachment").WithChain(chainID)
		}
		attachmentStartedAt = time.Now().UTC()
		lease := &writerLease{
			ID:        newID("attach"),
			Mode:      env.Mode,
			Protocol:  env.Protocol,
			Principal: env.Principal,
			Scope:     env.Scope,
			StartedAt: attachmentStartedAt,
			Sink:      env.Send,
		}
		chain.writer = lease
		chain.record.ActiveAttachment = &types.ActiveAttachmentInfo{Mode: env.Mode, StartedAt: attachmentStartedAt}
		attachmentID = lease.ID
	} else if chain.writer == nil || chain.writer.Mode != env.Mode {
		chain.mu.Unlock()
		return nil, nil, types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain is not attached on this transport").WithChain(chainID)
	}
	if chain.activeRun != nil {
		chain.mu.Unlock()
		return nil, nil, types.NewCanonicalError(types.ErrorCodeChainAttachConflict, "chain already has an active run").WithChain(chainID)
	}
	effective := mergeDefaults(chain.record.Defaults, valueOrZero(payload.Overrides))
	if strings.TrimSpace(effective.Model) == "" {
		chain.mu.Unlock()
		return nil, nil, types.NewCanonicalError(types.ErrorCodeProtocolUnsupportedCapability, "model is required").WithChain(chainID)
	}
	if !env.Scope.AllowsModel(effective.Model) {
		chain.mu.Unlock()
		return nil, nil, types.NewCanonicalError(types.ErrorCodeAuthCredentialScopeDenied, "current credential scope does not allow the requested model").WithChain(chainID)
	}
	for _, name := range effective.GatewayTools {
		if !env.Scope.AllowsGatewayTool(name) {
			chain.mu.Unlock()
			return nil, nil, types.NewCanonicalError(types.ErrorCodeAuthCredentialScopeDenied, "current credential scope does not allow the requested gateway tool").WithChain(chainID)
		}
	}
	var gatewayRegistry *servertools.Registry
	if len(effective.GatewayTools) > 0 {
		if env.BuildGatewayTools == nil {
			chain.mu.Unlock()
			return nil, nil, types.NewCanonicalError(types.ErrorCodeAuthCredentialScopeDenied, "gateway tools are not configured for this attachment").WithChain(chainID)
		}
		var buildErr error
		gatewayRegistry, buildErr = env.BuildGatewayTools(effective.GatewayTools, effective.GatewayToolConfig)
		if buildErr != nil {
			chain.mu.Unlock()
			return nil, nil, buildErr
		}
	}
	history := append(cloneMessages(chain.history), cloneMessages(payload.Input)...)
	if capabilityErr := types.ValidateCapability(effective.Model, history, effective); capabilityErr != nil {
		chain.mu.Unlock()
		return nil, nil, capabilityErr.WithChain(chainID)
	}
	now := time.Now().UTC()
	previousHistory := cloneMessages(chain.history)
	previousRecord := cloneJSON(*chain.record)
	chain.history = history
	chain.record.Status = types.ChainStatusRunning
	chain.record.ChainVersion++
	chain.record.UpdatedAt = now
	chain.record.MessageCountCached = len(history)
	runCtx, cancel := context.WithCancel(ctx)
	runMetadata := withChainRuntimeMetadata(mergeRunMetadata(payload.Metadata, env), len(payload.Input))
	runRecord := &types.ChainRunRecord{
		ID:              newID("run"),
		OrgID:           chain.record.OrgID,
		ChainID:         chainID,
		SessionID:       chain.record.SessionID,
		IdempotencyKey:  idempotencyKey,
		Provider:        providerFromModel(effective.Model),
		Model:           effective.Model,
		Status:          types.RunStatusRunning,
		EffectiveConfig: cloneJSON(effective),
		Metadata:        runMetadata,
		StartedAt:       now,
	}
	if chain.record.ForkedFromRunID != "" {
		if existingRuns, listErr := m.store.ListChainRuns(ctx, chainID); listErr == nil && len(existingRuns) == 0 {
			runRecord.RerunOfRunID = chain.record.ForkedFromRunID
		}
	}
	active := &activeRun{
		record:       runRecord,
		chain:        chain,
		env:          env,
		manager:      m,
		gatewayTools: gatewayRegistry,
		updateCh:     make(chan struct{}, 1),
		pending:      make(map[string]*pendingExecution),
		done:         make(chan struct{}),
		cancel:       cancel,
		attachmentID: attachmentID,
	}
	if attachmentID != "" {
		active.ephemeralWriter = true
	}
	chain.activeRun = active
	chain.mu.Unlock()

	rollbackStartFailure := func() {
		active.chain.mu.Lock()
		if active.chain.activeRun == active {
			active.chain.activeRun = nil
		}
		if attachmentID != "" && active.chain.writer != nil && active.chain.writer.ID == attachmentID {
			active.chain.writer = nil
		}
		restoredRecord := cloneJSON(previousRecord)
		*active.chain.record = restoredRecord
		active.chain.history = cloneMessages(previousHistory)
		active.chain.mu.Unlock()
		if attachmentID != "" {
			_ = m.store.CloseAttachment(context.Background(), attachmentID, "start_failed", time.Now().UTC())
		}
	}

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
			StartedAt:      attachmentStartedAt,
		}); err != nil {
			rollbackStartFailure()
			return nil, nil, fmt.Errorf("save attachment: %w", err)
		}
	}
	if err := m.store.UpdateChain(ctx, chain.record); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("update chain: %w", err)
	}
	if err := m.store.AppendChainMessages(ctx, chainID, runRecord.ID, payload.Input); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("append chain messages: %w", err)
	}
	if err := m.store.CreateRun(ctx, runRecord); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("create run: %w", err)
	}
	if err := m.store.AppendRunItems(ctx, runRecord.ID, buildInitialRunItems(effective, payload.Input, now)); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("append run items: %w", err)
	}
	effectiveRequest := buildEffectiveRequest(effective, history)
	effectiveRequest.RunID = runRecord.ID
	if err := m.store.SaveEffectiveRequest(ctx, effectiveRequest); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("save effective request: %w", err)
	}
	if err := m.store.SaveIdempotency(ctx, &IdempotencyRecord{
		ID:             newID("idem"),
		OrgID:          env.Principal.OrgID,
		PrincipalID:    env.Principal.PrincipalID,
		ChainID:        chainID,
		Operation:      "run.start",
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash,
		ResultRef: map[string]any{
			"run_id": runRecord.ID,
		},
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		rollbackStartFailure()
		return nil, nil, fmt.Errorf("save idempotency: %w", err)
	}

	_ = m.emitRunEvent(chain, runRecord.ID, types.RunStartEvent{
		Type:            "run_start",
		RequestID:       runRecord.ID,
		Model:           effective.Model,
		ProtocolVersion: "1",
	})

	go m.executeRun(runCtx, active, history, effective)
	if wait {
		result, err := waitForRun(active)
		return cloneJSON(runRecord), result, err
	}
	return cloneJSON(runRecord), nil, nil
}

func mergeRunMetadata(metadata map[string]any, env RuntimeEnvironment) map[string]any {
	merged := cloneJSON(metadata)
	if merged == nil {
		merged = map[string]any{}
	}
	obs, _ := merged["observability"].(map[string]any)
	obs = cloneJSON(obs)
	if obs == nil {
		obs = map[string]any{}
	}
	obs["transport"] = transportForAttachmentMode(env.Mode)
	obs["protocol"] = strings.TrimSpace(env.Protocol)
	obs["attachment_mode"] = string(env.Mode)
	merged["observability"] = obs
	return merged
}

func withChainRuntimeMetadata(metadata map[string]any, inputMessageCount int) map[string]any {
	merged := cloneJSON(metadata)
	if merged == nil {
		merged = map[string]any{}
	}
	runtimeMeta, _ := merged["chain_runtime"].(map[string]any)
	runtimeMeta = cloneJSON(runtimeMeta)
	if runtimeMeta == nil {
		runtimeMeta = map[string]any{}
	}
	runtimeMeta["input_message_count"] = inputMessageCount
	merged["chain_runtime"] = runtimeMeta
	return merged
}

func transportForAttachmentMode(mode types.AttachmentMode) string {
	switch mode {
	case types.AttachmentModeTurnWS, types.AttachmentModeLiveWS:
		return "websocket"
	case types.AttachmentModeStatefulSSE:
		return "sse"
	default:
		return "http"
	}
}

func (m *Manager) SubmitClientToolResult(ctx context.Context, chainID string, frame types.ClientToolResultFrame) error {
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return types.NewCanonicalError(types.ErrorCodeAuthResumeTokenInvalid, "chain is not available").WithChain(chainID)
	}
	chain.mu.Lock()
	active := chain.activeRun
	chain.mu.Unlock()
	if active == nil || active.record == nil || active.record.ID != frame.RunID {
		return types.NewCanonicalError(types.ErrorCodeToolResultConflict, "run is not waiting for this tool result").WithChain(chainID).WithRun(frame.RunID)
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	execution, ok := active.pending[frame.ExecutionID]
	if !ok {
		return types.NewCanonicalError(types.ErrorCodeToolResultConflict, "tool execution is not pending").WithChain(chainID).WithRun(frame.RunID).WithExecution(frame.ExecutionID)
	}
	payloadHash := payloadHash(map[string]any{
		"content":  frame.Content,
		"is_error": frame.IsError,
	})
	if execution.Resolved {
		if execution.PayloadHash == payloadHash {
			return nil
		}
		return types.NewCanonicalError(types.ErrorCodeToolResultConflict, "tool result payload conflicts with the existing resolution").WithChain(chainID).WithRun(frame.RunID).WithExecution(frame.ExecutionID)
	}
	execution.Resolved = true
	execution.Content = normalizeToolResultContent(frame.Content)
	execution.IsError = frame.IsError
	execution.PayloadHash = payloadHash
	select {
	case active.updateCh <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) CancelRun(ctx context.Context, chainID, runID string) error {
	chain, err := m.requireChain(ctx, chainID)
	if err != nil {
		return err
	}
	chain.mu.Lock()
	active := chain.activeRun
	chain.mu.Unlock()
	if active == nil || active.record == nil || active.record.ID != runID {
		return nil
	}
	if active.cancel != nil {
		active.cancel()
	}
	return nil
}

func (m *Manager) executeRun(ctx context.Context, active *activeRun, history []types.Message, effective types.ChainDefaults) {
	defer close(active.done)
	defer func() {
		active.chain.mu.Lock()
		if active.chain.activeRun == active {
			active.chain.activeRun = nil
			if active.ephemeralWriter && active.chain.writer != nil && active.chain.writer.ID == active.attachmentID {
				active.chain.writer = nil
				active.chain.record.ActiveAttachment = nil
			}
			if active.chain.record.Status != types.ChainStatusClosed && active.chain.record.Status != types.ChainStatusExpired {
				active.chain.record.Status = types.ChainStatusIdle
			}
			active.chain.record.UpdatedAt = time.Now().UTC()
		}
		record := cloneJSON(active.chain.record)
		active.chain.mu.Unlock()
		_ = m.store.UpdateChain(context.Background(), record)
		if active.ephemeralWriter && active.attachmentID != "" {
			_ = m.store.CloseAttachment(context.Background(), active.attachmentID, "request_complete", time.Now().UTC())
		}
	}()

	result := &types.RunResult{Steps: make([]types.RunStep, 0, 4)}
	runItemSeq := 1000
	chainItemSeq := len(history) * 10
	stepIndex := 0

	for {
		if stopReason, done := stopReasonFromContext(ctx.Err()); done {
			active.finish(result, stopReason, ctx.Err())
			return
		}

		_ = m.emitRunEvent(active.chain, active.record.ID, types.RunStepStartEvent{Type: "step_start", Index: stepIndex})
		stepStart := time.Now()
		resp, partial, imageRefs, err := m.executeModelTurn(ctx, active, history, effective)
		if err != nil {
			if len(partial) > 0 {
				_ = m.store.AppendRunItems(context.Background(), active.record.ID, []types.RunTimelineItem{{
					ID:              newID("item"),
					Kind:            "assistant",
					StepIndex:       stepIndex,
					SequenceInRun:   runItemSeq,
					SequenceInChain: chainItemSeq,
					Content:         partial,
					CreatedAt:       time.Now().UTC(),
				}})
			}
			stopReason := types.RunStopReasonError
			if reason, ok := stopReasonFromContext(err); ok {
				stopReason = reason
			}
			active.finish(result, stopReason, err)
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunErrorEvent{
				Type: "error",
				Error: types.Error{
					Type:    string(core.ErrAPI),
					Message: err.Error(),
				},
			})
			return
		}

		result.Usage = result.Usage.Add(resp.Usage)
		result.TurnCount++
		step := types.RunStep{
			Index:      stepIndex,
			Response:   resp,
			DurationMS: time.Since(stepStart).Milliseconds(),
		}

		toolUses := resp.ToolUses()
		if resp.StopReason != types.StopReasonToolUse || len(toolUses) == 0 {
			history = append(history, types.Message{Role: "assistant", Content: resp.Content})
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = types.RunStopReasonEndTurn
			result.Messages = cloneMessages(history)
			runItemSeq++
			chainItemSeq++
			_ = m.store.AppendRunItems(context.Background(), active.record.ID, []types.RunTimelineItem{{
				ID:              newID("item"),
				Kind:            "assistant",
				StepIndex:       stepIndex,
				SequenceInRun:   runItemSeq,
				SequenceInChain: chainItemSeq,
				Content:         cloneContentBlocks(resp.Content),
				CreatedAt:       time.Now().UTC(),
			}})
			m.commitHistory(active.chain, active.record.ID, types.ChainStatusIdle, types.Message{Role: "assistant", Content: resp.Content})
			active.finish(result, types.RunStopReasonEndTurn, nil)
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunHistoryDeltaEvent{
				Type:        "history_delta",
				ExpectedLen: len(history) - 1,
				Append:      []types.Message{{Role: "assistant", Content: resp.Content}},
			})
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunCompleteEvent{Type: "run_complete", Result: result})
			return
		}

		step.ToolCalls = make([]types.RunToolCall, len(toolUses))
		for i := range toolUses {
			step.ToolCalls[i] = types.RunToolCall{
				ID:    toolUses[i].ID,
				Name:  toolUses[i].Name,
				Input: cloneJSON(toolUses[i].Input),
			}
		}
		batch, stopReason, stopErr := m.resolveToolBatch(ctx, active, stepIndex, toolUses, imageRefs)
		step.ToolResults = batch.Results
		result.ToolCallCount += len(batch.Results)
		result.Steps = append(result.Steps, step)

		assistantMsg := types.Message{Role: "assistant", Content: resp.Content}
		toolMsg := types.Message{Role: "user", Content: batch.ToolResultBlocks}
		history = append(history, assistantMsg, toolMsg)
		m.commitHistory(active.chain, active.record.ID, types.ChainStatusRunning, assistantMsg, toolMsg)
		_ = m.store.AppendRunItems(context.Background(), active.record.ID, batch.Items)
		_ = m.emitRunEvent(active.chain, active.record.ID, types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
		_ = m.emitRunEvent(active.chain, active.record.ID, types.RunHistoryDeltaEvent{
			Type:        "history_delta",
			ExpectedLen: len(history) - 2,
			Append:      []types.Message{assistantMsg, toolMsg},
		})

		if stopErr != nil {
			result.Response = resp
			result.StopReason = stopReason
			result.Messages = cloneMessages(history)
			active.finish(result, stopReason, stopErr)
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunCompleteEvent{Type: "run_complete", Result: result})
			return
		}
		stepIndex++
	}
}

type resolvedBatch struct {
	Results          []types.RunToolResult
	ToolResultBlocks []types.ContentBlock
	Items            []types.RunTimelineItem
}

func (m *Manager) resolveToolBatch(ctx context.Context, active *activeRun, stepIndex int, uses []types.ToolUseBlock, imageRefs *servertools.ImageRefRegistry) (resolvedBatch, types.RunStopReason, error) {
	now := time.Now().UTC()
	results := make([]types.RunToolResult, 0, len(uses))
	toolBlocks := make([]types.ContentBlock, 0, len(uses))
	items := make([]types.RunTimelineItem, 0, len(uses)*2)
	clientUses := make([]types.ToolUseBlock, 0)

	for i := range uses {
		tu := uses[i]
		items = append(items, types.RunTimelineItem{
			ID:        newID("item"),
			Kind:      "tool_call",
			StepIndex: stepIndex,
			Tool: &types.RunTimelineTool{
				Name: tu.Name,
				Args: cloneJSON(tu.Input),
			},
			ExecutionID: tu.ID,
			CreatedAt:   now,
		})

		if active.gatewayTools != nil && active.gatewayTools.Has(tu.Name) {
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunToolCallStartEvent{
				Type:  "tool_call_start",
				ID:    tu.ID,
				Name:  tu.Name,
				Input: cloneJSON(tu.Input),
			})
			toolCtx := servertools.ContextWithImageRefRegistry(ctx, imageRefs)
			content, toolErr := active.gatewayTools.Execute(toolCtx, tu.Name, tu.Input)
			result := types.RunToolResult{ToolUseID: tu.ID, Content: cloneContentBlocks(content)}
			if toolErr != nil {
				result.IsError = true
				result.Error = toolErr
				if len(result.Content) == 0 {
					result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: toolErr.Message}}
				}
			} else {
				result.Content = normalizeToolResultContent(result.Content)
			}
			if tu.Name == servertools.ToolImage {
				result.Content = servertools.FinalizeImageToolResultContent(result.Content, imageRefs)
			}
			results = append(results, result)
			toolBlocks = append(toolBlocks, types.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   cloneContentBlocks(result.Content),
				IsError:   result.IsError,
			})
			items = append(items, types.RunTimelineItem{
				ID:          newID("item"),
				Kind:        "tool_result",
				StepIndex:   stepIndex,
				ExecutionID: tu.ID,
				Content:     cloneContentBlocks(result.Content),
				CreatedAt:   time.Now().UTC(),
			})
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunToolResultEvent{
				Type:    "tool_result",
				ID:      tu.ID,
				Name:    tu.Name,
				Content: cloneContentBlocks(result.Content),
				IsError: result.IsError,
				Error:   result.Error,
			})
			continue
		}
		clientUses = append(clientUses, tu)
	}

	if len(clientUses) == 0 {
		return resolvedBatch{Results: results, ToolResultBlocks: toolBlocks, Items: items}, "", nil
	}
	if !active.env.AllowClientTools {
		for _, tu := range clientUses {
			content := []types.ContentBlock{types.TextBlock{Type: "text", Text: "client-executed tools require the chain websocket transport"}}
			results = append(results, types.RunToolResult{
				ToolUseID: tu.ID,
				Content:   normalizeToolResultContent(content),
				IsError:   true,
				Error: &types.Error{
					Type:    string(core.ErrInvalidRequest),
					Message: "client-executed tools require the chain websocket transport",
					Code:    string(types.ErrorCodeTransportClientToolsRequireWS),
				},
			})
			toolBlocks = append(toolBlocks, types.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   normalizeToolResultContent(content),
				IsError:   true,
			})
			items = append(items, types.RunTimelineItem{
				ID:          newID("item"),
				Kind:        "tool_result",
				StepIndex:   stepIndex,
				ExecutionID: tu.ID,
				Content:     normalizeToolResultContent(content),
				CreatedAt:   time.Now().UTC(),
			})
		}
		return resolvedBatch{Results: results, ToolResultBlocks: toolBlocks, Items: items}, types.RunStopReasonError, types.NewCanonicalError(types.ErrorCodeTransportClientToolsRequireWS, "client-executed tools require the chain websocket transport")
	}

	if err := m.enqueueClientToolCalls(active, clientUses); err != nil {
		return resolvedBatch{Results: results, ToolResultBlocks: toolBlocks, Items: items}, types.RunStopReasonError, err
	}
	waited, waitErr := m.waitForClientTools(ctx, active, clientUses)
	for _, resolved := range waited {
		results = append(results, types.RunToolResult{
			ToolUseID: resolved.ExecutionID,
			Content:   normalizeToolResultContent(resolved.Content),
			IsError:   resolved.IsError,
		})
		toolBlocks = append(toolBlocks, types.ToolResultBlock{
			Type:      "tool_result",
			ToolUseID: resolved.ExecutionID,
			Content:   normalizeToolResultContent(resolved.Content),
			IsError:   resolved.IsError,
		})
		items = append(items, types.RunTimelineItem{
			ID:          newID("item"),
			Kind:        "tool_result",
			StepIndex:   stepIndex,
			ExecutionID: resolved.ExecutionID,
			Content:     normalizeToolResultContent(resolved.Content),
			CreatedAt:   time.Now().UTC(),
		})
	}
	if waitErr != nil {
		stopReason := types.RunStopReasonError
		if errors.Is(waitErr, context.DeadlineExceeded) {
			stopReason = types.RunStopReasonTimeout
		} else if errors.Is(waitErr, context.Canceled) {
			stopReason = types.RunStopReasonCancelled
		}
		return resolvedBatch{Results: results, ToolResultBlocks: toolBlocks, Items: items}, stopReason, waitErr
	}
	return resolvedBatch{Results: results, ToolResultBlocks: toolBlocks, Items: items}, "", nil
}

func (m *Manager) enqueueClientToolCalls(active *activeRun, uses []types.ToolUseBlock) error {
	active.mu.Lock()
	for i := range uses {
		active.pending[uses[i].ID] = &pendingExecution{
			ExecutionID: uses[i].ID,
			Name:        uses[i].Name,
			Input:       cloneJSON(uses[i].Input),
			DeadlineAt:  time.Now().UTC().Add(m.cfg.ToolWaitTimeout),
			EffectClass: types.ToolEffectClassUnknown,
			CallIndex:   i,
		}
	}
	active.mu.Unlock()
	for i := range uses {
		if err := m.emitClientToolCall(active.chain, active.record.ID, uses[i], time.Now().UTC().Add(m.cfg.ToolWaitTimeout)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) waitForClientTools(ctx context.Context, active *activeRun, uses []types.ToolUseBlock) ([]pendingExecution, error) {
	timer := time.NewTimer(m.cfg.ToolWaitTimeout)
	defer timer.Stop()
	for {
		active.mu.Lock()
		done := true
		out := make([]pendingExecution, 0, len(uses))
		for _, use := range uses {
			execution := active.pending[use.ID]
			if execution == nil || !execution.Resolved {
				done = false
				continue
			}
			out = append(out, *execution)
		}
		active.mu.Unlock()
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return m.timeoutPending(active, uses, ctx.Err()), ctx.Err()
		case <-timer.C:
			return m.timeoutPending(active, uses, context.DeadlineExceeded), context.DeadlineExceeded
		case <-active.updateCh:
		}
	}
}

func (m *Manager) timeoutPending(active *activeRun, uses []types.ToolUseBlock, cause error) []pendingExecution {
	active.mu.Lock()
	defer active.mu.Unlock()
	out := make([]pendingExecution, 0, len(uses))
	for _, use := range uses {
		execution := active.pending[use.ID]
		if execution == nil {
			continue
		}
		if !execution.Resolved {
			message := "client tool execution timed out"
			if errors.Is(cause, context.Canceled) {
				message = "client tool execution was cancelled"
			}
			execution.Resolved = true
			execution.IsError = true
			execution.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: message}}
			execution.PayloadHash = payloadHash(message)
		}
		out = append(out, *execution)
	}
	return out
}

func (m *Manager) executeModelTurn(ctx context.Context, active *activeRun, history []types.Message, effective types.ChainDefaults) (*types.MessageResponse, []types.ContentBlock, *servertools.ImageRefRegistry, error) {
	providerName, modelName, ok := splitModel(effective.Model)
	if !ok {
		return nil, nil, nil, fmt.Errorf("invalid model %q", effective.Model)
	}
	if active.env.NewProvider == nil {
		return nil, nil, nil, fmt.Errorf("provider factory is not configured")
	}
	key := active.env.Scope.providerKeyFor(providerName)
	if key == "" {
		return nil, nil, nil, types.NewCanonicalError(types.ErrorCodeAuthCredentialScopeDenied, "current credential scope does not allow the requested model")
	}
	provider, err := active.env.NewProvider(providerName, key)
	if err != nil {
		return nil, nil, nil, err
	}
	capability, _ := types.LookupCapability(effective.Model)
	plannerMessages, imageRefs := servertools.BuildPlannerMessages(history, capability.SupportsVision)
	req := messageRequestFromDefaults(effective, plannerMessages)
	if active.env.ResolveMessageRequest != nil {
		resolvedReq, err := active.env.ResolveMessageRequest(ctx, req)
		if err != nil {
			return nil, nil, imageRefs, err
		}
		req = resolvedReq
	} else if types.RequestHasAssetReferences(req) {
		return nil, nil, imageRefs, core.NewInvalidRequestError("asset references require gateway asset storage to be configured")
	}
	req.Model = modelName

	stream, err := provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, nil, imageRefs, err
	}
	defer func() { _ = stream.Close() }()

	acc := runloop.NewStreamAccumulator()
	for {
		event, err := stream.Next()
		if event != nil {
			acc.Apply(event)
			_ = m.emitRunEvent(active.chain, active.record.ID, types.RunStreamEventWrapper{
				Type:  "stream_event",
				Event: event,
			})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			resp := acc.Response()
			if resp != nil {
				return nil, cloneContentBlocks(resp.Content), imageRefs, err
			}
			return nil, nil, imageRefs, err
		}
	}
	resp := acc.Response()
	if resp == nil {
		return nil, nil, imageRefs, fmt.Errorf("provider stream completed without a response")
	}
	return resp, cloneContentBlocks(resp.Content), imageRefs, nil
}

func (m *Manager) commitHistory(chain *hotChain, runID string, status types.ChainStatus, messages ...types.Message) {
	chain.mu.Lock()
	chain.history = append(chain.history, cloneMessages(messages)...)
	chain.record.Status = status
	chain.record.ChainVersion++
	chain.record.UpdatedAt = time.Now().UTC()
	chain.record.MessageCountCached = len(chain.history)
	record := cloneJSON(chain.record)
	chain.mu.Unlock()
	_ = m.store.AppendChainMessages(context.Background(), chain.record.ID, runID, messages)
	_ = m.store.UpdateChain(context.Background(), record)
}

func (m *Manager) emitRunEvent(chain *hotChain, runID string, event types.RunStreamEvent) error {
	chain.mu.Lock()
	chain.nextEventID++
	envelope := types.RunEnvelopeEvent{
		Type:         "run.event",
		EventID:      chain.nextEventID,
		ChainVersion: chain.record.ChainVersion,
		RunID:        runID,
		ChainID:      chain.record.ID,
		Event:        event,
	}
	chain.mu.Unlock()
	return chain.bufferAndEmit(m.cfg, envelope)
}

func (m *Manager) emitClientToolCall(chain *hotChain, runID string, use types.ToolUseBlock, deadline time.Time) error {
	chain.mu.Lock()
	chain.nextEventID++
	event := types.ClientToolCallEvent{
		Type:         "client_tool.call",
		EventID:      chain.nextEventID,
		ChainVersion: chain.record.ChainVersion,
		RunID:        runID,
		ChainID:      chain.record.ID,
		ExecutionID:  use.ID,
		Name:         use.Name,
		DeadlineAt:   deadline,
		Input:        cloneJSON(use.Input),
		EffectClass:  types.ToolEffectClassUnknown,
	}
	chain.mu.Unlock()
	return chain.bufferAndEmit(m.cfg, event)
}

func (a *activeRun) finish(result *types.RunResult, stopReason types.RunStopReason, err error) {
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result = result
	a.err = err
	if a.record == nil {
		return
	}
	a.record.Status = toRunStatus(stopReason, err)
	a.record.StopReason = string(stopReason)
	a.record.Usage = result.Usage
	a.record.ToolCount = result.ToolCallCount
	a.record.DurationMS = time.Since(a.record.StartedAt).Milliseconds()
	a.record.CompletedAt = &now
	if result != nil {
		result.StopReason = stopReason
	}
	_ = a.manager.store.UpdateRun(context.Background(), a.record)
}

func buildInitialRunItems(effective types.ChainDefaults, input []types.Message, now time.Time) []types.RunTimelineItem {
	items := make([]types.RunTimelineItem, 0, len(input)+1)
	seq := 0
	if effective.System != nil {
		seq++
		items = append(items, types.RunTimelineItem{
			ID:            newID("item"),
			Kind:          "system",
			SequenceInRun: seq,
			Content:       contentFromAny(effective.System),
			CreatedAt:     now,
		})
	}
	for _, msg := range input {
		seq++
		items = append(items, types.RunTimelineItem{
			ID:            newID("item"),
			Kind:          msg.Role,
			SequenceInRun: seq,
			Content:       cloneContentBlocks(msg.ContentBlocks()),
			CreatedAt:     now,
		})
	}
	return items
}

func buildEffectiveRequest(effective types.ChainDefaults, history []types.Message) *types.EffectiveRequestResponse {
	capability, _ := types.LookupCapability(effective.Model)
	plannerMessages, _ := servertools.BuildPlannerMessages(history, capability.SupportsVision)
	return &types.EffectiveRequestResponse{
		RunID:           "",
		Provider:        providerFromModel(effective.Model),
		Model:           effective.Model,
		EffectiveConfig: cloneChainDefaults(effective),
		Messages:        plannerMessages,
	}
}

func messageRequestFromDefaults(defaults types.ChainDefaults, messages []types.Message) *types.MessageRequest {
	return &types.MessageRequest{
		Model:         defaults.Model,
		Messages:      cloneMessages(messages),
		MaxTokens:     defaults.MaxTokens,
		System:        normalizedMessageContent(defaults.System),
		Temperature:   cloneJSON(defaults.Temperature),
		TopP:          cloneJSON(defaults.TopP),
		TopK:          cloneJSON(defaults.TopK),
		StopSequences: append([]string(nil), defaults.StopSequences...),
		Tools:         mergedTools(defaults.Tools, defaults.GatewayTools, defaults.GatewayToolConfig),
		ToolChoice:    cloneJSON(defaults.ToolChoice),
		STTModel:      defaults.STTModel,
		TTSModel:      defaults.TTSModel,
		OutputFormat:  cloneJSON(defaults.OutputFormat),
		Output:        cloneJSON(defaults.Output),
		Voice:         cloneJSON(defaults.Voice),
		Extensions:    cloneJSON(defaults.Extensions),
		Metadata:      cloneJSON(defaults.Metadata),
	}
}

func mergedTools(tools []types.Tool, gatewayTools []string, gatewayToolConfig map[string]any) []types.Tool {
	out := cloneJSON(tools)
	if len(gatewayTools) == 0 {
		return out
	}
	for _, name := range gatewayTools {
		switch name {
		case servertools.ToolWebSearch:
			cfg, _ := servertools.DecodeWebSearchConfig(gatewayToolConfig[name])
			out = append(out, servertools.NewWebSearchExecutor(cfg, nil, nil).Definition())
		case servertools.ToolWebFetch:
			cfg, _ := servertools.DecodeWebFetchConfig(gatewayToolConfig[name])
			out = append(out, servertools.NewWebFetchExecutor(cfg, nil, nil).Definition())
		case servertools.ToolImage:
			cfg, _ := servertools.DecodeImageConfig(gatewayToolConfig[name])
			out = append(out, servertools.NewImageExecutor(cfg, "", nil).Definition())
		}
	}
	return out
}

func contentFromAny(value any) []types.ContentBlock {
	switch typed := value.(type) {
	case string:
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: typed}}
	case []types.ContentBlock:
		return cloneContentBlocks(typed)
	case types.ContentBlock:
		return cloneContentBlocks([]types.ContentBlock{typed})
	default:
		return cloneContentBlocksFromAny(value)
	}
}

func valueOrZero(overrides *types.RunOverrides) types.ChainDefaults {
	if overrides == nil {
		return types.ChainDefaults{}
	}
	return cloneChainDefaults(*overrides)
}

func toRunStatus(stopReason types.RunStopReason, err error) types.RunStatus {
	switch stopReason {
	case types.RunStopReasonEndTurn:
		return types.RunStatusCompleted
	case types.RunStopReasonCancelled:
		return types.RunStatusCancelled
	case types.RunStopReasonTimeout:
		return types.RunStatusTimedOut
	default:
		if err != nil {
			return types.RunStatusFailed
		}
		return types.RunStatusCompleted
	}
}

func providerFromModel(model string) string {
	provider, _, ok := splitModel(model)
	if !ok {
		return ""
	}
	return provider
}

func stopReasonFromContext(err error) (types.RunStopReason, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return types.RunStopReasonCancelled, true
	case errors.Is(err, context.DeadlineExceeded):
		return types.RunStopReasonTimeout, true
	default:
		return "", false
	}
}
