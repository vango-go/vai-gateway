package gem

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"iter"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

type streamToolState struct {
	key       string
	id        string
	name      string
	blockIdx  int
	closed    bool
	emitted   bool
	mode      string // stream_root_string | buffer_full_object
	rootKey   string
	rootStart bool
	rootText  strings.Builder

	argsObject   map[string]any
	latestArgs   map[string]any
	thoughtSig   []byte
	incomplete   bool
	warningCount int
}

type streamPrimedResult struct {
	has  bool
	resp *genai.GenerateContentResponse
	err  error
	ok   bool
}

func (p streamPrimedResult) hasErr() bool {
	return p.has && p.err != nil
}

type eventStream struct {
	ctx    context.Context
	cancel context.CancelFunc

	provider *Provider
	model    string
	streamID uint64
	build    *requestBuild

	nextFn func() (*genai.GenerateContentResponse, error, bool)
	stopFn func()
	primed streamPrimedResult

	queue []types.StreamEvent

	started bool
	done    bool
	err     error

	nextBlockIdx int
	toolSeq      int

	textByPart     map[int]int
	thinkingByPart map[int]int
	mediaByPart    map[int]int
	openText       map[int]struct{}
	openThinking   map[int]struct{}

	toolByKey map[string]*streamToolState

	hadToolUse       bool
	latestUsage      types.Usage
	latestFinish     genai.FinishReason
	latestResponseID string

	closeOnce sync.Once
}

func newEventStream(
	ctx context.Context,
	provider *Provider,
	model string,
	streamID uint64,
	seq iter.Seq2[*genai.GenerateContentResponse, error],
	build *requestBuild,
) EventStream {
	nextFn, stopFn := iter.Pull2(seq)
	return newEventStreamFromPull(ctx, provider, model, streamID, nextFn, stopFn, build, streamPrimedResult{})
}

func newEventStreamFromPull(
	ctx context.Context,
	provider *Provider,
	model string,
	streamID uint64,
	nextFn func() (*genai.GenerateContentResponse, error, bool),
	stopFn func(),
	build *requestBuild,
	primed streamPrimedResult,
) EventStream {
	streamCtx, cancel := context.WithCancel(ctx)
	return &eventStream{
		ctx:            streamCtx,
		cancel:         cancel,
		provider:       provider,
		model:          model,
		streamID:       streamID,
		build:          build,
		nextFn:         nextFn,
		stopFn:         stopFn,
		primed:         primed,
		queue:          make([]types.StreamEvent, 0, 8),
		textByPart:     map[int]int{},
		thinkingByPart: map[int]int{},
		mediaByPart:    map[int]int{},
		openText:       map[int]struct{}{},
		openThinking:   map[int]struct{}{},
		toolByKey:      map[string]*streamToolState{},
	}
}

func (s *eventStream) Next() (types.StreamEvent, error) {
	if len(s.queue) > 0 {
		e := s.queue[0]
		s.queue = s.queue[1:]
		return e, nil
	}
	if s.done {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}

	if !s.started {
		s.started = true
		s.enqueueMessageStart()
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue = s.queue[1:]
			return e, nil
		}
	}

	for len(s.queue) == 0 && !s.done {
		if err := s.ctx.Err(); err != nil {
			s.done = true
			s.err = err
			break
		}

		var (
			resp    *genai.GenerateContentResponse
			iterErr error
			ok      bool
		)
		if s.primed.has {
			resp = s.primed.resp
			iterErr = s.primed.err
			ok = s.primed.ok
			s.primed = streamPrimedResult{}
		} else {
			resp, iterErr, ok = s.nextFn()
		}
		if !ok {
			s.finalize(nil)
			break
		}
		if iterErr != nil {
			mapped := mapError(iterErr)
			s.queue = append(s.queue, types.ErrorEvent{
				Type: "error",
				Error: types.Error{
					Type:    string(coreErrorType(mapped)),
					Message: mapped.Error(),
				},
			})
			s.done = true
			s.err = mapped
			break
		}
		s.consumeResponse(resp)
	}

	if len(s.queue) > 0 {
		e := s.queue[0]
		s.queue = s.queue[1:]
		return e, nil
	}
	if s.done {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	return nil, io.EOF
}

