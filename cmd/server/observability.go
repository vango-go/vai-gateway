package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	headerRequestID         = "X-Request-ID"
	headerSessionID         = "X-VAI-Session-ID"
	headerChainID           = "X-VAI-Chain-ID"
	headerParentRequestID   = "X-VAI-Parent-Request-ID"
	endpointKindMessages    = "messages"
	endpointKindMessagesSSE = "messages_stream"
	endpointKindRuns        = "runs"
	endpointKindRunsSSE     = "runs_stream"
)

type observedGatewayRequest struct {
	RequestID               string
	SessionID               string
	EndpointKind            string
	EndpointFamily          string
	Method                  string
	Path                    string
	Provider                string
	Model                   string
	RequestKind             string
	RequestBody             string
	RequestSummary          string
	InputContextFingerprint string
	System                  any
	Messages                []types.Message
	RunConfig               string
}

type observedGatewayCompletion struct {
	StatusCode               int
	DurationMS               int64
	ResponseBody             string
	ResponseSummary          string
	ErrorSummary             string
	ErrorJSON                string
	OutputContextFingerprint string
	RunTrace                 *services.GatewayRunTraceRecord
	Usage                    *types.Usage
	RunResult                *types.RunResult
}

func ensureGatewayRequestID(headers http.Header) string {
	if headers == nil {
		return "req_" + randomHexLocal(10)
	}
	if existing := strings.TrimSpace(headers.Get(headerRequestID)); existing != "" {
		return existing
	}
	id := "req_" + randomHexLocal(10)
	headers.Set(headerRequestID, id)
	return id
}

func fallbackObservedGatewayRequest(r *http.Request, body []byte, requestID string) *observedGatewayRequest {
	if r == nil {
		return nil
	}
	endpointKind := ""
	endpointFamily := ""
	requestKind := ""
	switch r.URL.Path {
	case "/v1/messages":
		endpointKind = endpointKindMessages
		endpointFamily = endpointKindMessages
		requestKind = "gateway_messages"
		if strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Accept"))), "text/event-stream") {
			endpointKind = endpointKindMessagesSSE
			requestKind = "gateway_messages_stream"
		}
	case "/v1/runs":
		endpointKind = endpointKindRuns
		endpointFamily = endpointKindRuns
		requestKind = "gateway_runs"
	case "/v1/runs:stream":
		endpointKind = endpointKindRunsSSE
		endpointFamily = endpointKindRuns
		requestKind = "gateway_runs_stream"
	default:
		return nil
	}
	return &observedGatewayRequest{
		RequestID:      requestID,
		SessionID:      strings.TrimSpace(r.Header.Get(headerSessionID)),
		EndpointKind:   endpointKind,
		EndpointFamily: endpointFamily,
		Method:         r.Method,
		Path:           r.URL.Path,
		RequestKind:    requestKind,
		RequestBody:    strings.TrimSpace(string(body)),
	}
}

