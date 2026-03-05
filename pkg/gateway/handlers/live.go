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
	"unicode"
	"unicode/utf8"

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
)

const (
	liveInputFormat               = "pcm_s16le"
	liveInputSampleRateHz         = 16000
	liveOutputFormat              = "pcm_s16le"
	liveDefaultOutputSampleRateHz = 16000
	liveSilenceCommitMS           = 900
)

const liveGracePeriod = 5 * time.Second

var liveTTSDrainTimeout = 10 * time.Second

const liveAssistantSpeechSystemInstruction = `Live voice mode:
- Speak to the user using normal assistant text only.
- Do NOT call the talk_to_user tool (it is deprecated in live mode).
- You may use other tools first, then answer the user with assistant text.
- Avoid tool-only chatter. All assistant text may be spoken aloud to the user.
User messages are transcribed via STT and may contain misspellings.`

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

type liveInboundPlaybackStateFrame struct {
	Type   string `json:"type"`
	TurnID string `json:"turn_id"`
	State  string `json:"state"`
}

type liveInboundPlaybackMarkFrame struct {
	Type     string `json:"type"`
	TurnID   string `json:"turn_id"`
	PlayedMS int    `json:"played_ms"`
}

type liveToolResultPayload struct {
	Content []types.ContentBlock
	IsError bool
	Error   *types.Error
}

type liveCommittedUtterance struct {
	TurnID      string
	PCM         []byte
	SpeechEnded time.Time
}

type liveTurnLifecycle string

const (
	liveTurnLifecycleRunning       liveTurnLifecycle = "running"
	liveTurnLifecycleAwaitingGrace liveTurnLifecycle = "awaiting_grace"
	liveTurnLifecycleFinalized     liveTurnLifecycle = "finalized"
	liveTurnLifecycleCancelled     liveTurnLifecycle = "cancelled"
)

type livePendingTurnResult struct {
	stopReason types.RunStopReason
	history    []types.Message
}

type liveTurnRuntime struct {
	id                  string
	lifecycle           liveTurnLifecycle
	speechEndedAt       time.Time
	graceDeadline       time.Time
	toolCalled          bool
	playbackFinished    bool
	audioStarted        bool
	playedMS            int
	interruptRequested  bool
	interruptPlayedMS   int
	interruptFrozen     bool
	suppressOutgoing    bool
	assistantFullText   strings.Builder
	assistantTimestamps []tts.WordTimestampsBatch
	pendingResult       *livePendingTurnResult
	runCancel           context.CancelFunc
	baseUserPCM         []byte
	baseHistory         []types.Message
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
	Timestamps() <-chan tts.WordTimestampsBatch
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
	ttsCtx, err := pipeline.NewStreamingTTSContextWithExtraOptions(ctx, voiceCfg, ttsModel, tts.StreamingContextOptions{
		AddTimestamps:           true,
		UseNormalizedTimestamps: true,
	})
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
		ctx:            ctx,
		cancel:         cancel,
		logger:         h.Logger,
		reqID:          reqID,
		conn:           conn,
		prioritySendCh: make(chan any, 16),
		sendCh:         make(chan any, 256),
		state:          liveStateRunning,
		toolWaiters:    make(map[string]chan liveToolResultPayload),
		runTemplate:    runReq,
		controllerCfg:  sessionCfg,
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

	session.commitCh = make(chan liveCommittedUtterance, 8)
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
	runReq.Request.System = ensureLiveAssistantSpeechSystem(runReq.Request.System)

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

	conn           *websocket.Conn
	prioritySendCh chan any
	sendCh         chan any

	wg sync.WaitGroup

	mu               sync.Mutex
	state            liveSessionState
	currentUtterance []byte
	lastSpeechAt     time.Time
	sttSawText       bool
	runBusy          bool
	activeTurn       *liveTurnRuntime
	aggregatePrefix  []byte
	history          []types.Message
	toolWaiters      map[string]chan liveToolResultPayload

	commitCh chan liveCommittedUtterance

	runTemplate    *types.RunRequest
	controllerCfg  *liveSessionConfig
	serverRegistry *servertools.Registry
	toolExec       *liveToolExecutor

	sttSession liveSTTSession
	sttFailed  bool

	execCounter atomic.Uint64
	turnCounter atomic.Uint64
}

