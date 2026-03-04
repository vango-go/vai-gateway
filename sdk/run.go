package vai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

// RunStopReason indicates why the run loop terminated.
type RunStopReason string

const (
	RunStopEndTurn      RunStopReason = "end_turn"       // Model finished naturally
	RunStopMaxToolCalls RunStopReason = "max_tool_calls" // Hit tool call limit
	RunStopMaxTurns     RunStopReason = "max_turns"      // Hit turn limit
	RunStopMaxTokens    RunStopReason = "max_tokens"     // Hit token limit
	RunStopTimeout      RunStopReason = "timeout"        // Hit timeout
	RunStopCustom       RunStopReason = "custom"         // Custom stop condition
	RunStopCancelled    RunStopReason = "cancelled"      // Cancelled by user
	RunStopError        RunStopReason = "error"          // Error occurred
)

// RunResult contains the result of a tool execution loop.
type RunResult struct {
	Response      *Response       `json:"response"`
	Steps         []RunStep       `json:"steps"`
	ToolCallCount int             `json:"tool_call_count"`
	TurnCount     int             `json:"turn_count"`
	Usage         types.Usage     `json:"usage"`
	StopReason    RunStopReason   `json:"stop_reason"`
	Messages      []types.Message `json:"messages,omitempty"`
}

// RunStep represents a single step in the tool execution loop.
type RunStep struct {
	Index       int                   `json:"index"`
	Response    *Response             `json:"response"`
	ToolCalls   []ToolCall            `json:"tool_calls,omitempty"`
	ToolResults []ToolExecutionResult `json:"tool_results,omitempty"`
	DurationMs  int64                 `json:"duration_ms"`
}

// ToolCall represents a tool invocation from the model.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolExecutionResult represents the result of a tool execution.
type ToolExecutionResult struct {
	ToolUseID string               `json:"tool_use_id"`
	Content   []types.ContentBlock `json:"content"`
	Error     error                `json:"-"`
	ErrorMsg  string               `json:"error,omitempty"`
}

// ToolHandler is a function that handles a tool call.
type ToolHandler func(ctx context.Context, input json.RawMessage) (any, error)

func stopReasonFromContextErr(err error) (RunStopReason, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return RunStopCancelled, true
	case errors.Is(err, context.DeadlineExceeded):
		return RunStopTimeout, true
	default:
		return "", false
	}
}

// runConfig holds configuration for the Run loop.
type runConfig struct {
	maxToolCalls  int
	maxTurns      int
	maxTokens     int
	timeout       time.Duration
	stopWhen      func(*Response) bool
	toolHandlers  map[string]ToolHandler
	extraTools    []types.Tool
	beforeCall    func(*MessageRequest)
	afterResponse func(*Response)
	onToolCall    func(name string, input map[string]any, output any, err error)
	onStop        func(*RunResult)
	parallelTools bool
	toolTimeout   time.Duration
	buildTurnMsgs func(info TurnInfo) []types.Message
}

// defaultRunConfig returns the default run configuration.
func defaultRunConfig() runConfig {
	return runConfig{
		toolHandlers:  make(map[string]ToolHandler),
		parallelTools: true,
		toolTimeout:   30 * time.Second,
	}
}

// RunOption configures the Run loop.
type RunOption func(*runConfig)

// TurnInfo describes the state at a turn boundary.
// History is a snapshot of the SDK's internal append-only history and should be treated as read-only.
type TurnInfo struct {
	TurnIndex int
	History   []types.Message
}

// WithMaxToolCalls sets the maximum number of tool calls before stopping.
func WithMaxToolCalls(n int) RunOption {
	return func(c *runConfig) { c.maxToolCalls = n }
}

// WithMaxTurns sets the maximum number of LLM turns before stopping.
func WithMaxTurns(n int) RunOption {
	return func(c *runConfig) { c.maxTurns = n }
}

// WithMaxTokensRun sets the maximum total tokens before stopping.
func WithMaxTokensRun(n int) RunOption {
	return func(c *runConfig) { c.maxTokens = n }
}

// WithRunTimeout sets a timeout for the entire Run loop.
func WithRunTimeout(d time.Duration) RunOption {
	return func(c *runConfig) { c.timeout = d }
}

// WithStopWhen sets a custom stop condition.
// The function is called after each response.
// If it returns true, the run stops.
func WithStopWhen(fn func(*Response) bool) RunOption {
	return func(c *runConfig) { c.stopWhen = fn }
}

// WithToolHandler registers a handler for a specific tool.
func WithToolHandler(name string, fn ToolHandler) RunOption {
	return func(c *runConfig) {
		if c.toolHandlers == nil {
			c.toolHandlers = make(map[string]ToolHandler)
		}
		c.toolHandlers[name] = fn
	}
}

// WithToolHandlers registers multiple tool handlers.
func WithToolHandlers(handlers map[string]ToolHandler) RunOption {
	return func(c *runConfig) {
		if c.toolHandlers == nil {
			c.toolHandlers = make(map[string]ToolHandler)
		}
		for name, fn := range handlers {
			c.toolHandlers[name] = fn
		}
	}
}

// WithTools registers tools that have embedded handlers (from MakeTool).
func WithTools(tools ...ToolWithHandler) RunOption {
	return func(c *runConfig) {
		if c.toolHandlers == nil {
			c.toolHandlers = make(map[string]ToolHandler)
		}
		for _, t := range tools {
			if t.Handler != nil && t.Name != "" {
				c.toolHandlers[t.Name] = t.Handler
				c.extraTools = append(c.extraTools, t.Tool)
			}
		}
	}
}

// WithToolSet registers all handlers from a ToolSet.
func WithToolSet(ts *ToolSet) RunOption {
	return func(c *runConfig) {
		if c.toolHandlers == nil {
			c.toolHandlers = make(map[string]ToolHandler)
		}
		for name, handler := range ts.Handlers() {
			c.toolHandlers[name] = handler
		}
		c.extraTools = append(c.extraTools, ts.Tools()...)
	}
}