func parseObservedGatewayRequest(r *http.Request, body []byte, requestID string) (*observedGatewayRequest, error) {
	if r == nil {
		return nil, errors.New("request is required")
	}
	switch r.URL.Path {
	case "/v1/messages":
		var req types.MessageRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return &observedGatewayRequest{
					RequestID:      requestID,
					SessionID:      strings.TrimSpace(r.Header.Get(headerSessionID)),
					EndpointKind:   endpointKindMessages,
					EndpointFamily: endpointKindMessages,
					Method:         r.Method,
					Path:           r.URL.Path,
					RequestKind:    "gateway_messages",
					RequestBody:    string(body),
				}, nil
			}
		}
		endpointKind := endpointKindMessages
		requestKind := "gateway_messages"
		if req.Stream || strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Accept"))), "text/event-stream") {
			endpointKind = endpointKindMessagesSSE
			requestKind = "gateway_messages_stream"
		}
		inputFingerprint, err := services.ContextFingerprint(req.System, req.Messages)
		if err != nil {
			return nil, err
		}
		return &observedGatewayRequest{
			RequestID:               requestID,
			SessionID:               strings.TrimSpace(r.Header.Get(headerSessionID)),
			EndpointKind:            endpointKind,
			EndpointFamily:          endpointKindMessages,
			Method:                  r.Method,
			Path:                    r.URL.Path,
			Provider:                providerFromModel(req.Model),
			Model:                   strings.TrimSpace(req.Model),
			RequestKind:             requestKind,
			RequestBody:             strings.TrimSpace(string(body)),
			RequestSummary:          mustJSONText(messageRequestSummary(&req, endpointKind == endpointKindMessagesSSE)),
			InputContextFingerprint: inputFingerprint,
			System:                  req.System,
			Messages:                req.Messages,
		}, nil
	case "/v1/runs", "/v1/runs:stream":
		var req types.RunRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				kind := endpointKindRuns
				requestKind := "gateway_runs"
				if r.URL.Path == "/v1/runs:stream" {
					kind = endpointKindRunsSSE
					requestKind = "gateway_runs_stream"
				}
				return &observedGatewayRequest{
					RequestID:      requestID,
					SessionID:      strings.TrimSpace(r.Header.Get(headerSessionID)),
					EndpointKind:   kind,
					EndpointFamily: endpointKindRuns,
					Method:         r.Method,
					Path:           r.URL.Path,
					RequestKind:    requestKind,
					RequestBody:    string(body),
				}, nil
			}
		}
		endpointKind := endpointKindRuns
		requestKind := "gateway_runs"
		if r.URL.Path == "/v1/runs:stream" {
			endpointKind = endpointKindRunsSSE
			requestKind = "gateway_runs_stream"
		}
		inputFingerprint, err := services.ContextFingerprint(req.Request.System, req.Request.Messages)
		if err != nil {
			return nil, err
		}
		runConfig, err := json.Marshal(req.Run)
		if err != nil {
			return nil, err
		}
		return &observedGatewayRequest{
			RequestID:               requestID,
			SessionID:               strings.TrimSpace(r.Header.Get(headerSessionID)),
			EndpointKind:            endpointKind,
			EndpointFamily:          endpointKindRuns,
			Method:                  r.Method,
			Path:                    r.URL.Path,
			Provider:                providerFromModel(req.Request.Model),
			Model:                   strings.TrimSpace(req.Request.Model),
			RequestKind:             requestKind,
			RequestBody:             strings.TrimSpace(string(body)),
			RequestSummary:          mustJSONText(runRequestSummary(&req, endpointKind == endpointKindRunsSSE)),
			InputContextFingerprint: inputFingerprint,
			System:                  req.Request.System,
			Messages:                req.Request.Messages,
			RunConfig:               string(runConfig),
		}, nil
	default:
		return nil, nil
	}
}