func (s *eventStream) Close() error {
	s.closeOnce.Do(func() {
		s.stopFn()
		s.cancel()
		s.done = true
		if s.err == nil {
			s.err = io.EOF
		}
	})
	return nil
}

func (s *eventStream) enqueueMessageStart() {
	msg := types.MessageResponse{
		ID:      s.latestResponseID,
		Type:    "message",
		Role:    "assistant",
		Model:   s.provider.prefixedModel(s.model),
		Content: []types.ContentBlock{},
		Usage:   types.Usage{},
	}
	if len(s.build.warnings) > 0 {
		msg.Metadata = map[string]any{"gem": map[string]any{"warnings": append([]string(nil), s.build.warnings...)}}
	}
	s.queue = append(s.queue, types.MessageStartEvent{Type: "message_start", Message: msg})
}

func (s *eventStream) consumeResponse(resp *genai.GenerateContentResponse) {
	if resp == nil {
		return
	}
	if strings.TrimSpace(resp.ResponseID) != "" {
		s.latestResponseID = strings.TrimSpace(resp.ResponseID)
	}
	s.latestUsage = mapUsage(resp.UsageMetadata)
	candidate := firstCandidate(resp)
	if candidate == nil {
		return
	}
	s.latestFinish = candidate.FinishReason
	if candidate.Content == nil {
		return
	}
	for partIdx, part := range candidate.Content.Parts {
		if part == nil {
			continue
		}
		switch {
		case part.FunctionCall != nil:
			s.hadToolUse = true
			s.handleFunctionCall(partIdx, part)
		case part.Text != "" && part.Thought:
			s.handleThinkingText(partIdx, part.Text)
		case part.Text != "":
			s.handleText(partIdx, part.Text)
		case part.InlineData != nil || part.FileData != nil:
			s.handleStaticMedia(partIdx, part)
		}
	}
}

func (s *eventStream) handleText(partIdx int, delta string) {
	idx, ok := s.textByPart[partIdx]
	if !ok {
		idx = s.allocBlockIndex()
		s.textByPart[partIdx] = idx
		s.openText[idx] = struct{}{}
		s.queue = append(s.queue, types.ContentBlockStartEvent{
			Type:         "content_block_start",
			Index:        idx,
			ContentBlock: types.TextBlock{Type: "text", Text: ""},
		})
	}
	if delta == "" {
		return
	}
	s.queue = append(s.queue, types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: idx,
		Delta: types.TextDelta{Type: "text_delta", Text: delta},
	})
}

func (s *eventStream) handleThinkingText(partIdx int, delta string) {
	idx, ok := s.thinkingByPart[partIdx]
	if !ok {
		idx = s.allocBlockIndex()
		s.thinkingByPart[partIdx] = idx
		s.openThinking[idx] = struct{}{}
		s.queue = append(s.queue, types.ContentBlockStartEvent{
			Type:         "content_block_start",
			Index:        idx,
			ContentBlock: types.ThinkingBlock{Type: "thinking", Thinking: ""},
		})
	}
	if delta == "" {
		return
	}
	s.queue = append(s.queue, types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: idx,
		Delta: types.ThinkingDelta{Type: "thinking_delta", Thinking: delta},
	})
}

func (s *eventStream) handleStaticMedia(partIdx int, part *genai.Part) {
	if _, exists := s.mediaByPart[partIdx]; exists {
		return
	}
	block, _ := staticPartToBlock(part)
	if block == nil {
		return
	}
	idx := s.allocBlockIndex()
	s.mediaByPart[partIdx] = idx
	s.queue = append(s.queue,
		types.ContentBlockStartEvent{Type: "content_block_start", Index: idx, ContentBlock: block},
		types.ContentBlockStopEvent{Type: "content_block_stop", Index: idx},
	)
}

