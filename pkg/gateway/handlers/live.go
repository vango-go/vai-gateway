package handlers

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
	"github.com/vango-go/vai-lite/pkg/gateway/compat"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
	"github.com/vango-go/vai-lite/pkg/gateway/limits"
	"github.com/vango-go/vai-lite/pkg/gateway/mw"
	"github.com/vango-go/vai-lite/pkg/gateway/principal"
	"github.com/vango-go/vai-lite/pkg/gateway/ratelimit"
	"github.com/vango-go/vai-lite/pkg/gateway/runloop"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
	vai "github.com/vango-go/vai-lite/sdk"
)

const (
	liveInputFormat               = "pcm_s16le"
	liveInputSampleRateHz         = 16000
	liveOutputFormat              = "pcm_s16le"
	liveDefaultOutputSampleRateHz = 16000
	liveSilenceCommitMS           = 600
)

var liveTTSDrainTimeout = 10 * time.Second

const liveTalkToUserSystemInstruction = `You must use the talk_to_user tool for any user-facing speech.
When talking to the user, call talk_to_user with JSON arguments shaped like {"content":"..."}.
You may use other tools first, then talk_to_user to present results.
Do not duplicate the same spoken content as plain assistant text.`

type liveSessionState string

const (
	liveStateAwaitingStart liveSessionState = "awaiting_start"
	liveStateRunning       liveSessionState = "running"
	liveStateClosing       liveSessionState = "closing"
)

type liveInboundStartFrame struct {
	Type       string          `json:"type"`
	RunRequest json.RawMessage `json:"run_request"`
}

type liveInboundToolResultFrame struct {
	Type        string            `json:"type"`
	ExecutionID string            `json:"execution_id"`
	Content     []json.RawMessage `json:"content"`
	IsError     bool              `json:"is_error,omitempty"`
	Error       any               `json:"error,omitempty"`
}

type liveInboundStopFrame struct {
	Type string `json:"type"`
}

type liveToolResultPayload struct {
	Content []types.ContentBlock
	IsError bool
	Error   *types.Error
}

type liveToolMeta struct {
	id   string
	name string
}

type liveTalkTTS struct {
	stream     *voice.StreamingTTS
	audioDone  chan struct{}
	audioErrCh chan error
	started    bool
}

type liveSTTSession interface {
	SendAudio(data []byte) error
	Transcripts() <-chan stt.TranscriptDelta
	Close() error
}

type liveTTSSession interface {
	OnTextDelta(text string) error
	Flush() error
	Close() error
	Audio() <-chan []byte
	Err() error
}

var liveWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var newLiveVoicePipelineFunc = func(cartesiaKey string, httpClient *http.Client) *voice.Pipeline {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return voice.NewPipelineWithProviders(
		stt.NewCartesiaWithClient(cartesiaKey, httpClient),
		tts.NewCartesiaWithClient(cartesiaKey, httpClient),
	)
}

var newLiveSTTSessionFunc = func(ctx context.Context, pipeline *voice.Pipeline, model string) (liveSTTSession, error) {
	if pipeline == nil || pipeline.STTProvider() == nil {
		return nil, errors.New("stt provider is not configured")
	}
	stream, err := pipeline.STTProvider().NewStreamingSTT(ctx, stt.TranscribeOptions{
		Model:      model,
		Format:     liveInputFormat,
		SampleRate: liveInputSampleRateHz,
	})
	if err != nil {
		return nil, err
	}
	return stream, nil
}

var newLiveTTSSessionFunc = func(ctx context.Context, pipeline *voice.Pipeline, voiceCfg *types.VoiceConfig, ttsModel string) (liveTTSSession, error) {
	if pipeline == nil {
		return nil, errors.New("voice pipeline is not configured")
	}
	ttsCtx, err := pipeline.NewStreamingTTSContext(ctx, voiceCfg, ttsModel)
	if err != nil {
		return nil, err
	}
	return voice.NewStreamingTTS(ttsCtx, voice.StreamingTTSOptions{BufferAudio: false}), nil
}

// LiveHandler handles /v1/live websocket sessions.
type LiveHandler struct {
	Config     config.Config
	Upstreams  ProviderFactory
	HTTPClient *http.Client
	Logger     *slog.Logger
	Limiter    *ratelimit.Limiter
	Lifecycle  *lifecycle.Lifecycle
}