func completeObservedGatewayRequest(observed *observedGatewayRequest, statusCode int, responseBody []byte) (*observedGatewayCompletion, error) {
	if observed == nil {
		return nil, nil
	}
	completion := &observedGatewayCompletion{
		StatusCode: statusCode,
	}
	if statusCode >= http.StatusBadRequest {
		completion.ResponseBody = strings.TrimSpace(string(responseBody))
		completion.ResponseSummary = mustJSONText(map[string]any{
			"status_code": statusCode,
			"error":       extractGatewayErrorMessage(responseBody),
		})
		completion.ErrorSummary, completion.ErrorJSON = extractGatewayErrorDetails(responseBody)
		return completion, nil
	}

	switch observed.EndpointKind {
	case endpointKindMessages:
		resp, err := types.UnmarshalMessageResponse(responseBody)
		if err != nil {
			return nil, err
		}
		completion.Usage = &resp.Usage
		completion.ResponseBody = mustJSONText(resp)
		completion.ResponseSummary = mustJSONText(messageResponseSummary(resp, false))
		completion.OutputContextFingerprint, err = services.AssistantResponseFingerprint(observed.System, observed.Messages, resp)
		if err != nil {
			return nil, err
		}
	case endpointKindMessagesSSE:
		resp, streamErr, err := materializeMessageResponseFromSSE(responseBody)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			completion.Usage = &resp.Usage
			completion.ResponseBody = mustJSONText(resp)
			completion.ResponseSummary = mustJSONText(messageResponseSummary(resp, streamErr != nil))
			if streamErr == nil {
				completion.OutputContextFingerprint, err = services.AssistantResponseFingerprint(observed.System, observed.Messages, resp)
				if err != nil {
					return nil, err
				}
			}
		}
		if streamErr != nil {
			completion.ErrorSummary = streamErr.Message
			completion.ErrorJSON = mustJSONText(streamErr)
		}
	case endpointKindRuns:
		var env types.RunResultEnvelope
		if err := json.Unmarshal(responseBody, &env); err != nil {
			return nil, err
		}
		if env.Result == nil {
			return completion, nil
		}
		completion.RunResult = env.Result
		completion.Usage = &env.Result.Usage
		completion.ResponseBody = mustJSONText(env)
		completion.ResponseSummary = mustJSONText(runResultSummary(env.Result, false))
		completion.RunTrace = buildRunTraceRecord(observed, env.Result)
		if env.Result.Response != nil {
			fingerprint, err := services.AssistantResponseFingerprint(observed.System, observed.Messages, env.Result.Response)
			if err != nil {
				return nil, err
			}
			completion.OutputContextFingerprint = fingerprint
		}
	case endpointKindRunsSSE:
		result, streamErr, err := materializeRunResultFromSSE(responseBody)
		if err != nil {
			return nil, err
		}
		if result != nil {
			completion.RunResult = result
			completion.Usage = &result.Usage
			completion.ResponseBody = mustJSONText(types.RunResultEnvelope{Result: result})
			completion.ResponseSummary = mustJSONText(runResultSummary(result, streamErr != nil))
			completion.RunTrace = buildRunTraceRecord(observed, result)
			if streamErr == nil && result.Response != nil {
				fingerprint, err := services.AssistantResponseFingerprint(observed.System, observed.Messages, result.Response)
				if err != nil {
					return nil, err
				}
				completion.OutputContextFingerprint = fingerprint
			}
		}
		if streamErr != nil {
			completion.ErrorSummary = streamErr.Message
			completion.ErrorJSON = mustJSONText(streamErr)
		}
	}
	return completion, nil
}

func materializeMessageResponseFromSSE(raw []byte) (*types.MessageResponse, *types.Error, error) {
	events := parseSSE(raw)
	var response types.MessageResponse
	var currentContent []types.ContentBlock
	toolInputBuffers := make(map[int]string)
	applyToolInput := func(index int) {
		rawInput, ok := toolInputBuffers[index]
		if !ok || rawInput == "" || index < 0 || index >= len(currentContent) {
			return
		}
		delete(toolInputBuffers, index)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(rawInput), &parsed); err != nil {
			return
		}
		switch block := currentContent[index].(type) {
		case types.ToolUseBlock:
			block.Input = parsed
			currentContent[index] = block
		case types.ServerToolUseBlock:
			block.Input = parsed
			currentContent[index] = block
		}
	}

	var streamErr *types.Error
	for _, item := range events {
		event, err := types.UnmarshalStreamEvent([]byte(item.Data))
		if err != nil {
			return nil, nil, err
		}
		switch e := event.(type) {
		case types.MessageStartEvent:
			response = e.Message
		case types.ContentBlockStartEvent:
			for len(currentContent) <= e.Index {
				currentContent = append(currentContent, nil)
			}
			currentContent[e.Index] = e.ContentBlock
		case types.ContentBlockDeltaEvent:
			if e.Index < len(currentContent) {
				currentContent[e.Index] = applyObservedDelta(currentContent[e.Index], e.Delta)
			}
			if inputDelta, ok := e.Delta.(types.InputJSONDelta); ok {
				toolInputBuffers[e.Index] += inputDelta.PartialJSON
			}
		case types.ContentBlockStopEvent:
			applyToolInput(e.Index)
		case types.MessageDeltaEvent:
			response.StopReason = e.Delta.StopReason
			response.Usage = e.Usage
		case types.MessageStopEvent:
			// terminal success
		case types.ErrorEvent:
			copyErr := e.Error
			streamErr = &copyErr
		}
	}
	for index := range toolInputBuffers {
		applyToolInput(index)
	}
	response.Content = compactObservedContentBlocks(currentContent)
	if response.Type == "" && len(response.Content) == 0 && response.ID == "" {
		return nil, streamErr, nil
	}
	return &response, streamErr, nil
}

