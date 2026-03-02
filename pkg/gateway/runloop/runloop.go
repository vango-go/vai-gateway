package runloop

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/gateway/apierror"
)

type EmitFunc func(event types.RunStreamEvent) error

type Controller struct {
	Provider          core.Provider
	Tools             ToolExecutor
	VoicePipeline     *voice.Pipeline
	StreamIdleTimeout time.Duration
	RequestID         string
	PublicModel       string
}

type ToolExecutor interface {
	Execute(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error)
}

type toolExecResult struct {
	call   types.RunToolCall
	result types.RunToolResult
}

func (c *Controller) RunBlocking(ctx context.Context, req *types.RunRequest) (*types.RunResult, error) {
	return c.run(ctx, req, nil)
}

func (c *Controller) RunStream(ctx context.Context, req *types.RunRequest, emit EmitFunc) (*types.RunResult, error) {
	if emit == nil {
		return nil, fmt.Errorf("emit function is required")
	}
	if err := emit(types.RunStartEvent{Type: "run_start", RequestID: c.RequestID, Model: c.PublicModel, ProtocolVersion: "1"}); err != nil {
		return nil, err
	}
	return c.run(ctx, req, emit)
}

func (c *Controller) run(ctx context.Context, req *types.RunRequest, emit EmitFunc) (*types.RunResult, error) {
	if c == nil || c.Provider == nil {
		return nil, fmt.Errorf("provider is required")
	}
	if c.Tools == nil {
		return nil, fmt.Errorf("tool executor is required")
	}
	if req == nil {
		return nil, fmt.Errorf("run request is required")
	}

	workingReq := req.Request
	userTranscript := ""
	if req.Request.Voice != nil && req.Request.Voice.Input != nil {
		if c.VoicePipeline == nil {
			return nil, &core.Error{Type: core.ErrInvalidRequest, Message: "voice input is not configured", Code: "unsupported_voice"}
		}
		processed, transcript, err := voice.PreprocessMessageRequestInputAudio(ctx, c.VoicePipeline, &workingReq)
		if err != nil {
			return nil, err
		}
		workingReq = *processed
		userTranscript = transcript
	}
	if req.Request.Voice != nil && req.Request.Voice.Output != nil && c.VoicePipeline == nil {
		return nil, &core.Error{Type: core.ErrInvalidRequest, Message: "voice output is not configured", Code: "unsupported_voice"}
	}

	runCfg := req.Run
	if runCfg.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(runCfg.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	history := make([]types.Message, len(workingReq.Messages))
	copy(history, workingReq.Messages)

	result := &types.RunResult{Steps: make([]types.RunStep, 0, 4)}

	snapshotHistory := func() []types.Message {
		out := make([]types.Message, len(history))
		copy(out, history)
		return out
	}

	stepIndex := 0
	for {
		if stopReason, done := stopReasonFromContext(ctx.Err()); done {
			result.StopReason = stopReason
			result.Messages = snapshotHistory()
			return result, ctx.Err()
		}

		if runCfg.MaxTurns > 0 && result.TurnCount >= runCfg.MaxTurns {
			result.StopReason = types.RunStopReasonMaxTurns
			result.Messages = snapshotHistory()
			if emit != nil {
				_ = emit(types.RunCompleteEvent{Type: "run_complete", Result: result})
			}
			return result, nil
		}
		if runCfg.MaxTokens > 0 && result.Usage.TotalTokens >= runCfg.MaxTokens {
			result.StopReason = types.RunStopReasonMaxTokens
			result.Messages = snapshotHistory()
			if emit != nil {
				_ = emit(types.RunCompleteEvent{Type: "run_complete", Result: result})
			}
			return result, nil
		}

		if emit != nil {
			if err := emit(types.RunStepStartEvent{Type: "step_start", Index: stepIndex}); err != nil {
				return nil, err
			}
		}

		stepStart := time.Now()

		turnReq := &types.MessageRequest{
			Model:         workingReq.Model,
			Messages:      history,
			MaxTokens:     workingReq.MaxTokens,
			System:        workingReq.System,
			Temperature:   workingReq.Temperature,
			TopP:          workingReq.TopP,
			TopK:          workingReq.TopK,
			StopSequences: workingReq.StopSequences,
			Tools:         workingReq.Tools,
			ToolChoice:    workingReq.ToolChoice,
			OutputFormat:  workingReq.OutputFormat,
			Output:        workingReq.Output,
			Voice:         workingReq.Voice,
			Extensions:    workingReq.Extensions,
			Metadata:      workingReq.Metadata,
		}

		var resp *types.MessageResponse
		var err error
		if emit == nil {
			resp, err = c.Provider.CreateMessage(ctx, turnReq)
		} else {
			resp, err = c.streamTurn(ctx, turnReq, emit)
		}
		if err != nil {
			result.StopReason = types.RunStopReasonError
			_, isContextStop := stopReasonFromContext(err)
			if stopReason, done := stopReasonFromContext(err); done {
				result.StopReason = stopReason
			}
			result.Messages = snapshotHistory()
			if emit != nil && !isContextStop {
				_ = emit(types.RunErrorEvent{Type: "error", Error: toTypesError(err, c.RequestID)})
			}
			return result, err
		}

		if userTranscript != "" {
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			resp.Metadata["user_transcript"] = userTranscript
		}

		result.Usage = result.Usage.Add(resp.Usage)
		result.TurnCount++
		maxTokensExceeded := runCfg.MaxTokens > 0 && result.Usage.TotalTokens >= runCfg.MaxTokens

		step := types.RunStep{Index: stepIndex, Response: resp, DurationMS: time.Since(stepStart).Milliseconds()}

		toolUses := resp.ToolUses()
		if resp.StopReason != types.StopReasonToolUse || len(toolUses) == 0 {
			if req.Request.Voice != nil && req.Request.Voice.Output != nil {
				if err := voice.AppendVoiceOutputToMessageResponse(ctx, c.VoicePipeline, req.Request.Voice, resp); err != nil {
					result.StopReason = types.RunStopReasonError
					result.Messages = snapshotHistory()
					if emit != nil {
						_ = emit(types.RunErrorEvent{Type: "error", Error: toTypesError(err, c.RequestID)})
					}
					return result, err
				}
			}
			history = append(history, types.Message{Role: "assistant", Content: resp.Content})
			result.Response = resp
			result.Steps = append(result.Steps, step)
			if maxTokensExceeded {
				result.StopReason = types.RunStopReasonMaxTokens
			} else {
				result.StopReason = types.RunStopReasonEndTurn
			}
			result.Messages = snapshotHistory()
			if emit != nil {
				_ = emit(types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
				expectedLen := len(history) - 1
				_ = emit(types.RunHistoryDeltaEvent{Type: "history_delta", ExpectedLen: expectedLen, Append: []types.Message{{Role: "assistant", Content: resp.Content}}})
				_ = emit(types.RunCompleteEvent{Type: "run_complete", Result: result})
			}
			return result, nil
		}
		if maxTokensExceeded {
			history = append(history, types.Message{Role: "assistant", Content: resp.Content})
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = types.RunStopReasonMaxTokens
			result.Messages = snapshotHistory()
			if emit != nil {
				_ = emit(types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
				expectedLen := len(history) - 1
				_ = emit(types.RunHistoryDeltaEvent{Type: "history_delta", ExpectedLen: expectedLen, Append: []types.Message{{Role: "assistant", Content: resp.Content}}})
				_ = emit(types.RunCompleteEvent{Type: "run_complete", Result: result})
			}
			return result, nil
		}

		if runCfg.MaxToolCalls > 0 && result.ToolCallCount+len(toolUses) > runCfg.MaxToolCalls {
			history = append(history, types.Message{Role: "assistant", Content: resp.Content})
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = types.RunStopReasonMaxToolCalls
			result.Messages = snapshotHistory()
			if emit != nil {
				_ = emit(types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
				expectedLen := len(history) - 1
				_ = emit(types.RunHistoryDeltaEvent{Type: "history_delta", ExpectedLen: expectedLen, Append: []types.Message{{Role: "assistant", Content: resp.Content}}})
				_ = emit(types.RunCompleteEvent{Type: "run_complete", Result: result})
			}
			return result, nil
		}

		step.ToolCalls = make([]types.RunToolCall, len(toolUses))
		for i, tu := range toolUses {
			step.ToolCalls[i] = types.RunToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input}
		}

		execRes := c.executeTools(ctx, toolUses, runCfg, emit)
		step.ToolResults = make([]types.RunToolResult, len(execRes))
		toolResultBlocks := make([]types.ContentBlock, len(execRes))
		for i, ex := range execRes {
			step.ToolResults[i] = ex.result
			toolResultBlocks[i] = types.ToolResultBlock{Type: "tool_result", ToolUseID: ex.result.ToolUseID, Content: ex.result.Content, IsError: ex.result.IsError}
		}

		result.ToolCallCount += len(execRes)
		result.Steps = append(result.Steps, step)

		assistantMsg := types.Message{Role: "assistant", Content: resp.Content}
		toolMsg := types.Message{Role: "user", Content: toolResultBlocks}
		expectedLen := len(history)
		history = append(history, assistantMsg, toolMsg)

		if emit != nil {
			_ = emit(types.RunStepCompleteEvent{Type: "step_complete", Index: stepIndex, Response: resp})
			_ = emit(types.RunHistoryDeltaEvent{Type: "history_delta", ExpectedLen: expectedLen, Append: []types.Message{assistantMsg, toolMsg}})
		}

		stepIndex++
	}
}

func (c *Controller) executeTools(ctx context.Context, uses []types.ToolUseBlock, cfg types.RunConfig, emit EmitFunc) []toolExecResult {
	results := make([]toolExecResult, len(uses))
	runOne := func(i int, tu types.ToolUseBlock) {
		if emit != nil {
			_ = emit(types.RunToolCallStartEvent{Type: "tool_call_start", ID: tu.ID, Name: tu.Name, Input: tu.Input})
		}

		toolCtx := ctx
		if cfg.ToolTimeoutMS > 0 {
			var cancel context.CancelFunc
			toolCtx, cancel = context.WithTimeout(ctx, time.Duration(cfg.ToolTimeoutMS)*time.Millisecond)
			defer cancel()
		}

		content, toolErr := c.Tools.Execute(toolCtx, tu.Name, tu.Input)
		res := types.RunToolResult{ToolUseID: tu.ID}
		if toolErr != nil {
			res.IsError = true
			res.Error = toolErr
			if len(content) == 0 {
				msg := toolErr.Message
				if msg == "" {
					msg = "tool execution failed"
				}
				content = []types.ContentBlock{types.TextBlock{Type: "text", Text: msg}}
			}
		} else if len(content) == 0 {
			content = []types.ContentBlock{types.TextBlock{Type: "text", Text: ""}}
		}
		res.Content = content
		results[i] = toolExecResult{call: types.RunToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input}, result: res}

		if emit != nil {
			_ = emit(types.RunToolResultEvent{Type: "tool_result", ID: tu.ID, Name: tu.Name, Content: res.Content, IsError: res.IsError, Error: res.Error})
		}
	}

	if cfg.ParallelTools && len(uses) > 1 {
		var wg sync.WaitGroup
		for i, tu := range uses {
			wg.Add(1)
			go func(idx int, use types.ToolUseBlock) {
				defer wg.Done()
				runOne(idx, use)
			}(i, tu)
		}
		wg.Wait()
		return results
	}

	for i, tu := range uses {
		runOne(i, tu)
	}
	return results
}

func (c *Controller) streamTurn(ctx context.Context, req *types.MessageRequest, emit EmitFunc) (*types.MessageResponse, error) {
	reqCopy := *req
	reqCopy.Stream = true

	stream, err := c.Provider.StreamMessage(ctx, &reqCopy)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	acc := NewStreamAccumulator()

	sampleRate := 24000
	if req.Voice != nil && req.Voice.Output != nil && req.Voice.Output.SampleRate > 0 {
		sampleRate = req.Voice.Output.SampleRate
	}

	var ttsStream *voice.StreamingTTS
	var audioCh <-chan []byte
	if req.Voice != nil && req.Voice.Output != nil {
		ttsCtx, err := c.VoicePipeline.NewStreamingTTSContext(ctx, req.Voice)
		if err != nil {
			return nil, err
		}
		ttsStream = voice.NewStreamingTTS(ttsCtx, voice.StreamingTTSOptions{BufferAudio: true})
		audioCh = ttsStream.Audio()
	}

	emitAudio := func(chunk []byte, isFinal bool) error {
		return emit(types.AudioChunkEvent{Type: "audio_chunk", Format: "pcm_s16le", Audio: base64.StdEncoding.EncodeToString(chunk), SampleRateHz: sampleRate, IsFinal: isFinal})
	}

	type next struct {
		ev  types.StreamEvent
		err error
	}
	nextCh := make(chan next, 1)
	go func() {
		defer close(nextCh)
		for {
			ev, err := stream.Next()
			select {
			case nextCh <- next{ev: ev, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	idle := c.StreamIdleTimeout
	var idleTimer *time.Timer
	if idle > 0 {
		idleTimer = time.NewTimer(idle)
		defer idleTimer.Stop()
	}

	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idle)
	}

	for {
		select {
		case <-ctx.Done():
			_ = stream.Close()
			return nil, ctx.Err()
		case <-func() <-chan time.Time {
			if idleTimer == nil {
				return nil
			}
			return idleTimer.C
		}():
			_ = stream.Close()
			return nil, core.NewAPIError("upstream stream idle timeout")
		case n, ok := <-nextCh:
			if !ok {
				goto done
			}
			if n.ev != nil {
				acc.Apply(n.ev)
				if err := emit(types.RunStreamEventWrapper{Type: "stream_event", Event: n.ev}); err != nil {
					return nil, err
				}
				if ttsStream != nil {
					if cbd, ok := n.ev.(types.ContentBlockDeltaEvent); ok {
						if td, ok := cbd.Delta.(types.TextDelta); ok {
							if err := ttsStream.OnTextDelta(td.Text); err != nil {
								_ = emit(types.AudioUnavailableEvent{Type: "audio_unavailable", Reason: "tts_failed", Message: "TTS synthesis failed: " + err.Error()})
								ttsStream = nil
							}
						}
					}
					if ttsStream != nil {
						for i := 0; i < 2; i++ {
							select {
							case chunk, ok := <-audioCh:
								if !ok {
									audioCh = nil
									break
								}
								if len(chunk) == 0 {
									continue
								}
								if err := emitAudio(chunk, false); err != nil {
									return nil, err
								}
							default:
								i = 2
							}
						}
					}
				}
			}
			if n.err != nil {
				if errors.Is(n.err, io.EOF) {
					goto done
				}
				return nil, n.err
			}
			resetIdle()
		}
	}

done:
	if ttsStream != nil {
		if err := ttsStream.Flush(); err != nil {
			_ = emit(types.AudioUnavailableEvent{Type: "audio_unavailable", Reason: "tts_failed", Message: "TTS synthesis failed: " + err.Error()})
		}
		_ = ttsStream.Close()

		var pending []byte
		for chunk := range audioCh {
			if len(chunk) == 0 {
				continue
			}
			if len(pending) > 0 {
				if err := emitAudio(pending, false); err != nil {
					return nil, err
				}
			}
			pending = chunk
		}
		if len(pending) > 0 {
			if err := emitAudio(pending, true); err != nil {
				return nil, err
			}
		}

		if err := ttsStream.Err(); err != nil {
			_ = emit(types.AudioUnavailableEvent{Type: "audio_unavailable", Reason: "tts_failed", Message: "TTS synthesis failed: " + err.Error()})
		}
	}

	return acc.Response(), nil
}

func stopReasonFromContext(err error) (types.RunStopReason, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.Canceled) {
		return types.RunStopReasonCancelled, true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return types.RunStopReasonTimeout, true
	}
	return "", false
}

func toTypesError(err error, requestID string) types.Error {
	coreErr, _ := apierror.FromError(err, requestID)

	if coreErr == nil {
		return types.Error{Type: string(core.ErrAPI), Message: "internal error", RequestID: requestID}
	}
	// Preserve raw cause text for stream-level diagnostics when the mapper falls
	// back to a generic internal error.
	if coreErr.Type == core.ErrAPI && coreErr.Message == "internal error" && coreErr.ProviderError == nil && err != nil {
		coreErr.ProviderError = err.Error()
	}
	return types.Error{
		Type:          string(coreErr.Type),
		Message:       coreErr.Message,
		Param:         coreErr.Param,
		Code:          coreErr.Code,
		RequestID:     coreErr.RequestID,
		ProviderError: coreErr.ProviderError,
		RetryAfter:    coreErr.RetryAfter,
		CompatIssues:  toTypesCompatIssues(coreErr.CompatIssues),
	}
}

func toTypesCompatIssues(issues []core.CompatibilityIssue) []types.CompatibilityIssue {
	if len(issues) == 0 {
		return nil
	}
	out := make([]types.CompatibilityIssue, len(issues))
	for i := range issues {
		out[i] = types.CompatibilityIssue{
			Severity: issues[i].Severity,
			Param:    issues[i].Param,
			Code:     issues[i].Code,
			Message:  issues[i].Message,
		}
	}
	return out
}