func (h LiveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID, _ := mw.RequestIDFrom(r.Context())
	if r.Method != http.MethodGet {
		writeCoreErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "method not allowed",
			Code:      "method_not_allowed",
			RequestID: reqID,
		}, http.StatusMethodNotAllowed)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeCoreErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "websocket upgrade required",
			Code:      "ws_upgrade_required",
			RequestID: reqID,
		}, http.StatusBadRequest)
		return
	}
	if h.Lifecycle != nil && h.Lifecycle.IsDraining() {
		writeCoreErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrOverloaded,
			Message:   "gateway is draining",
			Code:      "draining",
			RequestID: reqID,
		}, 529)
		return
	}

	var wsPermit *ratelimit.Permit
	if h.Limiter != nil && h.Config.WSMaxSessionsPerPrincipal > 0 {
		p := principal.Resolve(r, h.Config)
		dec := h.Limiter.AcquireWSSession(p.Key, time.Now())
		if !dec.Allowed {
			if dec.RetryAfter > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", dec.RetryAfter))
			}
			writeCoreErrorJSON(w, reqID, core.NewRateLimitError("too many websocket sessions", dec.RetryAfter), http.StatusTooManyRequests)
			return
		}
		wsPermit = dec.Permit
	}
	if wsPermit != nil {
		defer wsPermit.Release()
	}

	conn, err := liveWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sessionTimeout := h.Config.WSMaxSessionDuration
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if sessionTimeout > 0 {
		ctx, cancel = context.WithTimeout(r.Context(), sessionTimeout)
	} else {
		ctx, cancel = context.WithCancel(r.Context())
	}
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	startFrame, runReq, sessionCfg, err := h.readAndValidateLiveStart(ctx, r, conn)
	if err != nil {
		h.writeLiveFatalAndClose(conn, err)
		return
	}
	_ = startFrame

	session := &liveSession{
		ctx:           ctx,
		cancel:        cancel,
		logger:        h.Logger,
		reqID:         reqID,
		conn:          conn,
		sendCh:        make(chan any, 256),
		state:         liveStateRunning,
		toolWaiters:   make(map[string]chan liveToolResultPayload),
		runTemplate:   runReq,
		controllerCfg: sessionCfg,
	}
	session.serverRegistry = sessionCfg.ServerRegistry
	session.toolExec = &liveToolExecutor{session: session}

	session.sttSession, err = newLiveSTTSessionFunc(ctx, sessionCfg.VoicePipeline, sessionCfg.ResolvedSTTModel)
	if err != nil {
		h.writeLiveFatalAndClose(conn, &core.Error{
			Type:      core.ErrAPI,
			Message:   "failed to initialize stt stream: " + err.Error(),
			Code:      "stt_init_failed",
			RequestID: reqID,
		})
		return
	}
	defer func() {
		if session.sttSession != nil {
			_ = session.sttSession.Close()
		}
	}()

	session.wg.Add(1)
	go session.writerLoop()

	if !session.send(types.LiveSessionStartedEvent{
		Type:               "session_started",
		InputFormat:        liveInputFormat,
		InputSampleRateHz:  liveInputSampleRateHz,
		OutputFormat:       liveOutputFormat,
		OutputSampleRateHz: sessionCfg.OutputSampleRateHz,
		SilenceCommitMS:    liveSilenceCommitMS,
	}) {
		return
	}

	session.commitCh = make(chan []byte, 8)
	session.wg.Add(4)
	go session.readerLoop()
	go session.sttLoop()
	go session.commitTickerLoop()
	go session.turnWorker()

	<-ctx.Done()
	session.mu.Lock()
	session.state = liveStateClosing
	session.mu.Unlock()
	if session.sttSession != nil {
		_ = session.sttSession.Close()
	}
	close(session.commitCh)
	session.wg.Wait()
}

type liveSessionConfig struct {
	PublicModel        string
	ModelName          string
	Provider           core.Provider
	VoicePipeline      *voice.Pipeline
	ResolvedSTTModel   string
	OutputSampleRateHz int
	ServerRegistry     *servertools.Registry
}