func staticPartToBlock(part *genai.Part) (types.ContentBlock, string) {
	if part.InlineData != nil {
		return inlinePartToContentBlock(part)
	}
	if part.FileData != nil {
		return filePartToContentBlock(part)
	}
	return nil, ""
}

func (s *eventStream) handleFunctionCall(partIdx int, part *genai.Part) {
	call := part.FunctionCall
	key := strings.TrimSpace(call.ID)
	if key == "" {
		key = "part_" + strconv.Itoa(partIdx)
	}
	state, ok := s.toolByKey[key]
	if !ok {
		s.toolSeq++
		id := strings.TrimSpace(call.ID)
		if id == "" {
			id = "gem_tool_" + strconv.FormatUint(s.streamID, 10) + "_" + strconv.Itoa(s.toolSeq)
		}
		state = &streamToolState{
			key:        key,
			id:         id,
			name:       call.Name,
			blockIdx:   s.allocBlockIndex(),
			mode:       "",
			argsObject: map[string]any{},
		}
		s.toolByKey[key] = state
		s.queue = append(s.queue, types.ContentBlockStartEvent{
			Type:  "content_block_start",
			Index: state.blockIdx,
			ContentBlock: types.ToolUseBlock{
				Type:  "tool_use",
				ID:    state.id,
				Name:  call.Name,
				Input: map[string]any{},
			},
		})
	}
	if strings.TrimSpace(call.Name) != "" {
		state.name = call.Name
	}
	if call.Args != nil {
		state.latestArgs = copyMap(call.Args)
	}
	if len(part.ThoughtSignature) > 0 {
		state.thoughtSig = append([]byte(nil), part.ThoughtSignature...)
	}

	if s.provider.mode == backendModeVertex && len(call.PartialArgs) > 0 {
		for _, pa := range call.PartialArgs {
			s.handleVertexPartialArg(state, pa)
		}
	} else if call.Args != nil && !state.emitted {
		s.emitToolArgsDelta(state, attachThoughtSignature(call.Args, state.thoughtSig))
		state.emitted = true
	}

	if call.WillContinue != nil && *call.WillContinue {
		return
	}
	s.closeToolState(state)
}

func (s *eventStream) handleVertexPartialArg(state *streamToolState, pa *genai.PartialArg) {
	if pa == nil || state.closed {
		return
	}
	value, isString := partialArgValue(pa)
	rootKey, simpleRoot := rootJSONPathKey(pa.JsonPath)

	if state.mode == "" {
		if simpleRoot && isString && isRootStreamingKey(rootKey) {
			state.mode = "stream_root_string"
			state.rootKey = rootKey
		} else {
			state.mode = "buffer_full_object"
		}
	}

	if state.mode == "stream_root_string" {
		if !(simpleRoot && isString && rootKey == state.rootKey) {
			state.mode = "buffer_full_object"
			if state.rootText.Len() > 0 {
				state.argsObject[state.rootKey] = state.rootText.String()
			}
		} else {
			if !state.rootStart {
				state.rootStart = true
				prefix := `{"` + state.rootKey + `":"`
				s.queue = append(s.queue, types.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: state.blockIdx,
					Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: prefix},
				})
			}
			fragment := value.(string)
			state.rootText.WriteString(fragment)
			esc := escapeJSONStringFragment(fragment)
			if esc != "" {
				s.queue = append(s.queue, types.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: state.blockIdx,
					Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: esc},
				})
			}
			state.emitted = true
			return
		}
	}

	if state.mode == "buffer_full_object" {
		if err := applyPartialArgPath(state.argsObject, pa.JsonPath, value); err != nil {
			state.incomplete = true
			state.warningCount++
		}
	}
}