// WithBeforeCall sets a hook called before each LLM call.
func WithBeforeCall(fn func(*MessageRequest)) RunOption {
	return func(c *runConfig) { c.beforeCall = fn }
}

// WithAfterResponse sets a hook called after each LLM response.
func WithAfterResponse(fn func(*Response)) RunOption {
	return func(c *runConfig) { c.afterResponse = fn }
}

// WithBuildTurnMessages sets a hook called at each turn boundary to build the message list
// that will be sent to the model for that turn.
//
// This enables advanced context management (pinned memory blocks, trimming, reordering, etc.)
// without mutating the underlying append-only history.
func WithBuildTurnMessages(fn func(info TurnInfo) []types.Message) RunOption {
	return func(c *runConfig) { c.buildTurnMsgs = fn }
}

// WithOnToolCall sets a hook called after each tool execution.
func WithOnToolCall(fn func(name string, input map[string]any, output any, err error)) RunOption {
	return func(c *runConfig) { c.onToolCall = fn }
}

// WithOnStop sets a hook called when the loop stops.
func WithOnStop(fn func(*RunResult)) RunOption {
	return func(c *runConfig) { c.onStop = fn }
}

// WithParallelTools enables parallel execution of independent tool calls.
// Default is true.
func WithParallelTools(enabled bool) RunOption {
	return func(c *runConfig) { c.parallelTools = enabled }
}

// WithToolTimeout sets a timeout for individual tool executions.
// Default is 30 seconds.
func WithToolTimeout(d time.Duration) RunOption {
	return func(c *runConfig) { c.toolTimeout = d }
}

func mergeTools(reqTools []types.Tool, extraTools []types.Tool) []types.Tool {
	if len(extraTools) == 0 {
		return reqTools
	}

	out := make([]types.Tool, 0, len(reqTools)+len(extraTools))
	seenFunc := make(map[string]struct{}, len(reqTools)+len(extraTools))

	add := func(t types.Tool) {
		if t.Type == types.ToolTypeFunction && t.Name != "" {
			if _, ok := seenFunc[t.Name]; ok {
				return
			}
			seenFunc[t.Name] = struct{}{}
		}
		out = append(out, t)
	}

	for _, t := range reqTools {
		add(t)
	}
	for _, t := range extraTools {
		add(t)
	}
	return out
}

// --- Run Loop Implementation ---