func (h LiveHandler) readAndValidateLiveStart(ctx context.Context, r *http.Request, conn *websocket.Conn) (*liveInboundStartFrame, *types.RunRequest, *liveSessionConfig, error) {
	reqID, _ := mw.RequestIDFrom(r.Context())
	mt, data, err := conn.ReadMessage()
	if err != nil {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "failed to read start frame: " + err.Error(),
			Code:      "start_frame_invalid",
			RequestID: reqID,
		}
	}
	if mt != websocket.TextMessage {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "first frame must be text start frame",
			Code:      "start_frame_required",
			RequestID: reqID,
		}
	}
	var start liveInboundStartFrame
	if err := json.Unmarshal(data, &start); err != nil {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "invalid start frame json",
			Code:      "start_frame_invalid",
			RequestID: reqID,
		}
	}
	if strings.TrimSpace(start.Type) != "start" {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "first frame must be type=start",
			Code:      "start_frame_required",
			RequestID: reqID,
		}
	}
	if len(start.RunRequest) == 0 {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "run_request is required",
			Param:     "run_request",
			Code:      "start_frame_invalid",
			RequestID: reqID,
		}
	}
	normalizedRunReqRaw, seededEmptyMessages := normalizeLiveRunRequestForStrict(start.RunRequest)
	runReq, err := types.UnmarshalRunRequestStrict(normalizedRunReqRaw)
	if err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}
	if seededEmptyMessages {
		runReq.Request.Messages = nil
	}
	if err := limits.ValidateMessageRequest(&runReq.Request, h.Config); err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}

	if len(h.Config.ModelAllowlist) > 0 {
		if _, ok := h.Config.ModelAllowlist[runReq.Request.Model]; !ok {
			return nil, nil, nil, &core.Error{
				Type:      core.ErrPermission,
				Message:   "model is not allowlisted",
				Param:     "request.model",
				RequestID: reqID,
			}
		}
	}

	providerName, modelName, err := core.ParseModelString(runReq.Request.Model)
	if err != nil {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   err.Error(),
			Param:     "request.model",
			RequestID: reqID,
		}
	}
	upstreamKeyHeader, ok := compat.ProviderKeyHeader(providerName)
	if !ok {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "unsupported provider",
			Param:     "request.model",
			RequestID: reqID,
		}
	}
	upstreamKey := strings.TrimSpace(r.Header.Get(upstreamKeyHeader))
	if upstreamKey == "" {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrAuthentication,
			Message:   "missing upstream provider api key header",
			Param:     upstreamKeyHeader,
			Code:      "provider_key_missing",
			RequestID: reqID,
		}
	}
	if compatIssues := compat.ValidateMessageRequest(&runReq.Request, providerName, runReq.Request.Model); len(compatIssues) > 0 {
		return nil, nil, nil, &core.Error{
			Type:         core.ErrInvalidRequest,
			Message:      fmt.Sprintf("Request is incompatible with provider %s and model %s", providerName, modelName),
			CompatIssues: compatIssues,
			RequestID:    reqID,
		}
	}

	cartesiaKey := strings.TrimSpace(r.Header.Get("X-Provider-Key-Cartesia"))
	if cartesiaKey == "" {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrAuthentication,
			Message:   "missing voice provider api key header",
			Param:     "X-Provider-Key-Cartesia",
			Code:      "provider_key_missing",
			RequestID: reqID,
		}
	}

	if runReq.Request.Voice == nil || runReq.Request.Voice.Output == nil || strings.TrimSpace(runReq.Request.Voice.Output.Voice) == "" {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "run_request.request.voice.output.voice is required for /v1/live",
			Param:     "run_request.request.voice.output.voice",
			Code:      "live_voice_required",
			RequestID: reqID,
		}
	}
	if runReq.Request.Voice.Output.SampleRate <= 0 {
		runReq.Request.Voice.Output.SampleRate = liveDefaultOutputSampleRateHz
	}
	if !isSupportedLiveOutputSampleRate(runReq.Request.Voice.Output.SampleRate) {
		return nil, nil, nil, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "unsupported live output sample_rate",
			Param:     "run_request.request.voice.output.sample_rate",
			Code:      "run_validation_failed",
			RequestID: reqID,
		}
	}
	if strings.TrimSpace(runReq.Request.Voice.Output.Format) == "" {
		runReq.Request.Voice.Output.Format = "pcm"
	}
	if strings.TrimSpace(runReq.Request.STTModel) == "" {
		runReq.Request.STTModel = "cartesia/ink-whisper"
	}
	resolvedSTTModel, err := voice.ResolveSTTModel(runReq.Request.STTModel)
	if err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}
	runReq.Request.STTModel = resolvedSTTModel.Raw

	provider, err := h.Upstreams.New(providerName, upstreamKey)
	if err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}

	voicePipeline := newLiveVoicePipelineFunc(cartesiaKey, h.HTTPClient)

	effectiveServerTools := runReq.ServerTools
	if len(effectiveServerTools) == 0 {
		effectiveServerTools = runReq.Builtins
	}
	registry, err := newServerToolsRegistry(h.Config, h.HTTPClient, r, effectiveServerTools, runReq.ServerToolConfig)
	if err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}
	injectedTools, err := injectLiveTools(runReq.Request.Tools, effectiveServerTools, registry)
	if err != nil {
		coreErr, _ := coreErrorFrom(err, reqID)
		return nil, nil, nil, coreErr
	}
	runReq.Request.Tools = injectedTools
	runReq.Request.System = ensureLiveTalkToUserSystem(runReq.Request.System)

	publicModel := runReq.Request.Model
	runReq.Request.Model = modelName

	return &start, runReq, &liveSessionConfig{
		PublicModel:        publicModel,
		ModelName:          modelName,
		Provider:           provider,
		VoicePipeline:      voicePipeline,
		ResolvedSTTModel:   resolvedSTTModel.Model,
		OutputSampleRateHz: runReq.Request.Voice.Output.SampleRate,
		ServerRegistry:     registry,
	}, nil
}

func (h LiveHandler) writeLiveFatalAndClose(conn *websocket.Conn, err error) {
	if conn == nil {
		return
	}
	coreErr, _ := coreErrorFrom(err, "")
	msg := "internal error"
	code := ""
	if coreErr != nil {
		if strings.TrimSpace(coreErr.Message) != "" {
			msg = coreErr.Message
		}
		code = strings.TrimSpace(coreErr.Code)
	}
	_ = conn.WriteJSON(types.LiveErrorEvent{
		Type:    "error",
		Fatal:   true,
		Message: msg,
		Code:    code,
	})
	_ = conn.Close()
}

type liveSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	logger *slog.Logger
	reqID  string

	conn   *websocket.Conn
	sendCh chan any

	wg sync.WaitGroup

	mu               sync.Mutex
	state            liveSessionState
	currentUtterance []byte
	lastSpeechAt     time.Time
	sttSawText       bool
	runActive        bool
	history          []types.Message
	toolWaiters      map[string]chan liveToolResultPayload

	commitCh chan []byte

	runTemplate    *types.RunRequest
	controllerCfg  *liveSessionConfig
	serverRegistry *servertools.Registry
	toolExec       *liveToolExecutor

	sttSession liveSTTSession
	sttFailed  bool

	execCounter atomic.Uint64
}