func (s *liveSession) send(event any) bool {
	select {
	case s.sendCh <- event:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *liveSession) sendPriority(event any) bool {
	// Tests and some non-live call paths may not initialize prioritySendCh.
	// Fall back to the normal send channel so callers don't block forever.
	if s == nil || s.prioritySendCh == nil {
		return s.send(event)
	}
	select {
	case s.prioritySendCh <- event:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *liveSession) writerLoop() {
	defer s.wg.Done()
	for {
		// Drain priority events first to minimize latency for hard-stop/control messages.
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.prioritySendCh:
			if err := s.conn.WriteJSON(event); err != nil {
				s.cancel()
				return
			}
			continue
		default:
		}

		select {
		case <-s.ctx.Done():
			return
		case event := <-s.prioritySendCh:
			if err := s.conn.WriteJSON(event); err != nil {
				s.cancel()
				return
			}
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
	case "playback_state":
		return s.handlePlaybackStateFrame(data)
	case "playback_mark":
		return s.handlePlaybackMarkFrame(data)
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

func (s *liveSession) handlePlaybackStateFrame(data []byte) error {
	var frame liveInboundPlaybackStateFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("invalid playback_state frame")
	}
	turnID := strings.TrimSpace(frame.TurnID)
	if turnID == "" {
		// Keep wire compatibility: ignore empty turn_id from older clients.
		return nil
	}
	state := strings.ToLower(strings.TrimSpace(frame.State))
	if state != "finished" && state != "stopped" {
		return fmt.Errorf("playback_state.state must be finished or stopped")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return nil
	}
	s.activeTurn.playbackFinished = true
	return nil
}

func (s *liveSession) handlePlaybackMarkFrame(data []byte) error {
	var frame liveInboundPlaybackMarkFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("invalid playback_mark frame")
	}
	turnID := strings.TrimSpace(frame.TurnID)
	if turnID == "" {
		// Keep wire compatibility: ignore empty turn_id.
		return nil
	}
	if frame.PlayedMS < 0 {
		return fmt.Errorf("playback_mark.played_ms must be >= 0")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return nil
	}
	if frame.PlayedMS > s.activeTurn.playedMS {
		s.activeTurn.playedMS = frame.PlayedMS
	}
	return nil
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
		now := time.Now()
		var (
			cancelRun    context.CancelFunc
			cancelTurnID string
			emitCancel   bool
			emitReset    bool
		)
		s.mu.Lock()
		s.sttSawText = true
		s.lastSpeechAt = now
		if active := s.activeTurn; active != nil && !active.interruptRequested {
			playing := active.audioStarted && !active.playbackFinished
			if s.isGraceCancelableLocked(active, now) {
				active.lifecycle = liveTurnLifecycleCancelled
				active.pendingResult = nil
				active.playbackFinished = false
				s.aggregatePrefix = append([]byte(nil), active.baseUserPCM...)
				cancelRun = active.runCancel
				active.runCancel = nil
				cancelTurnID = active.id
				emitCancel = true
			} else if playing {
				// Interrupt (barge-in) outside grace-cancel rules.
				active.interruptRequested = true
				active.interruptPlayedMS = active.playedMS
				active.interruptFrozen = true
				active.suppressOutgoing = true
				cancelRun = active.runCancel
				active.runCancel = nil
				cancelTurnID = active.id
				emitReset = true
			}
		}
		s.mu.Unlock()
		if cancelRun != nil {
			cancelRun()
		}
		if emitCancel {
			s.sendPriority(types.LiveTurnCancelledEvent{
				Type:   "turn_cancelled",
				TurnID: cancelTurnID,
				Reason: "grace_period",
			})
		}
		if emitReset {
			s.sendPriority(types.LiveAudioResetEvent{
				Type:   "audio_reset",
				TurnID: cancelTurnID,
				Reason: "barge_in",
			})
		}
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
			commit, ok := s.maybeCommitUtterance()
			if !ok {
				continue
			}
			if !s.send(types.LiveUserTurnCommittedEvent{
				Type:       "user_turn_committed",
				TurnID:     commit.TurnID,
				AudioBytes: len(commit.PCM),
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

func (s *liveSession) maybeCommitUtterance() (liveCommittedUtterance, bool) {
	var none liveCommittedUtterance
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runBusy {
		return none, false
	}
	if !s.sttSawText {
		return none, false
	}
	if len(s.currentUtterance) == 0 {
		return none, false
	}
	if s.lastSpeechAt.IsZero() {
		return none, false
	}
	if time.Since(s.lastSpeechAt) < liveSilenceCommitMS*time.Millisecond {
		return none, false
	}

	segment := make([]byte, len(s.currentUtterance))
	copy(segment, s.currentUtterance)
	s.currentUtterance = s.currentUtterance[:0]
	speechEnded := s.lastSpeechAt
	if speechEnded.IsZero() {
		speechEnded = time.Now()
	}
	s.sttSawText = false
	s.lastSpeechAt = time.Time{}

	committed := segment
	if len(s.aggregatePrefix) > 0 {
		combined := make([]byte, len(s.aggregatePrefix)+len(segment))
		copy(combined, s.aggregatePrefix)
		copy(combined[len(s.aggregatePrefix):], segment)
		committed = combined
		s.aggregatePrefix = nil
	}

	return liveCommittedUtterance{
		TurnID:      fmt.Sprintf("turn_%d", s.turnCounter.Add(1)),
		PCM:         committed,
		SpeechEnded: speechEnded,
	}, true
}

func (s *liveSession) turnWorker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case committed, ok := <-s.commitCh:
			if !ok {
				return
			}
			s.runTurn(committed)
		}
	}
}

func (s *liveSession) runTurn(committed liveCommittedUtterance) {
	if len(committed.PCM) == 0 {
		return
	}

	runCtx, runCancel := context.WithCancel(s.ctx)
	turn := &liveTurnRuntime{
		id:            committed.TurnID,
		lifecycle:     liveTurnLifecycleRunning,
		speechEndedAt: committed.SpeechEnded,
		graceDeadline: committed.SpeechEnded.Add(liveGracePeriod),
		runCancel:     runCancel,
		baseUserPCM:   append([]byte(nil), committed.PCM...),
	}

	s.mu.Lock()
	s.runBusy = true
	s.activeTurn = turn
	s.mu.Unlock()
	defer func() {
		runCancel()
		s.mu.Lock()
		s.runBusy = false
		if s.activeTurn == turn {
			s.activeTurn = nil
		}
		s.mu.Unlock()
	}()

	wav := encodeWAVPCM16Mono(committed.PCM, liveInputSampleRateHz)
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
	turn.baseHistory = append([]types.Message(nil), runReq.Request.Messages...)

	controller := &runloop.Controller{
		Provider:          s.controllerCfg.Provider,
		Tools:             s.toolExec,
		VoicePipeline:     s.controllerCfg.VoicePipeline,
		StreamIdleTimeout: 60 * time.Second,
		RequestID:         s.reqID,
		PublicModel:       s.controllerCfg.PublicModel,
	}

	talkState := newLiveTalkTurnState(s, turn.id)
	result, err := controller.RunStream(runCtx, runReq, func(event types.RunStreamEvent) error {
		return talkState.handleRunEvent(event)
	})
	talkState.finish()

	s.mu.Lock()
	if s.activeTurn == turn {
		turn.runCancel = nil
	}
	turnCancelled := s.activeTurn == turn && turn.lifecycle == liveTurnLifecycleCancelled
	turnInterrupted := s.activeTurn == turn && turn.interruptRequested
	s.mu.Unlock()

	if err != nil {
		if turnCancelled && errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, context.Canceled) && turnInterrupted {
			// Continue below to finalize an interrupted turn with truncated assistant speech content.
		} else {
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
	}
	if result == nil && !turnInterrupted {
		return
	}
	if result != nil && len(result.Messages) == 0 && !turnInterrupted {
		return
	}

	// If this was an interruption-driven cancellation, synthesize a coherent pending history.
	if turnInterrupted && (result == nil || errors.Is(err, context.Canceled)) {
		stopReason := types.RunStopReasonCancelled
		history := append([]types.Message(nil), turn.baseHistory...)
		if result != nil && len(result.Messages) > 0 {
			stopReason = result.StopReason
			history = append([]types.Message(nil), result.Messages...)
		}
		history = ensureAssistantTextHistory(history, turn.assistantFullText.String())
		if !s.setPendingTurnResult(turn.id, &livePendingTurnResult{
			stopReason: stopReason,
			history:    history,
		}) {
			return
		}
		s.finalizeTurn(turn.id)
		return
	}

	if !s.setPendingTurnResult(turn.id, &livePendingTurnResult{
		stopReason: result.StopReason,
		history:    append([]types.Message(nil), result.Messages...),
	}) {
		return
	}
	if s.shouldFinalizeTurnNow(turn.id) {
		s.finalizeTurn(turn.id)
		return
	}

	waitTicker := time.NewTicker(50 * time.Millisecond)
	defer waitTicker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-waitTicker.C:
			finalize, cancelled := s.turnFinalizeStatus(turn.id)
			if cancelled {
				return
			}
			if finalize {
				s.finalizeTurn(turn.id)
				return
			}
		}
	}
}