// runLoop executes the main tool execution loop.
func (s *MessagesService) runLoop(ctx context.Context, req *MessageRequest, cfg *runConfig) (*RunResult, error) {
	if req == nil {
		return nil, fmt.Errorf("req must not be nil")
	}

	result := &RunResult{
		Steps: make([]RunStep, 0),
	}

	workingReq := *req
	userTranscript := ""
	proxyMode := s.client != nil && s.client.isProxyMode()

	if !proxyMode && req.Voice != nil && req.Voice.Input != nil {
		processedReq, transcript, err := s.preprocessVoiceInput(ctx, req)
		if err != nil {
			return nil, err
		}
		workingReq = *processedReq
		userTranscript = transcript
	}
	if !proxyMode && req.Voice != nil && req.Voice.Output != nil {
		if err := s.requireVoicePipeline(); err != nil {
			return nil, err
		}
	}

	history := make([]types.Message, len(workingReq.Messages))
	copy(history, workingReq.Messages)

	snapshotMessages := func() []types.Message {
		out := make([]types.Message, len(history))
		copy(out, history)
		return out
	}

	finalizeResponse := func(resp *Response) error {
		if resp == nil || resp.MessageResponse == nil {
			return nil
		}
		if userTranscript != "" {
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			resp.Metadata["user_transcript"] = userTranscript
		}
		if !proxyMode && workingReq.Voice != nil && workingReq.Voice.Output != nil {
			if err := s.appendVoiceOutput(ctx, &workingReq, resp.MessageResponse); err != nil {
				return err
			}
		}
		return nil
	}

	// Apply timeout if configured
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	// Main loop
	for {
		// Check timeout
		select {
		case <-ctx.Done():
			if stopReason, ok := stopReasonFromContextErr(ctx.Err()); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, ctx.Err()
		default:
		}

		// Check turn limit
		if cfg.maxTurns > 0 && result.TurnCount >= cfg.maxTurns {
			result.StopReason = RunStopMaxTurns
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		// Check token limit
		if cfg.maxTokens > 0 && result.Usage.TotalTokens >= cfg.maxTokens {
			result.StopReason = RunStopMaxTokens
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		messagesForTurn := history
		if cfg.buildTurnMsgs != nil {
			historySnapshot := make([]types.Message, len(history))
			copy(historySnapshot, history)
			messagesForTurn = cfg.buildTurnMsgs(TurnInfo{
				TurnIndex: result.TurnCount,
				History:   historySnapshot,
			})
		}

		// Build request for this turn
		turnReq := &types.MessageRequest{
			Model:         workingReq.Model,
			Messages:      messagesForTurn,
			MaxTokens:     workingReq.MaxTokens,
			System:        workingReq.System,
			Temperature:   workingReq.Temperature,
			TopP:          workingReq.TopP,
			TopK:          workingReq.TopK,
			StopSequences: workingReq.StopSequences,
			Tools:         mergeTools(workingReq.Tools, cfg.extraTools),
			ToolChoice:    workingReq.ToolChoice,
			OutputFormat:  workingReq.OutputFormat,
			Output:        workingReq.Output,
			Extensions:    workingReq.Extensions,
			Metadata:      workingReq.Metadata,
		}

		// Call before hook
		if cfg.beforeCall != nil {
			cfg.beforeCall(turnReq)
		}

		// Make the API call
		stepStart := time.Now()
		resp, err := s.createTurn(ctx, turnReq)
		stepDuration := time.Since(stepStart).Milliseconds()

		if err != nil {
			if stopReason, ok := stopReasonFromContextErr(err); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, err
		}

		// Call after hook
		if cfg.afterResponse != nil {
			cfg.afterResponse(resp)
		}

		// Aggregate usage
		result.Usage = result.Usage.Add(resp.Usage)
		result.TurnCount++

		// Create step record
		step := RunStep{
			Index:      len(result.Steps),
			Response:   resp,
			DurationMs: stepDuration,
		}

		// Check custom stop condition
		if cfg.stopWhen != nil && cfg.stopWhen(resp) {
			if err := finalizeResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotMessages()
				if cfg.onStop != nil {
					cfg.onStop(result)
				}
				return result, err
			}
			history = AppendAssistantMessage(history, resp.Content)
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopCustom
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		// Check if model finished without tool calls
		if resp.StopReason != types.StopReasonToolUse {
			if err := finalizeResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotMessages()
				if cfg.onStop != nil {
					cfg.onStop(result)
				}
				return result, err
			}
			history = AppendAssistantMessage(history, resp.Content)
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopEndTurn
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		// Process tool calls
		toolUses := resp.ToolUses()
		if len(toolUses) == 0 {
			if err := finalizeResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotMessages()
				if cfg.onStop != nil {
					cfg.onStop(result)
				}
				return result, err
			}
			history = AppendAssistantMessage(history, resp.Content)
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopEndTurn
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		// Check tool call limit
		if cfg.maxToolCalls > 0 && result.ToolCallCount+len(toolUses) > cfg.maxToolCalls {
			if err := finalizeResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotMessages()
				if cfg.onStop != nil {
					cfg.onStop(result)
				}
				return result, err
			}
			history = AppendAssistantMessage(history, resp.Content)
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopMaxToolCalls
			result.Messages = snapshotMessages()
			if cfg.onStop != nil {
				cfg.onStop(result)
			}
			return result, nil
		}

		// Execute tool calls
		toolResults := s.executeToolCalls(ctx, toolUses, cfg)

		step.ToolCalls = make([]ToolCall, len(toolUses))
		for i, tu := range toolUses {
			step.ToolCalls[i] = ToolCall{
				ID:    tu.ID,
				Name:  tu.Name,
				Input: tu.Input,
			}
		}
		step.ToolResults = toolResults
		result.Steps = append(result.Steps, step)
		result.ToolCallCount += len(toolUses)

		// Append assistant message with tool calls
		history = AppendAssistantMessage(history, resp.Content)
		history = AppendToolResultsMessage(history, toolResults)
	}
}

// executeToolCalls executes all tool calls, either in parallel or sequentially.
func (s *MessagesService) executeToolCalls(ctx context.Context, toolUses []types.ToolUseBlock, cfg *runConfig) []ToolExecutionResult {
	results := make([]ToolExecutionResult, len(toolUses))

	if cfg.parallelTools && len(toolUses) > 1 {
		// Execute in parallel
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i, tu := range toolUses {
			wg.Add(1)
			go func(idx int, toolUse types.ToolUseBlock) {
				defer wg.Done()
				result := s.executeToolCall(ctx, toolUse, cfg)
				mu.Lock()
				results[idx] = result
				mu.Unlock()
			}(i, tu)
		}
		wg.Wait()
	} else {
		// Execute sequentially
		for i, tu := range toolUses {
			results[i] = s.executeToolCall(ctx, tu, cfg)
		}
	}

	return results
}

// executeToolCall executes a single tool call.
func (s *MessagesService) executeToolCall(ctx context.Context, toolUse types.ToolUseBlock, cfg *runConfig) ToolExecutionResult {
	result := ToolExecutionResult{
		ToolUseID: toolUse.ID,
	}

	// Apply tool timeout
	if cfg.toolTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.toolTimeout)
		defer cancel()
	}

	// Find handler
	handler, ok := cfg.toolHandlers[toolUse.Name]
	if !ok {
		// No handler - return a generic message
		result.Content = []types.ContentBlock{
			types.TextBlock{
				Type: "text",
				Text: fmt.Sprintf("Tool '%s' was called but no handler is registered.", toolUse.Name),
			},
		}
		return result
	}

	// Marshal input
	inputJSON, err := json.Marshal(toolUse.Input)
	if err != nil {
		result.Error = err
		result.ErrorMsg = err.Error()
		result.Content = []types.ContentBlock{
			types.TextBlock{
				Type: "text",
				Text: fmt.Sprintf("Error marshaling tool input: %v", err),
			},
		}
		return result
	}

	// Execute handler with SDK client bound in context for gateway-native tool handlers.
	toolCtx := contextWithToolExecutionClient(ctx, s.client)
	output, err := handler(toolCtx, inputJSON)

	// Call hook
	if cfg.onToolCall != nil {
		cfg.onToolCall(toolUse.Name, toolUse.Input, output, err)
	}

	if err != nil {
		result.Error = err
		result.ErrorMsg = err.Error()
		result.Content = []types.ContentBlock{
			types.TextBlock{
				Type: "text",
				Text: fmt.Sprintf("Error executing tool: %v", err),
			},
		}
		return result
	}

	// Convert output to content
	result.Content = outputToContentBlocks(output)

	return result
}