func (s *liveSession) send(event any) bool {
	select {
	case s.sendCh <- event:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *liveSession) writerLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-s.sendCh:
			if !ok {
				return
			}
			if err := s.conn.WriteJSON(event); err != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *liveSession) readerLoop() {
	defer s.wg.Done()
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			s.cancel()
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			s.handleAudioChunk(data)
		case websocket.TextMessage:
			if err := s.handleControlFrame(data); err != nil {
				s.send(types.LiveErrorEvent{
					Type:    "error",
					Fatal:   false,
					Message: err.Error(),
					Code:    "protocol_error",
				})
			}
		case websocket.CloseMessage:
			s.cancel()
			return
		}
	}
}

func (s *liveSession) handleAudioChunk(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	s.currentUtterance = append(s.currentUtterance, data...)
	sttSession := s.sttSession
	s.mu.Unlock()
	if sttSession != nil {
		if err := sttSession.SendAudio(data); err != nil {
			s.failSTT(err)
		}
	}
}

func (s *liveSession) handleControlFrame(data []byte) error {
	var holder struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &holder); err != nil {
		return fmt.Errorf("invalid control frame json")
	}
	switch strings.TrimSpace(holder.Type) {
	case "tool_result":
		return s.handleToolResultFrame(data)
	case "stop":
		var frame liveInboundStopFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return fmt.Errorf("invalid stop frame")
		}
		s.cancel()
		return nil
	default:
		return fmt.Errorf("unsupported frame type %q", holder.Type)
	}
}

func (s *liveSession) handleToolResultFrame(data []byte) error {
	var frame liveInboundToolResultFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("invalid tool_result frame")
	}
	execID := strings.TrimSpace(frame.ExecutionID)
	if execID == "" {
		return fmt.Errorf("tool_result.execution_id is required")
	}
	content := make([]types.ContentBlock, 0, len(frame.Content))
	for _, raw := range frame.Content {
		block, err := types.UnmarshalContentBlock(raw)
		if err != nil {
			return fmt.Errorf("invalid tool_result.content block: %w", err)
		}
		content = append(content, block)
	}

	var toolErr *types.Error
	if frame.IsError {
		toolErr = &types.Error{
			Type:    string(core.ErrAPI),
			Message: "tool execution failed",
			Code:    "tool_execution_failed",
		}
		switch v := frame.Error.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				toolErr.Message = strings.TrimSpace(v)
			}
		case map[string]any:
			if raw, err := json.Marshal(v); err == nil {
				toolErr.ProviderError = string(raw)
			}
		default:
			if v != nil {
				toolErr.ProviderError = v
			}
		}
	}

	s.mu.Lock()
	waiter, ok := s.toolWaiters[execID]
	if ok {
		delete(s.toolWaiters, execID)
	}
	s.mu.Unlock()
	if !ok {
		s.send(types.LiveErrorEvent{
			Type:    "error",
			Fatal:   false,
			Message: "received tool_result for unknown execution_id",
			Code:    "tool_result_unmatched",
		})
		return nil
	}
	select {
	case waiter <- liveToolResultPayload{
		Content: content,
		IsError: frame.IsError,
		Error:   toolErr,
	}:
	case <-s.ctx.Done():
	}
	return nil
}

func (s *liveSession) sttLoop() {
	defer s.wg.Done()
	s.mu.Lock()
	sttSession := s.sttSession
	s.mu.Unlock()
	if sttSession == nil {
		return
	}
	for delta := range sttSession.Transcripts() {
		if strings.TrimSpace(delta.Text) == "" {
			continue
		}
		s.mu.Lock()
		s.sttSawText = true
		s.lastSpeechAt = time.Now()
		s.mu.Unlock()
	}
	if s.ctx.Err() == nil {
		s.failSTT(errors.New("stt stream closed unexpectedly"))
	}
}

func (s *liveSession) commitTickerLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			commit := s.maybeCommitUtterance()
			if len(commit) == 0 {
				continue
			}
			if !s.send(types.LiveUserTurnCommittedEvent{
				Type:       "user_turn_committed",
				AudioBytes: len(commit),
			}) {
				return
			}
			select {
			case s.commitCh <- commit:
			case <-s.ctx.Done():
				return
			}
		}
	}
}

func (s *liveSession) maybeCommitUtterance() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runActive {
		return nil
	}
	if !s.sttSawText {
		return nil
	}
	if len(s.currentUtterance) == 0 {
		return nil
	}
	if s.lastSpeechAt.IsZero() {
		return nil
	}
	if time.Since(s.lastSpeechAt) < liveSilenceCommitMS*time.Millisecond {
		return nil
	}
	committed := make([]byte, len(s.currentUtterance))
	copy(committed, s.currentUtterance)
	s.currentUtterance = s.currentUtterance[:0]
	s.sttSawText = false
	s.lastSpeechAt = time.Time{}
	return committed
}