func materializeRunResultFromSSE(raw []byte) (*types.RunResult, *types.Error, error) {
	events := parseSSE(raw)
	var streamErr *types.Error
	for i := len(events) - 1; i >= 0; i-- {
		ev, err := types.UnmarshalRunStreamEvent([]byte(events[i].Data))
		if err != nil {
			return nil, nil, err
		}
		switch event := ev.(type) {
		case types.RunCompleteEvent:
			return event.Result, streamErr, nil
		case types.RunErrorEvent:
			copyErr := event.Error
			streamErr = &copyErr
		}
	}
	return nil, streamErr, nil
}

func buildRunTraceRecord(observed *observedGatewayRequest, result *types.RunResult) *services.GatewayRunTraceRecord {
	if observed == nil || result == nil {
		return nil
	}
	trace := &services.GatewayRunTraceRecord{
		RunConfig:     observed.RunConfig,
		Usage:         mustJSONText(result.Usage),
		StopReason:    string(result.StopReason),
		TurnCount:     result.TurnCount,
		ToolCallCount: result.ToolCallCount,
	}
	for _, step := range result.Steps {
		stepRecord := services.GatewayRunStepRecord{
			StepIndex:       step.Index,
			DurationMS:      step.DurationMS,
			ResponseSummary: mustJSONText(runStepSummary(step)),
		}
		if step.Response != nil {
			stepRecord.ResponseBody = mustJSONText(step.Response)
		}
		for _, call := range step.ToolCalls {
			inputJSON := mustJSONText(call.Input)
			resultBody := ""
			isError := false
			errorSummary := ""
			for _, toolResult := range step.ToolResults {
				if toolResult.ToolUseID != call.ID {
					continue
				}
				resultBody = mustJSONText(toolResult.Content)
				isError = toolResult.IsError
				if toolResult.Error != nil {
					errorSummary = toolResult.Error.Message
				}
				break
			}
			stepRecord.ToolCalls = append(stepRecord.ToolCalls, services.GatewayRunToolCallRecord{
				ToolCallID:   call.ID,
				Name:         call.Name,
				InputJSON:    inputJSON,
				ResultBody:   resultBody,
				IsError:      isError,
				ErrorSummary: errorSummary,
			})
		}
		trace.Steps = append(trace.Steps, stepRecord)
	}
	return trace
}

func messageRequestSummary(req *types.MessageRequest, stream bool) map[string]any {
	if req == nil {
		return map[string]any{"stream": stream}
	}
	serverTools := make([]string, 0)
	if req.Tools != nil {
		for _, tool := range req.Tools {
			if name := strings.TrimSpace(tool.Name); name != "" {
				serverTools = append(serverTools, name)
			}
		}
	}
	return map[string]any{
		"stream":                stream,
		"messages_count":        len(req.Messages),
		"system_present":        req.System != nil,
		"tool_count":            len(req.Tools),
		"tool_names":            serverTools,
		"max_tokens":            req.MaxTokens,
		"has_audio_input":       requestHasAudioInput(req),
		"requests_image_output": requestRequestsImageOutput(req),
	}
}