// systemToString converts the System field to a string.
func systemToString(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []types.ContentBlock:
		var parts []string
		for _, block := range v {
			if textBlock, ok := block.(types.TextBlock); ok {
				parts = append(parts, textBlock.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		// Try to convert to string
		if s, ok := system.(fmt.Stringer); ok {
			return s.String()
		}
		return fmt.Sprintf("%v", system)
	}
}

// outputToContentBlocks converts tool output to content blocks.
func outputToContentBlocks(output any) []types.ContentBlock {
	switch v := output.(type) {
	case string:
		return []types.ContentBlock{
			types.TextBlock{Type: "text", Text: v},
		}
	case []types.ContentBlock:
		return v
	case types.ContentBlock:
		return []types.ContentBlock{v}
	default:
		// JSON encode other types
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return []types.ContentBlock{
				types.TextBlock{Type: "text", Text: fmt.Sprintf("%v", v)},
			}
		}
		return []types.ContentBlock{
			types.TextBlock{Type: "text", Text: string(jsonBytes)},
		}
	}
}

// --- RunStream Implementation ---

// InterruptBehavior specifies how to handle partial responses when interrupted.
type InterruptBehavior int

const (
	// InterruptDiscard discards the partial response, don't add to history.
	InterruptDiscard InterruptBehavior = iota

	// InterruptSavePartial saves the partial response as-is to history.
	InterruptSavePartial

	// InterruptSaveMarked saves with [interrupted] marker so model knows.
	InterruptSaveMarked
)

// interruptRequest is sent through the interrupt channel.
type interruptRequest struct {
	message  types.Message
	behavior InterruptBehavior
	result   chan error
}

// RunStream wraps a streaming tool execution loop with interrupt support.
type RunStream struct {
	// State protected by mutex
	mu             sync.RWMutex
	messages       []types.Message
	currentStream  *Stream
	partialContent strings.Builder
	cancel         context.CancelFunc

	// Channels
	events    chan RunStreamEvent
	interrupt chan interruptRequest
	done      chan struct{}

	// Result state
	result     *RunResult
	err        error
	closed     atomic.Bool
	finishOnce sync.Once
}

// RunStreamEvent is an event from the RunStream.
type RunStreamEvent interface {
	runStreamEventType() string
}

// StepStartEvent signals the start of a new step.
type StepStartEvent struct {
	Index int `json:"index"`
}

func (e StepStartEvent) runStreamEventType() string { return "step_start" }

// StreamEventWrapper wraps regular stream events from the underlying stream.
type StreamEventWrapper struct {
	Event types.StreamEvent `json:"event"`
}

func (e StreamEventWrapper) runStreamEventType() string { return "stream_event" }

// ToolCallStartEvent signals the start of a tool call.
type ToolCallStartEvent struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func (e ToolCallStartEvent) runStreamEventType() string { return "tool_call_start" }

// ToolResultEvent contains the result of a tool call.
type ToolResultEvent struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Content []types.ContentBlock `json:"content"`
	Error   error                `json:"-"`
}

func (e ToolResultEvent) runStreamEventType() string { return "tool_result" }

// StepCompleteEvent signals the completion of a step.
type StepCompleteEvent struct {
	Index    int       `json:"index"`
	Response *Response `json:"response"`
}

func (e StepCompleteEvent) runStreamEventType() string { return "step_complete" }

// HistoryDeltaEvent signals messages to append to history.
// This is emitted after StepCompleteEvent.
type HistoryDeltaEvent struct {
	ExpectedLen int             `json:"expected_len"`
	Append      []types.Message `json:"append"`
}

func (e HistoryDeltaEvent) runStreamEventType() string { return "history_delta" }

// RunCompleteEvent signals the run is complete.
type RunCompleteEvent struct {
	Result *RunResult `json:"result"`
}

func (e RunCompleteEvent) runStreamEventType() string { return "run_complete" }

// AudioChunkEvent contains streaming audio data from TTS.
type AudioChunkEvent struct {
	Data   []byte `json:"data"`
	Format string `json:"format"`
}

func (e AudioChunkEvent) runStreamEventType() string { return "audio_chunk" }

// InterruptedEvent signals that the stream was interrupted.
type InterruptedEvent struct {
	PartialText string            `json:"partial_text,omitempty"`
	Behavior    InterruptBehavior `json:"behavior"`
}

func (e InterruptedEvent) runStreamEventType() string { return "interrupted" }

// Cancel stops the current stream immediately without injecting a new message.
// The run loop will terminate and control returns to the caller.
// This method is safe to call from any goroutine.
func (rs *RunStream) Cancel() error {
	// Check if already done first to avoid blocking
	select {
	case <-rs.done:
		return nil // Already done
	default:
	}

	req := interruptRequest{
		message:  types.Message{}, // Empty message signals termination
		behavior: InterruptDiscard,
		result:   make(chan error, 1),
	}

	select {
	case rs.interrupt <- req:
		// Interrupt was sent; wait for the run goroutine to process it.
		// Also select on rs.done in case the run goroutine exits without
		// reading the interrupt (e.g., it was between streamLoop and finish).
		select {
		case err := <-req.result:
			return err
		case <-rs.done:
			return nil
		}
	case <-rs.done:
		return nil // Closed while we were waiting to send
	}
}

// Interrupt stops the current stream, saves the partial response according to behavior,
// injects a new message, and continues the conversation.
// This method is safe to call from any goroutine.
func (rs *RunStream) Interrupt(msg types.Message, behavior InterruptBehavior) error {
	// Check if already done first to avoid blocking
	select {
	case <-rs.done:
		return fmt.Errorf("stream already closed")
	default:
	}

	req := interruptRequest{
		message:  msg,
		behavior: behavior,
		result:   make(chan error, 1),
	}

	select {
	case rs.interrupt <- req:
		// Interrupt was sent; wait for the run goroutine to process it.
		// Also select on rs.done in case the run goroutine exits without
		// reading the interrupt (e.g., it was between streamLoop and finish).
		select {
		case err := <-req.result:
			return err
		case <-rs.done:
			return fmt.Errorf("stream closed before interrupt was processed")
		}
	case <-rs.done:
		return fmt.Errorf("stream already closed")
	}
}

// InterruptWithText is a convenience method for interrupting with a text message.
// Uses InterruptSaveMarked behavior by default.
func (rs *RunStream) InterruptWithText(text string) error {
	return rs.Interrupt(types.Message{
		Role: "user",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: text},
		},
	}, InterruptSaveMarked)
}