func (s *liveSession) isGraceCancelableLocked(turn *liveTurnRuntime, now time.Time) bool {
	if turn == nil {
		return false
	}
	if turn.lifecycle != liveTurnLifecycleRunning && turn.lifecycle != liveTurnLifecycleAwaitingGrace {
		return false
	}
	if turn.interruptRequested {
		return false
	}
	if turn.toolCalled || turn.playbackFinished {
		return false
	}
	return now.Before(turn.graceDeadline) || now.Equal(turn.graceDeadline)
}

func (s *liveSession) setPendingTurnResult(turnID string, result *livePendingTurnResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return false
	}
	if s.activeTurn.lifecycle == liveTurnLifecycleCancelled {
		return false
	}
	s.activeTurn.pendingResult = result
	s.activeTurn.lifecycle = liveTurnLifecycleAwaitingGrace
	return true
}

func (s *liveSession) shouldFinalizeTurnNow(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return false
	}
	turn := s.activeTurn
	if turn.lifecycle == liveTurnLifecycleCancelled || turn.pendingResult == nil {
		return false
	}
	if turn.interruptRequested {
		return true
	}
	if turn.toolCalled || !turn.audioStarted || turn.playbackFinished {
		return true
	}
	return !time.Now().Before(turn.graceDeadline)
}