func (s *liveSession) turnWorker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case pcm, ok := <-s.commitCh:
			if !ok {
				return
			}
			s.runTurn(pcm)
		}
	}
}

func (s *liveSession) runTurn(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	s.mu.Lock()
	s.runActive = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.runActive = false
		s.mu.Unlock()
	}()

	wav := encodeWAVPCM16Mono(pcm, liveInputSampleRateHz)
	userMsg := types.Message{
		Role: "user",
		Content: []types.ContentBlock{
			types.AudioSTTBlock{
				Type: "audio_stt",
				Source: types.AudioSource{
					Type:      "base64",
					MediaType: "audio/wav",
					Data:      base64.StdEncoding.EncodeToString(wav),
				},
			},
		},
	}

	runReq := cloneRunRequest(s.runTemplate)
	s.mu.Lock()
	historyCopy := make([]types.Message, len(s.history))
	copy(historyCopy, s.history)
	s.mu.Unlock()
	runReq.Request.Messages = append(historyCopy, userMsg)
	runReq.Request.Voice = nil

	controller := &runloop.Controller{
		Provider:          s.controllerCfg.Provider,
		Tools:             s.toolExec,
		VoicePipeline:     s.controllerCfg.VoicePipeline,
		StreamIdleTimeout: 60 * time.Second,
		RequestID:         s.reqID,
		PublicModel:       s.controllerCfg.PublicModel,
	}

	talkState := newLiveTalkTurnState(s)
	result, err := controller.RunStream(s.ctx, runReq, func(event types.RunStreamEvent) error {
		return talkState.handleRunEvent(event)
	})
	talkState.finish()

	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.send(types.LiveErrorEvent{
				Type:    "error",
				Fatal:   false,
				Message: "turn run failed: " + err.Error(),
				Code:    "turn_failed",
			})
		}
		return
	}
	if result == nil {
		return
	}

	if len(result.Messages) > 0 {
		s.mu.Lock()
		s.history = append([]types.Message(nil), result.Messages...)
		historySnapshot := append([]types.Message(nil), s.history...)
		s.mu.Unlock()
		_ = s.send(types.LiveTurnCompleteEvent{
			Type:       "turn_complete",
			StopReason: result.StopReason,
			History:    historySnapshot,
		})
	}
}

func (s *liveSession) failSTT(err error) {
	if err == nil {
		return
	}

	s.mu.Lock()
	if s.sttFailed {
		s.mu.Unlock()
		return
	}
	s.sttFailed = true
	sttSession := s.sttSession
	s.sttSession = nil
	s.mu.Unlock()

	if sttSession != nil {
		_ = sttSession.Close()
	}

	s.send(types.LiveErrorEvent{
		Type:    "error",
		Fatal:   true,
		Message: "live stt stream unavailable: " + err.Error(),
		Code:    "stt_unavailable",
	})
	s.cancel()
}

type liveTalkTurnState struct {
	session *liveSession

	metaByIndex    map[int]liveToolMeta
	decoderByIndex map[int]*vai.ToolArgStringDecoder

	tts      liveTTSSession
	ttsReady bool
	ttsErr   bool
	ttsDone  chan struct{}

	ttsProgressCh chan struct{}
}

func newLiveTalkTurnState(session *liveSession) *liveTalkTurnState {
	return &liveTalkTurnState{
		session:        session,
		metaByIndex:    make(map[int]liveToolMeta),
		decoderByIndex: make(map[int]*vai.ToolArgStringDecoder),
	}
}

func (s *liveTalkTurnState) handleRunEvent(event types.RunStreamEvent) error {
	switch ev := event.(type) {
	case types.AudioUnavailableEvent:
		s.session.send(types.LiveAudioUnavailableEvent{
			Type:    "audio_unavailable",
			Reason:  ev.Reason,
			Message: ev.Message,
		})
	case types.RunStreamEventWrapper:
		return s.handleStreamEvent(ev.Event)
	}
	return nil
}

func (s *liveTalkTurnState) handleStreamEvent(event types.StreamEvent) error {
	switch ev := event.(type) {
	case types.ContentBlockStartEvent:
		if tool, ok := ev.ContentBlock.(types.ToolUseBlock); ok {
			s.metaByIndex[ev.Index] = liveToolMeta{id: tool.ID, name: tool.Name}
			if strings.EqualFold(strings.TrimSpace(tool.Name), "talk_to_user") {
				s.decoderByIndex[ev.Index] = vai.NewToolArgStringDecoder(vai.ToolArgStringDecoderOptions{})
			}
		}
	case types.ContentBlockDeltaEvent:
		switch delta := ev.Delta.(type) {
		case types.TextDelta:
			if delta.Text == "" {
				return nil
			}
			s.session.send(types.LiveAssistantTextDeltaEvent{
				Type: "assistant_text_delta",
				Text: delta.Text,
			})
		case types.InputJSONDelta:
			meta, ok := s.metaByIndex[ev.Index]
			if !ok || !strings.EqualFold(strings.TrimSpace(meta.name), "talk_to_user") {
				return nil
			}
			decoder := s.decoderByIndex[ev.Index]
			if decoder == nil {
				decoder = vai.NewToolArgStringDecoder(vai.ToolArgStringDecoderOptions{})
				s.decoderByIndex[ev.Index] = decoder
			}
			update := decoder.Push(delta.PartialJSON)
			if !update.Found || update.Delta == "" {
				return nil
			}
			s.session.send(types.LiveTalkToUserTextDeltaEvent{
				Type:   "talk_to_user_text_delta",
				CallID: meta.id,
				Index:  ev.Index,
				Text:   update.Delta,
			})
			if s.ttsErr {
				return nil
			}
			if err := s.ensureTTS(); err != nil {
				s.ttsErr = true
				s.session.send(types.LiveAudioUnavailableEvent{
					Type:    "audio_unavailable",
					Reason:  "tts_failed",
					Message: "TTS synthesis failed: " + err.Error(),
				})
				return nil
			}
			if err := s.tts.OnTextDelta(update.Delta); err != nil {
				s.ttsErr = true
				s.session.send(types.LiveAudioUnavailableEvent{
					Type:    "audio_unavailable",
					Reason:  "tts_failed",
					Message: "TTS synthesis failed: " + err.Error(),
				})
			}
		}
	case types.ContentBlockStopEvent:
		delete(s.metaByIndex, ev.Index)
		delete(s.decoderByIndex, ev.Index)
	}
	return nil
}