// runVoiceStreamer batches text deltas and forwards synthesized audio as run events.
type runVoiceStreamer struct {
	ttsCtx      *tts.StreamingContext
	sendEvents  func(RunStreamEvent)
	format      string
	transcript  strings.Builder
	audioBuffer bytes.Buffer

	buffer     strings.Builder
	bufferMu   sync.Mutex
	flushTimer *time.Timer
	done       chan struct{}
	wg         sync.WaitGroup
	closeOnce  sync.Once

	maxChars     int
	maxDelay     time.Duration
	sentenceEnds string

	errMu sync.Mutex
	err   error
}

func newRunVoiceStreamer(ttsCtx *tts.StreamingContext, format string, sendEvents func(RunStreamEvent)) *runVoiceStreamer {
	vs := &runVoiceStreamer{
		ttsCtx:       ttsCtx,
		sendEvents:   sendEvents,
		format:       normalizeAudioFormat(format),
		done:         make(chan struct{}),
		maxChars:     80,
		maxDelay:     150 * time.Millisecond,
		sentenceEnds: ".!?",
	}

	vs.wg.Add(1)
	go vs.forwardAudio()
	return vs
}

func (vs *runVoiceStreamer) AddText(text string) error {
	if text == "" {
		return nil
	}
	vs.bufferMu.Lock()
	defer vs.bufferMu.Unlock()

	if err := vs.Err(); err != nil {
		return err
	}

	vs.buffer.WriteString(text)
	vs.transcript.WriteString(text)
	content := vs.buffer.String()

	shouldSend := false
	if len(content) >= vs.maxChars {
		shouldSend = true
	}
	if strings.ContainsAny(text, vs.sentenceEnds) {
		shouldSend = true
	}

	if shouldSend {
		return vs.sendBufferLocked(false)
	}
	vs.resetTimerLocked()
	return nil
}

func (vs *runVoiceStreamer) sendBufferLocked(isFinal bool) error {
	content := strings.TrimSpace(vs.buffer.String())
	if content == "" && !isFinal {
		return nil
	}

	vs.buffer.Reset()
	if vs.flushTimer != nil {
		vs.flushTimer.Stop()
		vs.flushTimer = nil
	}

	var err error
	if content != "" {
		err = vs.ttsCtx.SendText(content, isFinal)
	} else {
		err = vs.ttsCtx.Flush()
	}
	if err != nil {
		vs.setErr(fmt.Errorf("voice output stream send failed: %w", err))
		return vs.Err()
	}
	return nil
}

func (vs *runVoiceStreamer) resetTimerLocked() {
	if vs.flushTimer != nil {
		vs.flushTimer.Stop()
	}
	vs.flushTimer = time.AfterFunc(vs.maxDelay, func() {
		vs.bufferMu.Lock()
		defer vs.bufferMu.Unlock()
		_ = vs.sendBufferLocked(false)
	})
}

func (vs *runVoiceStreamer) Flush() error {
	vs.bufferMu.Lock()
	defer vs.bufferMu.Unlock()
	return vs.sendBufferLocked(true)
}

func (vs *runVoiceStreamer) Close() error {
	vs.closeOnce.Do(func() {
		if vs.flushTimer != nil {
			vs.flushTimer.Stop()
		}

		close(vs.done)

		if err := vs.ttsCtx.Close(); err != nil {
			vs.setErr(err)
		}
		vs.wg.Wait()
	})
	return vs.Err()
}

func (vs *runVoiceStreamer) Err() error {
	vs.errMu.Lock()
	defer vs.errMu.Unlock()
	return vs.err
}

func (vs *runVoiceStreamer) setErr(err error) {
	if err == nil {
		return
	}
	vs.errMu.Lock()
	defer vs.errMu.Unlock()
	if vs.err == nil {
		vs.err = err
	}
}

func (vs *runVoiceStreamer) forwardAudio() {
	defer vs.wg.Done()

	for chunk := range vs.ttsCtx.Audio() {
		vs.audioBuffer.Write(chunk)
		vs.sendEvents(AudioChunkEvent{
			Data:   chunk,
			Format: vs.format,
		})
	}
	if err := vs.ttsCtx.Err(); err != nil {
		vs.setErr(fmt.Errorf("voice output stream failed: %w", err))
	}
}

func (vs *runVoiceStreamer) AudioBytes() []byte {
	out := make([]byte, vs.audioBuffer.Len())
	copy(out, vs.audioBuffer.Bytes())
	return out
}

func (vs *runVoiceStreamer) Transcript() string {
	return strings.TrimSpace(vs.transcript.String())
}