func (s *liveSession) turnFinalizeStatus(turnID string) (finalize bool, cancelled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return false, true
	}
	turn := s.activeTurn
	if turn.lifecycle == liveTurnLifecycleCancelled {
		return false, true
	}
	if turn.pendingResult == nil {
		return false, false
	}
	if turn.interruptRequested {
		return true, false
	}
	if turn.toolCalled || !turn.audioStarted || turn.playbackFinished {
		return true, false
	}
	return !time.Now().Before(turn.graceDeadline), false
}

func (s *liveSession) finalizeTurn(turnID string) {
	var ev types.LiveTurnCompleteEvent

	s.mu.Lock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		s.mu.Unlock()
		return
	}
	turn := s.activeTurn
	if turn.lifecycle == liveTurnLifecycleCancelled || turn.lifecycle == liveTurnLifecycleFinalized || turn.pendingResult == nil {
		s.mu.Unlock()
		return
	}
	if turn.interruptRequested {
		s.applyInterruptTruncationLocked(turn)
	}
	turn.lifecycle = liveTurnLifecycleFinalized
	s.history = append([]types.Message(nil), turn.pendingResult.history...)
	ev = types.LiveTurnCompleteEvent{
		Type:       "turn_complete",
		TurnID:     turnID,
		StopReason: turn.pendingResult.stopReason,
		History:    append([]types.Message(nil), s.history...),
	}
	s.mu.Unlock()

	_ = s.send(ev)
}

func (s *liveSession) markToolCalled(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return
	}
	s.activeTurn.toolCalled = true
}

func (s *liveSession) markAudioStarted(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return
	}
	s.activeTurn.audioStarted = true
}

func (s *liveSession) isTurnSuppressed(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil || s.activeTurn.id != turnID {
		return false
	}
	return s.activeTurn.suppressOutgoing
}