func (s *eventStream) closeToolState(state *streamToolState) {
	if state == nil || state.closed {
		return
	}
	defer func() { state.closed = true }()

	if state.mode == "stream_root_string" && state.rootStart {
		suffix := `"}`
		if len(state.thoughtSig) > 0 {
			suffix = `","` + thoughtSignatureKey + `":"` + base64.StdEncoding.EncodeToString(state.thoughtSig) + `"}`
		}
		s.queue = append(s.queue, types.ContentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: state.blockIdx,
			Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: suffix},
		})
		state.emitted = true
	} else if !state.emitted {
		finalArgs := state.finalArgs()
		s.emitToolArgsDelta(state, finalArgs)
		state.emitted = true
	}

	s.queue = append(s.queue, types.ContentBlockStopEvent{Type: "content_block_stop", Index: state.blockIdx})
}

func (s *eventStream) emitToolArgsDelta(state *streamToolState, args map[string]any) {
	if state == nil {
		return
	}
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		b = []byte("{}")
	}
	s.queue = append(s.queue, types.ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: state.blockIdx,
		Delta: types.InputJSONDelta{Type: "input_json_delta", PartialJSON: string(b)},
	})
}

func (s *streamToolState) finalArgs() map[string]any {
	out := map[string]any{}
	if len(s.latestArgs) > 0 {
		out = copyMap(s.latestArgs)
	} else if len(s.argsObject) > 0 {
		out = copyMap(s.argsObject)
	}
	if out == nil {
		out = map[string]any{}
	}
	if len(s.thoughtSig) > 0 {
		out[thoughtSignatureKey] = base64.StdEncoding.EncodeToString(s.thoughtSig)
	}
	if s.incomplete && len(out) == 0 {
		out = map[string]any{}
	}
	return out
}

func partialArgValue(pa *genai.PartialArg) (any, bool) {
	if pa == nil {
		return "", true
	}
	if pa.BoolValue != nil {
		return *pa.BoolValue, false
	}
	if pa.NumberValue != nil {
		return *pa.NumberValue, false
	}
	if pa.NULLValue != "" {
		return nil, false
	}
	return pa.StringValue, true
}

func rootJSONPathKey(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "$.") {
		key := strings.TrimPrefix(path, "$.")
		if strings.ContainsAny(key, ".[\"") || key == "" {
			return "", false
		}
		return key, true
	}
	if strings.HasPrefix(path, "$['") && strings.HasSuffix(path, "']") {
		key := strings.TrimSuffix(strings.TrimPrefix(path, "$['"), "']")
		if key == "" || strings.Contains(key, "'") {
			return "", false
		}
		return key, true
	}
	return "", false
}

func isRootStreamingKey(key string) bool {
	switch key {
	case "text", "content", "message":
		return true
	default:
		return false
	}
}

func escapeJSONStringFragment(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}

func attachThoughtSignature(args map[string]any, sig []byte) map[string]any {
	out := copyMap(args)
	if out == nil {
		out = map[string]any{}
	}
	if len(sig) > 0 {
		out[thoughtSignatureKey] = base64.StdEncoding.EncodeToString(sig)
	}
	return out
}

func (s *eventStream) finalize(finalErr error) {
	for _, state := range s.toolByKey {
		s.closeToolState(state)
	}
	for idx := range s.openText {
		s.queue = append(s.queue, types.ContentBlockStopEvent{Type: "content_block_stop", Index: idx})
	}
	for idx := range s.openThinking {
		s.queue = append(s.queue, types.ContentBlockStopEvent{Type: "content_block_stop", Index: idx})
	}

	stopReason := types.StopReasonEndTurn
	if s.hadToolUse {
		stopReason = types.StopReasonToolUse
	} else if s.latestFinish == genai.FinishReasonMaxTokens {
		stopReason = types.StopReasonMaxTokens
	}

	delta := types.MessageDeltaEvent{Type: "message_delta", Usage: s.latestUsage}
	delta.Delta.StopReason = stopReason
	s.queue = append(s.queue, delta, types.MessageStopEvent{Type: "message_stop"})

	s.done = true
	if finalErr != nil {
		s.err = finalErr
	}
}

func (s *eventStream) allocBlockIndex() int {
	idx := s.nextBlockIdx
	s.nextBlockIdx++
	return idx
}

func coreErrorType(err error) ErrorType {
	if ge, ok := err.(*Error); ok {
		return ge.Type
	}
	return ErrAPI
}