func hasSpokenText(text string) bool {
	text = strings.TrimSpace(text)
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// runStreamLoop executes the streaming tool loop.
func (s *MessagesService) runStreamLoop(ctx context.Context, req *MessageRequest, cfg *runConfig) *RunStream {
	var prepErr error
	var userTranscript string
	proxyMode := s.client != nil && s.client.isProxyMode()

	workingReq := req
	if !proxyMode && req != nil && req.Voice != nil && req.Voice.Input != nil {
		workingReq, userTranscript, prepErr = s.preprocessVoiceInput(ctx, req)
	}
	if !proxyMode && prepErr == nil && req != nil && req.Voice != nil && req.Voice.Output != nil {
		prepErr = s.requireVoicePipeline()
	}

	// Copy messages to avoid mutating the original
	var initialMessages []types.Message
	if workingReq != nil {
		initialMessages = make([]types.Message, len(workingReq.Messages))
		copy(initialMessages, workingReq.Messages)
	}

	rs := &RunStream{
		messages:  initialMessages,
		events:    make(chan RunStreamEvent, 100),
		interrupt: make(chan interruptRequest, 1),
		done:      make(chan struct{}),
	}

	if prepErr != nil {
		rs.err = prepErr
		rs.result = &RunResult{
			Steps:      make([]RunStep, 0),
			StopReason: RunStopError,
			Messages:   initialMessages,
		}
		rs.finish()
		return rs
	}

	runCtx, cancel := context.WithCancel(ctx)
	rs.mu.Lock()
	rs.cancel = cancel
	rs.mu.Unlock()

	go func() {
		defer cancel()
		rs.run(runCtx, s, workingReq, cfg, userTranscript, proxyMode)
	}()
	return rs
}

func (rs *RunStream) run(ctx context.Context, svc *MessagesService, req *MessageRequest, cfg *runConfig, userTranscript string, proxyMode bool) {
	defer rs.finish()

	result := &RunResult{
		Steps: make([]RunStep, 0),
	}
	if req == nil {
		result.StopReason = RunStopError
		rs.result = result
		rs.err = fmt.Errorf("req must not be nil")
		return
	}

	// Apply timeout
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	voiceEnabled := !proxyMode && req.Voice != nil && req.Voice.Output != nil
	var voiceStream *runVoiceStreamer
	newVoiceStream := func() (*runVoiceStreamer, error) {
		ttsCtx, err := svc.client.voicePipeline.NewStreamingTTSContext(ctx, req.Voice)
		if err != nil {
			return nil, fmt.Errorf("initialize voice output stream: %w", err)
		}
		return newRunVoiceStreamer(ttsCtx, normalizeStreamingAudioFormat(req.Voice.Output.Format), rs.send), nil
	}
	if voiceEnabled {
		var err error
		voiceStream, err = newVoiceStream()
		if err != nil {
			result.StopReason = RunStopError
			rs.result = result
			rs.err = err
			return
		}
	}

	closeVoice := func() {
		if voiceStream != nil {
			_ = voiceStream.Close()
			voiceStream = nil
		}
	}
	defer closeVoice()

	appendTranscript := func(segment string) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return
		}
		if userTranscript == "" {
			userTranscript = segment
			return
		}
		userTranscript += "\n" + segment
	}

	finalizeTerminalResponse := func(resp *Response) error {
		if resp == nil || resp.MessageResponse == nil {
			return nil
		}
		if userTranscript != "" {
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			resp.Metadata["user_transcript"] = userTranscript
		}
		if !voiceEnabled || voiceStream == nil {
			return nil
		}
		if !hasSpokenText(voiceStream.Transcript()) {
			fallbackText := strings.TrimSpace(resp.TextContent())
			if hasSpokenText(fallbackText) {
				if err := voiceStream.AddText(fallbackText); err != nil {
					return err
				}
			}
		}
		if !hasSpokenText(voiceStream.Transcript()) {
			if err := voiceStream.Close(); err != nil {
				return err
			}
			voiceStream = nil
			return nil
		}
		if err := voiceStream.Flush(); err != nil {
			return err
		}
		if err := voiceStream.Close(); err != nil {
			return err
		}

		audio := voiceStream.AudioBytes()
		if len(audio) == 0 {
			voiceStream = nil
			return nil
		}
		transcript := voiceStream.Transcript()
		if transcript == "" {
			transcript = strings.TrimSpace(resp.TextContent())
		}
		resp.Content = append(resp.Content, types.AudioBlock{
			Type: "audio",
			Source: types.AudioSource{
				Type:      "base64",
				MediaType: mediaTypeForAudioFormat(normalizeStreamingAudioFormat(req.Voice.Output.Format)),
				Data:      base64.StdEncoding.EncodeToString(audio),
			},
			Transcript: &transcript,
		})
		voiceStream = nil
		return nil
	}

	appendHistoryDelta := func(delta []types.Message) {
		if len(delta) == 0 {
			return
		}
		expectedLen := 0
		rs.mu.Lock()
		expectedLen = len(rs.messages)
		rs.messages = append(rs.messages, delta...)
		rs.mu.Unlock()
		rs.send(HistoryDeltaEvent{ExpectedLen: expectedLen, Append: delta})
	}

	snapshotHistory := func() []types.Message {
		rs.mu.RLock()
		out := make([]types.Message, len(rs.messages))
		copy(out, rs.messages)
		rs.mu.RUnlock()
		return out
	}

	stepIndex := 0

	for {
		select {
		case <-ctx.Done():
			if stopReason, ok := stopReasonFromContextErr(ctx.Err()); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotHistory()
			rs.result = result
			rs.err = ctx.Err()
			return
		default:
		}

		// Check limits
		if cfg.maxTurns > 0 && result.TurnCount >= cfg.maxTurns {
			result.StopReason = RunStopMaxTurns
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		if cfg.maxTokens > 0 && result.Usage.TotalTokens >= cfg.maxTokens {
			result.StopReason = RunStopMaxTokens
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		// Signal step start
		rs.send(StepStartEvent{Index: stepIndex})

		// Snapshot internal history for this turn boundary.
		rs.mu.RLock()
		historySnapshot := make([]types.Message, len(rs.messages))
		copy(historySnapshot, rs.messages)
		turnReq := &types.MessageRequest{
			Model:         req.Model,
			Messages:      historySnapshot,
			MaxTokens:     req.MaxTokens,
			System:        req.System,
			Temperature:   req.Temperature,
			TopP:          req.TopP,
			TopK:          req.TopK,
			StopSequences: req.StopSequences,
			Tools:         mergeTools(req.Tools, cfg.extraTools),
			ToolChoice:    req.ToolChoice,
			Stream:        true, // Always stream in RunStream
			OutputFormat:  req.OutputFormat,
			Output:        req.Output,
			Extensions:    req.Extensions,
			Metadata:      req.Metadata,
		}
		rs.mu.RUnlock()

		if cfg.buildTurnMsgs != nil {
			turnReq.Messages = cfg.buildTurnMsgs(TurnInfo{
				TurnIndex: result.TurnCount,
				History:   historySnapshot,
			})
		}

		if cfg.beforeCall != nil {
			cfg.beforeCall(turnReq)
		}

		stepStart := time.Now()

		// Stream this turn
		stream, err := svc.streamTurn(ctx, turnReq)
		if err != nil {
			if stopReason, ok := stopReasonFromContextErr(err); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotHistory()
			rs.result = result
			rs.err = err
			return
		}

		// Store current stream for interrupt access
		rs.mu.Lock()
		rs.currentStream = stream
		rs.partialContent.Reset()
		rs.mu.Unlock()

		// Process stream events with cancel support using select
	streamLoop:
		for {
			select {
			case <-ctx.Done():
				stream.Close()
				if stopReason, ok := stopReasonFromContextErr(ctx.Err()); ok {
					result.StopReason = stopReason
				} else {
					result.StopReason = RunStopError
				}
				result.Messages = snapshotHistory()
				rs.result = result
				rs.err = ctx.Err()
				return

			case intReq := <-rs.interrupt:
				// Stop current stream immediately
				stream.Close()

				rs.mu.Lock()
				partialText := rs.partialContent.String()
				rs.currentStream = nil
				rs.partialContent.Reset()
				rs.mu.Unlock()

				// Check if this is Cancel (empty message) or Interrupt (has message)
				isCancel := intReq.message.Role == "" && intReq.message.Content == nil

				// Cut current voice output immediately on any interrupt/cancel.
				if voiceStream != nil {
					_ = voiceStream.Close()
					voiceStream = nil
				}

				if !isCancel {
					var delta []types.Message

					// Handle partial response based on behavior
					switch intReq.behavior {
					case InterruptSavePartial:
						if partialText != "" {
							delta = append(delta, types.Message{
								Role: "assistant",
								Content: []types.ContentBlock{
									types.TextBlock{Type: "text", Text: partialText},
								},
							})
						}
					case InterruptSaveMarked:
						if partialText != "" {
							delta = append(delta, types.Message{
								Role: "assistant",
								Content: []types.ContentBlock{
									types.TextBlock{Type: "text", Text: partialText + " [interrupted]"},
								},
							})
						}
					case InterruptDiscard:
						// Don't save partial response
					}

					injectedMessage := intReq.message
					if !proxyMode && req.Voice != nil && req.Voice.Input != nil {
						processed, transcript, err := svc.client.voicePipeline.ProcessInputAudio(ctx, []types.Message{injectedMessage}, req.Voice)
						if err != nil {
							intReq.result <- fmt.Errorf("transcribe interrupt audio: %w", err)
							result.StopReason = RunStopError
							result.Messages = snapshotHistory()
							rs.result = result
							rs.err = err
							return
						}
						if len(processed) > 0 {
							injectedMessage = processed[0]
						}
						appendTranscript(transcript)
					}

					// Inject the new message
					delta = append(delta, injectedMessage)
					appendHistoryDelta(delta)

					// Start a fresh voice stream for subsequent turns after interruption.
					if voiceEnabled {
						nextVoiceStream, err := newVoiceStream()
						if err != nil {
							intReq.result <- err
							result.StopReason = RunStopError
							result.Messages = snapshotHistory()
							rs.result = result
							rs.err = err
							return
						}
						voiceStream = nextVoiceStream
					}
				}

				// Notify caller
				intReq.result <- nil

				if isCancel {
					// Cancel: Terminate the run loop
					rs.send(InterruptedEvent{PartialText: partialText, Behavior: InterruptDiscard})
					result.StopReason = RunStopCancelled
					result.Messages = snapshotHistory()
					rs.result = result
					rs.send(RunCompleteEvent{Result: result})
					return
				}

				// Interrupt: Continue to next turn
				rs.send(InterruptedEvent{PartialText: partialText, Behavior: intReq.behavior})
				break streamLoop

			case event, ok := <-stream.Events():
				if !ok {
					// Stream ended normally
					break streamLoop
				}

				rs.send(StreamEventWrapper{Event: event})

				if audioEvent, ok := event.(types.AudioChunkEvent); ok {
					audioBytes, decodeErr := base64.StdEncoding.DecodeString(audioEvent.Audio)
					if decodeErr != nil {
						result.StopReason = RunStopError
						result.Messages = snapshotHistory()
						rs.result = result
						rs.err = fmt.Errorf("decode audio chunk: %w", decodeErr)
						stream.Close()
						return
					}
					rs.send(AudioChunkEvent{Data: audioBytes, Format: audioEvent.Format})
				}

				// Track partial text content for potential interruption
				if deltaEvent, ok := event.(types.ContentBlockDeltaEvent); ok {
					if textDelta, ok := deltaEvent.Delta.(types.TextDelta); ok {
						rs.mu.Lock()
						rs.partialContent.WriteString(textDelta.Text)
						rs.mu.Unlock()
						if voiceStream != nil {
							if err := voiceStream.AddText(textDelta.Text); err != nil {
								stream.Close()
								result.StopReason = RunStopError
								result.Messages = snapshotHistory()
								rs.result = result
								rs.err = err
								return
							}
						}
					}
				}

				// Forward provider-native web search results as tool result lifecycle events.
				if startEvent, ok := event.(types.ContentBlockStartEvent); ok {
					switch block := startEvent.ContentBlock.(type) {
					case types.WebSearchToolResultBlock:
						var resultContent []types.ContentBlock
						if len(block.Content) > 0 {
							var summary strings.Builder
							summary.WriteString(fmt.Sprintf("Found %d results", len(block.Content)))
							resultContent = []types.ContentBlock{
								types.TextBlock{Type: "text", Text: summary.String()},
							}
						}
						rs.send(ToolResultEvent{
							ID:      block.ToolUseID,
							Name:    "web_search",
							Content: resultContent,
						})
					}
				}
			}
		}

		// Clear current stream reference
		rs.mu.Lock()
		rs.currentStream = nil
		rs.mu.Unlock()

		// EOF is normal stream termination, not an error
		if streamErr := stream.Err(); streamErr != nil && streamErr != io.EOF {
			if stopReason, ok := stopReasonFromContextErr(streamErr); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotHistory()
			rs.result = result
			rs.err = streamErr
			stream.Close()
			return
		}
		if voiceStream != nil && voiceStream.Err() != nil {
			voiceErr := voiceStream.Err()
			if stopReason, ok := stopReasonFromContextErr(voiceErr); ok {
				result.StopReason = stopReason
			} else {
				result.StopReason = RunStopError
			}
			result.Messages = snapshotHistory()
			rs.result = result
			rs.err = voiceErr
			stream.Close()
			return
		}

		coreResp := stream.Response()
		stream.Close()

		resp := &Response{MessageResponse: coreResp}
		stepDuration := time.Since(stepStart).Milliseconds()

		if cfg.afterResponse != nil {
			cfg.afterResponse(resp)
		}

		result.Usage = result.Usage.Add(resp.Usage)
		result.TurnCount++

		step := RunStep{
			Index:      stepIndex,
			Response:   resp,
			DurationMs: stepDuration,
		}

		// Check custom stop
		if cfg.stopWhen != nil && cfg.stopWhen(resp) {
			if err := finalizeTerminalResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotHistory()
				rs.result = result
				rs.err = err
				return
			}
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopCustom
			rs.send(StepCompleteEvent{Index: stepIndex, Response: resp})
			appendHistoryDelta([]types.Message{
				{Role: "assistant", Content: resp.Content},
			})
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		// Check if done
		if resp.StopReason != types.StopReasonToolUse {
			if err := finalizeTerminalResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotHistory()
				rs.result = result
				rs.err = err
				return
			}
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopEndTurn
			rs.send(StepCompleteEvent{Index: stepIndex, Response: resp})
			appendHistoryDelta([]types.Message{
				{Role: "assistant", Content: resp.Content},
			})
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		// Process tool calls
		toolUses := resp.ToolUses()
		if len(toolUses) == 0 {
			if err := finalizeTerminalResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotHistory()
				rs.result = result
				rs.err = err
				return
			}
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopEndTurn
			rs.send(StepCompleteEvent{Index: stepIndex, Response: resp})
			appendHistoryDelta([]types.Message{
				{Role: "assistant", Content: resp.Content},
			})
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		// Check tool call limit
		if cfg.maxToolCalls > 0 && result.ToolCallCount+len(toolUses) > cfg.maxToolCalls {
			if err := finalizeTerminalResponse(resp); err != nil {
				result.StopReason = RunStopError
				result.Messages = snapshotHistory()
				rs.result = result
				rs.err = err
				return
			}
			result.Response = resp
			result.Steps = append(result.Steps, step)
			result.StopReason = RunStopMaxToolCalls
			rs.send(StepCompleteEvent{Index: stepIndex, Response: resp})
			appendHistoryDelta([]types.Message{
				{Role: "assistant", Content: resp.Content},
			})
			result.Messages = snapshotHistory()
			rs.result = result
			rs.send(RunCompleteEvent{Result: result})
			return
		}

		// Execute tools with events
		toolResults := make([]ToolExecutionResult, len(toolUses))
		for i, tu := range toolUses {
			rs.send(ToolCallStartEvent{ID: tu.ID, Name: tu.Name, Input: tu.Input})

			tr := svc.executeToolCall(ctx, tu, cfg)
			toolResults[i] = tr

			rs.send(ToolResultEvent{ID: tu.ID, Name: tu.Name, Content: tr.Content, Error: tr.Error})
		}

		step.ToolCalls = make([]ToolCall, len(toolUses))
		for i, tu := range toolUses {
			step.ToolCalls[i] = ToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input}
		}
		step.ToolResults = toolResults
		result.Steps = append(result.Steps, step)
		result.ToolCallCount += len(toolUses)

		rs.send(StepCompleteEvent{Index: stepIndex, Response: resp})

		toolResultBlocks := make([]types.ContentBlock, len(toolResults))
		for i, tr := range toolResults {
			toolResultBlocks[i] = types.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: tr.ToolUseID,
				Content:   tr.Content,
				IsError:   tr.Error != nil,
			}
		}
		appendHistoryDelta([]types.Message{
			{Role: "assistant", Content: resp.Content},
			{Role: "user", Content: toolResultBlocks},
		})

		stepIndex++
	}
}