const liveInterruptMarker = "[user interrupt detected]"

func (s *liveSession) applyInterruptTruncationLocked(turn *liveTurnRuntime) {
	if turn == nil || turn.pendingResult == nil {
		return
	}
	playedMS := turn.interruptPlayedMS
	if playedMS <= 0 {
		playedMS = turn.playedMS
	}
	fullText := turn.assistantFullText.String()
	prefix := truncateAssistantTextPrefix(fullText, turn.assistantTimestamps, playedMS)

	content := liveInterruptMarker
	if strings.TrimSpace(prefix) != "" {
		content = prefix + " " + liveInterruptMarker
	}

	if ok := setLastAssistantTextContent(turn.pendingResult.history, content); ok {
		return
	}
	turn.pendingResult.history = ensureAssistantTextHistory(turn.pendingResult.history, content)
}

func ensureAssistantTextHistory(history []types.Message, content string) []types.Message {
	if ok := setLastAssistantTextContent(history, content); ok {
		return history
	}
	assistant := types.Message{Role: "assistant", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: content}}}
	return append(history, assistant)
}

func setLastAssistantTextContent(history []types.Message, content string) bool {
	for mi := len(history) - 1; mi >= 0; mi-- {
		if !strings.EqualFold(strings.TrimSpace(history[mi].Role), "assistant") {
			continue
		}
		blocks := history[mi].ContentBlocks()
		hasText := false
		for _, block := range blocks {
			switch block.(type) {
			case types.TextBlock, *types.TextBlock:
				hasText = true
			}
		}
		if !hasText {
			continue
		}

		out := make([]types.ContentBlock, 0, len(blocks))
		replaced := false
		for _, block := range blocks {
			switch block.(type) {
			case types.TextBlock, *types.TextBlock:
				if replaced {
					continue
				}
				out = append(out, types.TextBlock{Type: "text", Text: content})
				replaced = true
			default:
				out = append(out, block)
			}
		}
		if replaced {
			history[mi].Content = out
			return true
		}
	}
	return false
}

type liveTalkToken struct {
	start int
	end   int
	norm  string
}

func truncateAssistantTextPrefix(fullText string, batches []tts.WordTimestampsBatch, playedMS int) string {
	fullText = strings.TrimRight(fullText, " \n\t")
	if fullText == "" || playedMS <= 0 {
		return ""
	}

	type tsWord struct {
		norm  string
		endMS int
	}
	var ts []tsWord
	for _, batch := range batches {
		n := len(batch.Words)
		if len(batch.EndSec) < n {
			n = len(batch.EndSec)
		}
		for i := 0; i < n; i++ {
			w := normalizeSpokenToken(batch.Words[i])
			if w == "" {
				continue
			}
			endMS := int(batch.EndSec[i] * 1000)
			ts = append(ts, tsWord{norm: w, endMS: endMS})
		}
	}
	if len(ts) == 0 {
		return ""
	}

	lastIdx := -1
	for i := range ts {
		if ts[i].endMS <= playedMS {
			lastIdx = i
		}
	}
	if lastIdx < 0 {
		return ""
	}

	tokens := tokenizeTalkText(fullText)
	if len(tokens) == 0 {
		return ""
	}

	// Greedy sequential alignment from timestamps → text tokens.
	mapping := make(map[int]int, lastIdx+1) // tsIndex -> tokenEndChar
	ti := 0
	for i := 0; i <= lastIdx && ti < len(tokens); i++ {
		if ts[i].norm == "" {
			continue
		}
		found := -1
		limit := ti + 8
		if limit > len(tokens) {
			limit = len(tokens)
		}
		for j := ti; j < limit; j++ {
			if tokens[j].norm == ts[i].norm {
				found = j
				break
			}
		}
		if found == -1 {
			continue
		}
		mapping[i] = tokens[found].end
		ti = found + 1
	}

	cutPos := -1
	for i := lastIdx; i >= 0; i-- {
		if end, ok := mapping[i]; ok {
			cutPos = end
			break
		}
	}
	if cutPos <= 0 {
		return ""
	}
	if cutPos > len(fullText) {
		cutPos = len(fullText)
	}

	// Include immediate trailing punctuation/quotes (if any) up to whitespace.
	extended := cutPos
	for extended < len(fullText) {
		r, size := utf8.DecodeRuneInString(fullText[extended:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		if unicode.IsSpace(r) {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			break
		}
		extended += size
	}
	out := strings.TrimRight(fullText[:extended], " \n\t")
	return out
}

func tokenizeTalkText(text string) []liveTalkToken {
	var tokens []liveTalkToken
	start := -1
	prevWasWord := false

	flush := func(end int) {
		if start < 0 || end <= start {
			start = -1
			prevWasWord = false
			return
		}
		raw := text[start:end]
		norm := normalizeSpokenToken(raw)
		if norm != "" {
			tokens = append(tokens, liveTalkToken{start: start, end: end, norm: norm})
		}
		start = -1
		prevWasWord = false
	}

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			flush(i)
			i++
			continue
		}
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r)
		if isWord {
			if start < 0 {
				start = i
			}
			prevWasWord = true
			i += size
			continue
		}
		// Allow internal apostrophes/hyphens when surrounded by word chars.
		if (r == '\'' || r == '-') && start >= 0 && prevWasWord {
			nextIndex := i + size
			if nextIndex < len(text) {
				nr, _ := utf8.DecodeRuneInString(text[nextIndex:])
				if unicode.IsLetter(nr) || unicode.IsDigit(nr) {
					i += size
					prevWasWord = false
					continue
				}
			}
		}
		flush(i)
		i += size
	}
	flush(len(text))
	return tokens
}