func runRequestSummary(req *types.RunRequest, stream bool) map[string]any {
	if req == nil {
		return map[string]any{"stream": stream}
	}
	enabledServerTools := append([]string(nil), req.ServerTools...)
	if len(enabledServerTools) == 0 {
		enabledServerTools = append(enabledServerTools, req.Builtins...)
	}
	return map[string]any{
		"stream":                stream,
		"messages_count":        len(req.Request.Messages),
		"system_present":        req.Request.System != nil,
		"tool_count":            len(req.Request.Tools),
		"server_tools":          enabledServerTools,
		"max_tokens":            req.Request.MaxTokens,
		"run_max_turns":         req.Run.MaxTurns,
		"run_max_tool_calls":    req.Run.MaxToolCalls,
		"run_timeout_ms":        req.Run.TimeoutMS,
		"run_parallel_tools":    req.Run.ParallelTools,
		"has_audio_input":       requestHasAudioInput(&req.Request),
		"requests_image_output": requestRequestsImageOutput(&req.Request),
	}
}

func messageResponseSummary(resp *types.MessageResponse, partial bool) map[string]any {
	if resp == nil {
		return map[string]any{"partial": partial}
	}
	return map[string]any{
		"message_id":          resp.ID,
		"stop_reason":         string(resp.StopReason),
		"input_tokens":        resp.Usage.InputTokens,
		"output_tokens":       resp.Usage.OutputTokens,
		"total_tokens":        resp.Usage.TotalTokens,
		"content_block_count": len(resp.Content),
		"partial":             partial,
	}
}

func runResultSummary(result *types.RunResult, partial bool) map[string]any {
	if result == nil {
		return map[string]any{"partial": partial}
	}
	return map[string]any{
		"stop_reason":     string(result.StopReason),
		"turn_count":      result.TurnCount,
		"tool_call_count": result.ToolCallCount,
		"input_tokens":    result.Usage.InputTokens,
		"output_tokens":   result.Usage.OutputTokens,
		"total_tokens":    result.Usage.TotalTokens,
		"partial":         partial,
	}
}

func runStepSummary(step types.RunStep) map[string]any {
	return map[string]any{
		"step_index":      step.Index,
		"duration_ms":     step.DurationMS,
		"tool_call_count": len(step.ToolCalls),
		"has_response":    step.Response != nil,
	}
}

func extractGatewayErrorDetails(raw []byte) (string, string) {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Error) == 0 {
		message := strings.TrimSpace(string(raw))
		if message == "" {
			message = "gateway request failed"
		}
		return message, mustJSONText(map[string]any{
			"message": message,
		})
	}
	var typed types.Error
	if err := json.Unmarshal(env.Error, &typed); err == nil && strings.TrimSpace(typed.Message) != "" {
		return strings.TrimSpace(typed.Message), mustJSONText(typed)
	}
	var text string
	if err := json.Unmarshal(env.Error, &text); err == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text), mustJSONText(map[string]any{"message": strings.TrimSpace(text)})
	}
	var obj map[string]any
	if err := json.Unmarshal(env.Error, &obj); err == nil {
		if message, _ := obj["message"].(string); strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message), mustJSONText(obj)
		}
	}
	message := strings.TrimSpace(string(env.Error))
	if message == "" {
		message = "gateway request failed"
	}
	return message, mustJSONText(map[string]any{"message": message})
}

func providerFromModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "/"); idx > 0 {
		return model[:idx]
	}
	return model
}

func mustJSONText(value any) string {
	if value == nil {
		return "{}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func applyObservedDelta(block types.ContentBlock, delta types.Delta) types.ContentBlock {
	switch d := delta.(type) {
	case types.TextDelta:
		if tb, ok := block.(types.TextBlock); ok {
			tb.Text += d.Text
			return tb
		}
		if tb, ok := block.(*types.TextBlock); ok && tb != nil {
			tb.Text += d.Text
			return tb
		}
	case types.ThinkingDelta:
		if tb, ok := block.(types.ThinkingBlock); ok {
			tb.Thinking += d.Thinking
			return tb
		}
		if tb, ok := block.(*types.ThinkingBlock); ok && tb != nil {
			tb.Thinking += d.Thinking
			return tb
		}
	}
	return block
}

func compactObservedContentBlocks(blocks []types.ContentBlock) []types.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]types.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block == nil {
			continue
		}
		out = append(out, block)
	}
	return out
}

func randomHexLocal(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)[:n]
}