func (rs *RunStream) send(event RunStreamEvent) {
	if rs.closed.Load() {
		return
	}
	select {
	case <-rs.done:
		return
	default:
	}
	select {
	case rs.events <- event:
	case <-rs.done:
	}
}

func (rs *RunStream) finish() {
	rs.finishOnce.Do(func() {
		// Close done before events so senders can cheaply bail out before touching events.
		close(rs.done)
		close(rs.events)
	})
}

// Events returns the channel of run stream events.
func (rs *RunStream) Events() <-chan RunStreamEvent {
	return rs.events
}

// Result returns the final result after the stream ends.
func (rs *RunStream) Result() *RunResult {
	<-rs.done
	return rs.result
}

// Err returns any error that occurred.
func (rs *RunStream) Err() error {
	<-rs.done
	return rs.err
}

// Close stops the run stream.
func (rs *RunStream) Close() error {
	if rs.closed.Swap(true) {
		rs.mu.RLock()
		cancel := rs.cancel
		rs.mu.RUnlock()
		if cancel != nil {
			<-rs.done
		}
		return nil
	}

	rs.mu.RLock()
	cancel := rs.cancel
	stream := rs.currentStream
	rs.mu.RUnlock()

	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.Close()
	}

	// Synthetic test streams may not have an owner goroutine; finish directly in that case.
	if cancel == nil {
		rs.finish()
		return nil
	}

	<-rs.done
	return nil
}