func (s *liveTalkTurnState) ensureTTS() error {
	if s.ttsReady {
		return nil
	}
	if s.session.runTemplate == nil || s.session.runTemplate.Request.Voice == nil {
		return errors.New("voice output is not configured")
	}
	ttsModel := s.session.runTemplate.Request.TTSModel
	if strings.TrimSpace(ttsModel) == "" {
		ttsModel = "cartesia/sonic-3"
	}
	resolved, err := voice.ResolveTTSModel(ttsModel)
	if err != nil {
		return err
	}
	ttsSession, err := newLiveTTSSessionFunc(s.session.ctx, s.session.controllerCfg.VoicePipeline, s.session.runTemplate.Request.Voice, resolved.Model)
	if err != nil {
		return err
	}
	s.tts = ttsSession
	s.ttsReady = true
	s.ttsDone = make(chan struct{})
	s.ttsProgressCh = make(chan struct{}, 1)
	go s.forwardTTSAudio()
	return nil
}

func (s *liveTalkTurnState) forwardTTSAudio() {
	defer close(s.ttsDone)
	sampleRate := liveDefaultOutputSampleRateHz
	if s.session != nil && s.session.controllerCfg != nil && s.session.controllerCfg.OutputSampleRateHz > 0 {
		sampleRate = s.session.controllerCfg.OutputSampleRateHz
	}
	if s.session.runTemplate != nil && s.session.runTemplate.Request.Voice != nil && s.session.runTemplate.Request.Voice.Output != nil && s.session.runTemplate.Request.Voice.Output.SampleRate > 0 {
		sampleRate = s.session.runTemplate.Request.Voice.Output.SampleRate
	}
	var pending []byte
	flushPending := func(isFinal bool) {
		if len(pending) == 0 {
			return
		}
		s.session.send(types.LiveAudioChunkEvent{
			Type:         "audio_chunk",
			Format:       liveOutputFormat,
			SampleRateHz: sampleRate,
			Audio:        base64.StdEncoding.EncodeToString(pending),
			IsFinal:      isFinal,
		})
		s.signalTTSProgress()
		pending = nil
	}
	for chunk := range s.tts.Audio() {
		if len(chunk) == 0 {
			continue
		}
		flushPending(false)
		pending = append(pending[:0], chunk...)
	}
	flushPending(true)
}

func (s *liveTalkTurnState) signalTTSProgress() {
	if s == nil || s.ttsProgressCh == nil {
		return
	}
	select {
	case s.ttsProgressCh <- struct{}{}:
	default:
	}
}

func (s *liveTalkTurnState) finish() {
	if s.tts == nil {
		return
	}

	closeTTS := func(timeout time.Duration) {
		done := make(chan struct{})
		go func() {
			_ = s.tts.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}

	if s.ttsErr {
		closeTTS(500 * time.Millisecond)
		return
	}

	flushDone := false
	audioDone := s.ttsDone == nil
	var flushErr error
	flushResultCh := make(chan error, 1)
	go func() {
		flushResultCh <- s.tts.Flush()
	}()

	ttsDoneCh := s.ttsDone
	ttsProgressCh := s.ttsProgressCh
	timer := time.NewTimer(liveTTSDrainTimeout)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(liveTTSDrainTimeout)
	}

	for !flushDone || !audioDone {
		select {
		case err := <-flushResultCh:
			flushDone = true
			flushErr = err
			flushResultCh = nil
		case <-ttsDoneCh:
			audioDone = true
			ttsDoneCh = nil
		case <-ttsProgressCh:
			if !audioDone {
				resetTimer()
			}
		case <-timer.C:
			timeoutMsg := "timed out waiting for TTS completion"
			switch {
			case !flushDone && !audioDone:
				timeoutMsg = "timed out waiting for TTS flush and audio drain"
			case !flushDone:
				timeoutMsg = "timed out waiting for TTS flush"
			case !audioDone:
				timeoutMsg = "timed out waiting for TTS audio drain"
			}
			s.session.send(types.LiveAudioUnavailableEvent{
				Type:    "audio_unavailable",
				Reason:  "tts_failed",
				Message: "TTS synthesis failed: " + timeoutMsg,
			})
			closeTTS(500 * time.Millisecond)
			return
		case <-s.session.ctx.Done():
			closeTTS(500 * time.Millisecond)
			return
		}
	}

	if flushErr != nil {
		s.session.send(types.LiveAudioUnavailableEvent{
			Type:    "audio_unavailable",
			Reason:  "tts_failed",
			Message: "TTS synthesis failed: " + flushErr.Error(),
		})
	}
	_ = s.tts.Close()
}