func normalizeSpokenToken(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *liveSession) currentTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil {
		return ""
	}
	return s.activeTurn.id
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
	turnID  string

	tts      liveTTSSession
	ttsReady bool
	ttsErr   bool
	ttsDone  chan struct{}

	ttsProgressCh chan struct{}
}

func newLiveTalkTurnState(session *liveSession, turnID string) *liveTalkTurnState {
	return &liveTalkTurnState{
		session: session,
		turnID:  turnID,
	}
}

func (s *liveTalkTurnState) handleRunEvent(event types.RunStreamEvent) error {
	switch ev := event.(type) {
	case types.AudioUnavailableEvent:
		s.session.send(types.LiveAudioUnavailableEvent{
			Type:    "audio_unavailable",
			TurnID:  s.turnID,
			Reason:  ev.Reason,
			Message: ev.Message,
		})
	case types.RunToolCallStartEvent:
		s.session.markToolCalled(s.turnID)
	case types.RunStreamEventWrapper:
		return s.handleStreamEvent(ev.Event)
	}
	return nil
}

func (s *liveTalkTurnState) handleStreamEvent(event types.StreamEvent) error {
	handleAssistantText := func(text string) error {
		if text == "" {
			return nil
		}
		if s.session != nil {
			s.session.mu.Lock()
			if s.session.activeTurn != nil && s.session.activeTurn.id == s.turnID && !s.session.activeTurn.interruptFrozen {
				s.session.activeTurn.assistantFullText.WriteString(text)
			}
			s.session.mu.Unlock()
			if s.session.isTurnSuppressed(s.turnID) {
				return nil
			}
		}
		s.session.send(types.LiveAssistantTextDeltaEvent{
			Type:   "assistant_text_delta",
			TurnID: s.turnID,
			Text:   text,
		})
		if s.ttsErr {
			return nil
		}
		if s.session == nil || s.session.runTemplate == nil || s.session.runTemplate.Request.Voice == nil {
			// Live voice output not configured for this session/test run.
			return nil
		}
		if err := s.ensureTTS(); err != nil {
			s.ttsErr = true
			s.session.send(types.LiveAudioUnavailableEvent{
				Type:    "audio_unavailable",
				TurnID:  s.turnID,
				Reason:  "tts_failed",
				Message: "TTS synthesis failed: " + err.Error(),
			})
			return nil
		}
		if err := s.tts.OnTextDelta(text); err != nil {
			s.ttsErr = true
			s.session.send(types.LiveAudioUnavailableEvent{
				Type:    "audio_unavailable",
				TurnID:  s.turnID,
				Reason:  "tts_failed",
				Message: "TTS synthesis failed: " + err.Error(),
			})
		}
		return nil
	}

	switch ev := event.(type) {
	case types.ContentBlockStartEvent:
		if tb, ok := ev.ContentBlock.(types.TextBlock); ok {
			return handleAssistantText(tb.Text)
		}
		if tb, ok := ev.ContentBlock.(*types.TextBlock); ok && tb != nil {
			return handleAssistantText(tb.Text)
		}
	case types.ContentBlockDeltaEvent:
		if td, ok := ev.Delta.(types.TextDelta); ok {
			return handleAssistantText(td.Text)
		}
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
	go s.collectTTSTimestamps()
	return nil
}

func (s *liveTalkTurnState) collectTTSTimestamps() {
	if s == nil || s.tts == nil || s.session == nil {
		return
	}
	for batch := range s.tts.Timestamps() {
		if len(batch.Words) == 0 || len(batch.EndSec) == 0 {
			continue
		}
		s.session.mu.Lock()
		if s.session.activeTurn != nil && s.session.activeTurn.id == s.turnID {
			s.session.activeTurn.assistantTimestamps = append(s.session.activeTurn.assistantTimestamps, batch)
		}
		s.session.mu.Unlock()
	}
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

	// Keep audio chunks relatively small so client playback writes do not block the
	// websocket reader for long stretches. This is critical for barge-in: the
	// client must be able to receive `audio_reset` promptly while audio is playing.
	//
	// Target <= ~80ms of PCM per websocket event.
	maxChunkBytes := (sampleRate * 2 * 80) / 1000
	if maxChunkBytes < 320 {
		maxChunkBytes = 320
	}

	var pending []byte
	sendPCM := func(pcm []byte, isFinal bool) {
		if len(pcm) == 0 {
			return
		}
		for off := 0; off < len(pcm); {
			if s.session != nil && s.session.isTurnSuppressed(s.turnID) {
				return
			}
			end := off + maxChunkBytes
			if end > len(pcm) {
				end = len(pcm)
			}
			part := pcm[off:end]
			off = end
			finalPart := isFinal && off >= len(pcm)

			s.session.send(types.LiveAudioChunkEvent{
				Type:         "audio_chunk",
				TurnID:       s.turnID,
				Format:       liveOutputFormat,
				SampleRateHz: sampleRate,
				Audio:        base64.StdEncoding.EncodeToString(part),
				IsFinal:      finalPart,
			})
			s.session.markAudioStarted(s.turnID)
			s.signalTTSProgress()
		}
	}
	flushPending := func(isFinal bool) {
		if len(pending) == 0 {
			return
		}
		if s.session != nil && s.session.isTurnSuppressed(s.turnID) {
			pending = nil
			return
		}
		sendPCM(pending, isFinal)
		pending = nil
	}
	for chunk := range s.tts.Audio() {
		if len(chunk) == 0 {
			continue
		}
		if s.session != nil && s.session.isTurnSuppressed(s.turnID) {
			_ = s.tts.Close()
			return
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
	if s.session != nil && s.session.isTurnSuppressed(s.turnID) {
		_ = s.tts.Close()
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
				TurnID:  s.turnID,
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
			TurnID:  s.turnID,
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
		return nil, &types.Error{
			Type:    string(core.ErrInvalidRequest),
			Message: "talk_to_user is deprecated in live mode; use normal assistant text for speaking",
			Code:    "talk_to_user_deprecated",
		}
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
		TurnID:      e.session.currentTurnID(),
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
	out := make([]types.Tool, 0, len(reqTools)+len(serverTools))
	seenByName := make(map[string]types.Tool, len(reqTools)+len(serverTools))

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
			if strings.EqualFold(name, "talk_to_user") {
				return nil, &core.Error{
					Type:    core.ErrInvalidRequest,
					Message: "talk_to_user is deprecated in live mode; use normal assistant text for speaking",
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
	return out, nil
}

func ensureLiveAssistantSpeechSystem(system any) any {
	base := strings.TrimSpace(systemToStringValue(system))
	if base == "" {
		return liveAssistantSpeechSystemInstruction
	}
	return base + "\n\n" + liveAssistantSpeechSystemInstruction
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