type liveToolExecutor struct {
	session *liveSession
}

func (e *liveToolExecutor) Execute(ctx context.Context, name string, input map[string]any) ([]types.ContentBlock, *types.Error) {
	if e == nil || e.session == nil {
		return nil, &types.Error{
			Type:    string(core.ErrAPI),
			Message: "live session is not available",
			Code:    "live_session_unavailable",
		}
	}
	trimmed := strings.TrimSpace(name)
	if strings.EqualFold(trimmed, "talk_to_user") {
		return []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "delivered"},
		}, nil
	}
	if e.session.serverRegistry != nil && e.session.serverRegistry.Has(trimmed) {
		return e.session.serverRegistry.Execute(ctx, trimmed, input)
	}

	execID := fmt.Sprintf("exec_%d", e.session.execCounter.Add(1))
	waitCh := make(chan liveToolResultPayload, 1)
	e.session.mu.Lock()
	e.session.toolWaiters[execID] = waitCh
	e.session.mu.Unlock()

	if !e.session.send(types.LiveToolCallEvent{
		Type:        "tool_call",
		ExecutionID: execID,
		Name:        trimmed,
		Input:       input,
	}) {
		e.session.mu.Lock()
		delete(e.session.toolWaiters, execID)
		e.session.mu.Unlock()
		return nil, &types.Error{
			Type:    string(core.ErrAPI),
			Message: "live session closed while waiting for tool result",
			Code:    "tool_result_wait_failed",
		}
	}

	select {
	case payload := <-waitCh:
		if payload.IsError {
			if payload.Error != nil {
				return payload.Content, payload.Error
			}
			return payload.Content, &types.Error{
				Type:    string(core.ErrAPI),
				Message: "tool execution failed",
				Code:    "tool_execution_failed",
			}
		}
		return payload.Content, nil
	case <-ctx.Done():
		e.session.mu.Lock()
		delete(e.session.toolWaiters, execID)
		e.session.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &types.Error{
				Type:    string(core.ErrAPI),
				Message: "tool result timed out",
				Code:    "tool_timeout",
			}
		}
		return nil, &types.Error{
			Type:    string(core.ErrAPI),
			Message: "tool execution canceled",
			Code:    "tool_canceled",
		}
	}
}

func injectLiveTools(reqTools []types.Tool, serverTools []string, registry *servertools.Registry) ([]types.Tool, error) {
	out := make([]types.Tool, 0, len(reqTools)+len(serverTools)+1)
	seenByName := make(map[string]types.Tool, len(reqTools)+len(serverTools)+1)

	for i, tool := range reqTools {
		if tool.Type == types.ToolTypeFunction {
			name := strings.TrimSpace(tool.Name)
			if name == "" {
				return nil, &core.Error{
					Type:    core.ErrInvalidRequest,
					Message: "function tool name must be non-empty",
					Param:   fmt.Sprintf("request.tools[%d].name", i),
					Code:    "run_validation_failed",
				}
			}
			if _, exists := seenByName[name]; exists {
				return nil, &core.Error{
					Type:    core.ErrInvalidRequest,
					Message: "duplicate function tool name",
					Param:   fmt.Sprintf("request.tools[%d].name", i),
					Code:    "run_validation_failed",
				}
			}
			seenByName[name] = tool
		}
		out = append(out, tool)
	}

	for i, name := range serverTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		def, ok := registry.Definition(name)
		if !ok {
			return nil, &core.Error{
				Type:    core.ErrInvalidRequest,
				Message: fmt.Sprintf("unsupported server tool %q", name),
				Param:   fmt.Sprintf("server_tools[%d]", i),
				Code:    "unsupported_server_tool",
			}
		}
		if _, exists := seenByName[name]; exists {
			return nil, &core.Error{
				Type:    core.ErrInvalidRequest,
				Message: "function tool name collides with selected server tool",
				Param:   fmt.Sprintf("server_tools[%d]", i),
				Code:    "run_validation_failed",
			}
		}
		seenByName[name] = def
		out = append(out, def)
	}

	talkName := "talk_to_user"
	if existing, ok := seenByName[talkName]; ok {
		if existing.Type != types.ToolTypeFunction {
			return nil, &core.Error{
				Type:    core.ErrInvalidRequest,
				Message: "talk_to_user must be a function tool",
				Param:   "request.tools",
				Code:    "run_validation_failed",
			}
		}
		return out, nil
	}

	out = append(out, liveTalkToUserToolDefinition())
	return out, nil
}

func ensureLiveTalkToUserSystem(system any) any {
	base := strings.TrimSpace(systemToStringValue(system))
	if base == "" {
		return liveTalkToUserSystemInstruction
	}
	if strings.Contains(base, "talk_to_user") {
		return base
	}
	return base + "\n\n" + liveTalkToUserSystemInstruction
}

func systemToStringValue(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []types.ContentBlock:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			switch tb := block.(type) {
			case types.TextBlock:
				parts = append(parts, tb.Text)
			case *types.TextBlock:
				if tb != nil {
					parts = append(parts, tb.Text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if s, ok := system.(fmt.Stringer); ok {
			return s.String()
		}
		return fmt.Sprintf("%v", system)
	}
}

func liveTalkToUserToolDefinition() types.Tool {
	additionalProps := false
	return types.Tool{
		Type:        types.ToolTypeFunction,
		Name:        "talk_to_user",
		Description: "Speak text directly to the user via the voice/output channel.",
		InputSchema: &types.JSONSchema{
			Type: "object",
			Properties: map[string]types.JSONSchema{
				"content": {Type: "string", Description: "Exact text to speak to the user"},
			},
			Required:             []string{"content"},
			AdditionalProperties: &additionalProps,
		},
	}
}

func cloneRunRequest(src *types.RunRequest) *types.RunRequest {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Request = src.Request
	if len(src.Request.Messages) > 0 {
		dst.Request.Messages = append([]types.Message(nil), src.Request.Messages...)
	}
	if len(src.Request.Tools) > 0 {
		dst.Request.Tools = append([]types.Tool(nil), src.Request.Tools...)
	}
	if len(src.ServerTools) > 0 {
		dst.ServerTools = append([]string(nil), src.ServerTools...)
	}
	if len(src.Builtins) > 0 {
		dst.Builtins = append([]string(nil), src.Builtins...)
	}
	if src.Request.Metadata != nil {
		meta := make(map[string]any, len(src.Request.Metadata))
		for k, v := range src.Request.Metadata {
			meta[k] = v
		}
		dst.Request.Metadata = meta
	}
	if src.Request.Extensions != nil {
		ext := make(map[string]any, len(src.Request.Extensions))
		for k, v := range src.Request.Extensions {
			ext[k] = v
		}
		dst.Request.Extensions = ext
	}
	if src.ServerToolConfig != nil {
		cfg := make(map[string]any, len(src.ServerToolConfig))
		for k, v := range src.ServerToolConfig {
			cfg[k] = v
		}
		dst.ServerToolConfig = cfg
	}
	return &dst
}

func normalizeLiveRunRequestForStrict(raw json.RawMessage) (json.RawMessage, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw, false
	}

	changed := false
	seededMessages := false

	if reqRaw, ok := top["request"]; ok {
		var reqObj map[string]json.RawMessage
		if err := json.Unmarshal(reqRaw, &reqObj); err == nil {
			msgsRaw, hasMessages := reqObj["messages"]
			if !hasMessages || isEmptyMessageArrayRaw(msgsRaw) {
				reqObj["messages"] = json.RawMessage(`[{"role":"user","content":""}]`)
				changed = true
				seededMessages = true
			}
			if changedReqRaw, err := json.Marshal(reqObj); err == nil {
				top["request"] = changedReqRaw
			}
		}
	}

	if runRaw, ok := top["run"]; ok && len(runRaw) > 0 {
		var runObj map[string]any
		if err := json.Unmarshal(runRaw, &runObj); err == nil && runObj != nil {
			runChanged := false
			for _, key := range []string{"max_turns", "max_tool_calls", "timeout_ms", "tool_timeout_ms"} {
				if deleteIfNonPositiveNumber(runObj, key) {
					runChanged = true
				}
			}
			if runChanged {
				if changedRunRaw, err := json.Marshal(runObj); err == nil {
					top["run"] = changedRunRaw
					changed = true
				}
			}
		}
	}

	if !changed {
		return raw, seededMessages
	}
	normalizedRaw, err := json.Marshal(top)
	if err != nil {
		return raw, seededMessages
	}
	return normalizedRaw, seededMessages
}

func isEmptyMessageArrayRaw(raw json.RawMessage) bool {
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return false
	}
	return len(messages) == 0
}

func deleteIfNonPositiveNumber(obj map[string]any, key string) bool {
	value, ok := obj[key]
	if !ok {
		return false
	}
	switch v := value.(type) {
	case float64:
		if v <= 0 {
			delete(obj, key)
			return true
		}
	case int:
		if v <= 0 {
			delete(obj, key)
			return true
		}
	case int64:
		if v <= 0 {
			delete(obj, key)
			return true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n <= 0 {
			delete(obj, key)
			return true
		}
	}
	return false
}

func isSupportedLiveOutputSampleRate(rate int) bool {
	switch rate {
	case 8000, 16000, 22050, 24000, 44100, 48000:
		return true
	default:
		return false
	}
}

func encodeWAVPCM16Mono(pcm []byte, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = liveInputSampleRateHz
	}
	const bitsPerSample = 16
	const channels = 1
	dataSize := len(pcm)
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataSize))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))
	copy(buf[44:], pcm)
	return buf
}
