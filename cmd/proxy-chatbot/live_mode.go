package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
	vai "github.com/vango-go/vai-lite/sdk"
)

const (
	liveClientInputSampleRateHz          = 16000
	liveClientDefaultOutputSampleRateHz  = 16000
	liveClientFallbackOutputSampleRateHz = 8000
	liveClientSilenceCommitMS            = 600

	liveDefaultRunMaxTurns      = 8
	liveDefaultRunMaxToolCalls  = 20
	liveDefaultRunTimeoutMS     = 60000
	liveDefaultRunToolTimeoutMS = 30000
)

var (
	livePlaybackMarkInterval     = 250 * time.Millisecond
	livePlaybackMarkSafetyMargin = 100 * time.Millisecond
)

const (
	liveMicSampleRateHz = 16000

	// Local barge-in detection: we cut local playback quickly when the mic signal indicates
	// the user is speaking over assistant audio. This avoids waiting for STT to emit text.
	//
	// We avoid naive RMS-only triggers by tracking an adaptive baseline while assistant audio is playing.
	liveBargeInMinAboveMS     = 100
	liveBargeInRefractory     = 500 * time.Millisecond
	liveBargeInSuppressWindow = 750 * time.Millisecond

	liveBargeInFixedRMS      = 800.0
	liveBargeInBaselineAlpha = 0.08
	liveBargeInBaselineMul   = 1.8
)

var (
	startLiveModeFunc      = startLiveMode
	newLivePCMRecorderFunc = newLivePCMRecorder
	newLivePCMPlayerFunc   = newPCMPlayerWithSampleRate
)

type liveModeSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	session                    *vai.LiveSession
	sendFrame                  func(types.LiveClientFrame) error
	sendAudio                  func([]byte) error
	recorder                   *livePCMRecorder
	audioMu                    sync.Mutex
	player                     *pcmPlayer
	playerRate                 int
	turnAudioOpen              bool
	negotiatedOutputSampleRate int

	out     io.Writer
	errOut  io.Writer
	history []vai.Message

	outputMu               sync.Mutex
	assistantLineOpen      bool
	audioUnavailableWarned bool

	turns    *vai.LiveTurnTracker
	playback *vai.LivePlaybackReporter

	audioQueueMu     sync.Mutex
	audioQueueCond   *sync.Cond
	audioQueue       []liveAudioPacket
	audioQueueClosed bool

	bargeMu            sync.Mutex
	bargeTurnID        string
	bargeBaselineRMS   float64
	bargeAboveMS       int
	bargeLastTriggered time.Time
	bargeSuppressUntil time.Time

	errMu sync.RWMutex
	err   error

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

type liveAudioPacket struct {
	turnID  string
	rateHz  int
	pcm     []byte
	isFinal bool
}

func (s *liveModeSession) isTurnAudioOpen() bool {
	if s == nil {
		return false
	}
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	return s.turnAudioOpen
}

func startLiveMode(ctx context.Context, cfg chatConfig, state *chatRuntime, tools []vai.ToolWithHandler, out io.Writer, errOut io.Writer) (*liveModeSession, error) {
	if state == nil {
		return nil, errors.New("chat state must not be nil")
	}
	if !cfg.VoiceEnabled {
		return nil, errors.New("live mode requires -voice")
	}
	if strings.TrimSpace(cfg.VoiceID) == "" {
		return nil, errors.New("live mode requires --voice-id")
	}
	if strings.TrimSpace(cfg.ProviderKeys["cartesia"]) == "" {
		return nil, errors.New("live mode requires CARTESIA_API_KEY")
	}

	history := append([]vai.Message(nil), state.history...)
	requestedOutputRate := liveRequestedOutputSampleRate(cfg.LiveOutputRate)
	connectReq, connectOpts := buildLiveConnectRequest(cfg, state.currentModel, history, requestedOutputRate, tools)
	client := vai.NewClient(buildClientOptions(cfg)...)
	liveSession, err := client.Live.Connect(ctx, connectReq, connectOpts)
	if err != nil {
		return nil, err
	}
	started, err := readLiveSessionStartedEvent(liveSession)
	if err != nil {
		_ = liveSession.Close()
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	session := &liveModeSession{
		ctx:                        sessCtx,
		cancel:                     cancel,
		session:                    liveSession,
		out:                        out,
		errOut:                     errOut,
		negotiatedOutputSampleRate: started.OutputSampleRateHz,
		turns:                      vai.NewLiveTurnTracker(),
		playback: vai.NewLivePlaybackReporter(
			liveSession,
			vai.WithLivePlaybackInterval(livePlaybackMarkInterval),
			vai.WithLivePlaybackSafetyMargin(livePlaybackMarkSafetyMargin),
		),
	}
	session.audioQueueCond = sync.NewCond(&session.audioQueueMu)
	if session.out == nil {
		session.out = os.Stdout
	}
	if session.errOut == nil {
		session.errOut = os.Stderr
	}

	if session.negotiatedOutputSampleRate <= 0 {
		session.negotiatedOutputSampleRate = liveClientDefaultOutputSampleRateHz
	}
	if session.negotiatedOutputSampleRate != requestedOutputRate {
		fmt.Fprintf(session.errOut, "live info: negotiated output sample rate %d (requested %d)\n", session.negotiatedOutputSampleRate, requestedOutputRate)
	}
	// Leave player nil here; first audio chunk creates it.
	// This binds playback after mic capture is already active in live mode.
	session.player = nil
	session.playerRate = 0

	recorder, recErr := newLivePCMRecorderFunc(func(chunk []byte) error {
		session.maybeLocalBargeIn(chunk)
		return session.sendBinary(chunk)
	})
	if recErr != nil {
		session.shutdown(false)
		return nil, fmt.Errorf("start live mic stream: %w", recErr)
	}
	session.recorder = recorder

	session.wg.Add(1)
	go session.readerLoop()
	session.wg.Add(1)
	go session.audioPlaybackLoop()
	go func() {
		<-session.ctx.Done()
		session.shutdown(false)
	}()

	return session, nil
}

func buildLiveConnectRequest(cfg chatConfig, model string, history []vai.Message, outputSampleRate int, chatTools []vai.ToolWithHandler) (*vai.LiveConnectRequest, *vai.LiveConnectOptions) {
	requestTools, handlerMap, serverTools := partitionLiveTools(chatTools)
	serverToolConfig := buildLiveServerToolConfig(cfg, serverTools)

	if outputSampleRate <= 0 {
		outputSampleRate = liveClientDefaultOutputSampleRateHz
	}
	voice := vai.VoiceOutput(cfg.VoiceID, vai.WithAudioFormat(vai.AudioFormatPCM))
	if voice != nil && voice.Output != nil {
		voice.Output.SampleRate = outputSampleRate
	}

	req := &vai.LiveConnectRequest{
		Request: types.MessageRequest{
			Model:      model,
			Messages:   append([]types.Message(nil), history...),
			System:     composeSystemPrompt(cfg.SystemPrompt, true),
			MaxTokens:  cfg.MaxTokens,
			Tools:      requestTools,
			ToolChoice: types.ToolChoiceAuto(),
			STTModel:   "cartesia/ink-whisper",
			TTSModel:   "cartesia/sonic-3",
			Voice:      voice,
		},
		Run: types.RunConfig{
			MaxTurns:      liveDefaultRunMaxTurns,
			MaxToolCalls:  liveDefaultRunMaxToolCalls,
			MaxTokens:     0,
			TimeoutMS:     liveDefaultRunTimeoutMS,
			ParallelTools: true,
			ToolTimeoutMS: liveDefaultRunToolTimeoutMS,
		},
		ServerTools: serverTools,
	}
	if len(serverToolConfig) > 0 {
		req.ServerToolConfig = serverToolConfig
	}
	return req, &vai.LiveConnectOptions{ToolHandlers: handlerMap}
}

func buildLiveServerToolConfig(cfg chatConfig, serverTools []string) map[string]any {
	if len(serverTools) == 0 {
		return nil
	}
	serverToolConfig := make(map[string]any, len(serverTools))
	for _, name := range serverTools {
		switch strings.TrimSpace(name) {
		case "vai_web_search", "vai_web_fetch":
			serverToolConfig[name] = map[string]any{"provider": "tavily"}
		case "vai_image":
			if provider, ok := preferredImageToolProvider(cfg); ok {
				serverToolConfig[name] = map[string]any{"provider": string(provider)}
			}
		}
	}
	if len(serverToolConfig) == 0 {
		return nil
	}
	return serverToolConfig
}

func partitionLiveTools(chatTools []vai.ToolWithHandler) ([]types.Tool, map[string]vai.ToolHandler, []string) {
	requestTools := make([]types.Tool, 0, len(chatTools))
	handlers := make(map[string]vai.ToolHandler, len(chatTools))
	serverTools := make([]string, 0, 3)
	seenServerTools := make(map[string]struct{}, 3)

	for _, tool := range chatTools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}

		switch name {
		case "vai_web_search", "vai_web_fetch", "vai_image":
			if _, seen := seenServerTools[name]; !seen {
				serverTools = append(serverTools, name)
				seenServerTools[name] = struct{}{}
			}
			continue
		case "talk_to_user":
			continue
		}

		requestTools = append(requestTools, tool.Tool)
		if tool.Handler != nil {
			handlers[name] = tool.Handler
		}
	}

	return requestTools, handlers, serverTools
}

func liveOutputRateCandidates(ratePolicy string) []int {
	switch strings.TrimSpace(ratePolicy) {
	case "16000":
		return []int{16000}
	case "8000":
		return []int{8000}
	case "24000":
		return []int{24000}
	default:
		return []int{liveClientDefaultOutputSampleRateHz, liveClientFallbackOutputSampleRateHz}
	}
}

func liveRequestedOutputSampleRate(ratePolicy string) int {
	candidates := liveOutputRateCandidates(ratePolicy)
	if len(candidates) == 0 {
		return liveClientDefaultOutputSampleRateHz
	}
	return candidates[0]
}

func readLiveSessionStartedEvent(session *vai.LiveSession) (vai.LiveSessionStartedEvent, error) {
	var started vai.LiveSessionStartedEvent
	if session == nil {
		return started, errors.New("live session is not connected")
	}
	select {
	case event, ok := <-session.Events():
		if !ok {
			if err := session.Err(); err != nil {
				return started, err
			}
			return started, errors.New("live session ended before startup")
		}
		started, ok = event.(vai.LiveSessionStartedEvent)
		if !ok {
			return started, fmt.Errorf("unexpected live startup event %q", event.LiveServerEventType())
		}
		return started, nil
	case <-session.Done():
		if err := session.Err(); err != nil {
			return started, err
		}
		return started, errors.New("live session ended before startup")
	}
}

func (s *liveModeSession) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if s.session != nil {
		return s.session.Done()
	}
	return s.done
}

func (s *liveModeSession) Err() error {
	if s == nil {
		return nil
	}
	if s.session != nil && s.session.Err() != nil {
		return s.session.Err()
	}
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

func (s *liveModeSession) setErr(err error) {
	if s == nil || err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *liveModeSession) HistorySnapshot() []vai.Message {
	if s == nil {
		return nil
	}
	if s.session != nil {
		return s.session.HistorySnapshot()
	}
	return append([]vai.Message(nil), s.history...)
}

func (s *liveModeSession) sendJSON(v any) error {
	if s == nil {
		return errors.New("live session is not connected")
	}
	frame, ok := v.(types.LiveClientFrame)
	if !ok {
		return errors.New("value is not a live client frame")
	}
	if s.session != nil {
		return s.session.SendFrame(frame)
	}
	if s.sendFrame != nil {
		return s.sendFrame(frame)
	}
	return errors.New("live session is not connected")
}

func (s *liveModeSession) sendBinary(data []byte) error {
	if s == nil {
		return errors.New("live session is not connected")
	}
	if len(data) == 0 {
		return nil
	}
	if s.session != nil {
		return s.session.SendAudio(data)
	}
	if s.sendAudio != nil {
		return s.sendAudio(data)
	}
	return errors.New("live session is not connected")
}

func (s *liveModeSession) CommitText(text string) error {
	if s == nil {
		return errors.New("live session is not connected")
	}
	if s.session != nil {
		return s.session.CommitText(text)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return errors.New("text must not be empty")
	}
	return s.sendJSON(types.LiveInputCommitFrame{
		Type:    "input_commit",
		Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: trimmed}},
	})
}

func (s *liveModeSession) Close() error {
	if s == nil {
		return nil
	}
	s.shutdown(true)
	return s.Err()
}

func (s *liveModeSession) shutdown(wait bool) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closeAudioQueue()
		if s.cancel != nil {
			s.cancel()
		}
		if s.session != nil {
			_ = s.session.Close()
		}
		if s.recorder != nil {
			if err := s.recorder.Close(); err != nil {
				s.setErr(err)
			}
		}
		if s.playback != nil {
			s.playback.Close()
		}
		s.audioMu.Lock()
		if s.player != nil {
			closePlayerWithDebug(s.player, "live player close")
			s.player = nil
		}
		s.audioMu.Unlock()
		if s.session == nil && s.done != nil {
			close(s.done)
		}
	})
	if wait {
		s.wg.Wait()
	}
}

func (s *liveModeSession) readerLoop() {
	defer s.wg.Done()
	defer s.shutdown(false)

	for event := range s.session.Events() {
		s.observeTurnEvent(event)
		if err := s.handleServerEvent(event); err != nil {
			s.setErr(err)
			return
		}
	}
}

func (s *liveModeSession) handleServerEvent(event vai.LiveEvent) error {
	switch ev := event.(type) {
	case vai.LiveAssistantTextDeltaEvent:
		if s.shouldIgnoreStreamingTurn(ev.TurnID) {
			return nil
		}
		s.writeAssistantDelta(ev.Text)
	case vai.LiveAudioChunkEvent:
		if s.shouldIgnoreStreamingTurn(ev.TurnID) {
			return nil
		}
		s.handleAudioChunk(ev)
	case vai.LiveToolCallEvent:
		if s.shouldIgnoreStreamingTurn(ev.TurnID) {
			return nil
		}
		s.writeToolExecution(ev.Name)
		return nil
	case vai.LiveUserTurnCommittedEvent:
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (user_turn_committed)", vai.LivePlaybackStateStopped)
		}
	case vai.LiveTurnCompleteEvent:
		if s.shouldIgnoreTerminalTurn(ev.TurnID) {
			return nil
		}
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (turn_complete)", vai.LivePlaybackStateStopped)
		}
		s.history = append([]vai.Message(nil), ev.History...)
	case vai.LiveTurnCancelledEvent:
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.hardStopTurnAudio("live player kill (turn_cancelled)")
		}
	case vai.LiveAudioResetEvent:
		s.resetTurnOutputState()
		s.hardStopTurnAudio("live player kill (audio_reset)")
	case vai.LiveAudioUnavailableEvent:
		if s.shouldIgnoreStreamingTurn(ev.TurnID) {
			return nil
		}
		s.writeAudioUnavailable(ev.Reason, ev.Message)
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (audio_unavailable)", vai.LivePlaybackStateStopped)
		}
	case vai.LiveErrorEvent:
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "live session error"
		}
		if ev.Fatal {
			if strings.TrimSpace(ev.Code) != "" {
				return fmt.Errorf("%s (%s)", msg, ev.Code)
			}
			return errors.New(msg)
		}
		if strings.TrimSpace(ev.Code) != "" {
			fmt.Fprintf(s.errOut, "live warning: %s (%s)\n", msg, ev.Code)
		} else {
			fmt.Fprintf(s.errOut, "live warning: %s\n", msg)
		}
	case vai.LiveSessionStartedEvent:
		// Already consumed during startup. Ignore repeats.
	default:
		fmt.Fprintf(s.errOut, "live warning: unsupported event type %q\n", event.LiveServerEventType())
	}

	return nil
}

func (s *liveModeSession) handleAudioChunk(ev types.LiveAudioChunkEvent) {
	if s == nil {
		return
	}

	format := strings.TrimSpace(strings.ToLower(ev.Format))
	if format != "" && format != "pcm_s16le" {
		fmt.Fprintf(s.errOut, "live audio format warning: unsupported format %q\n", ev.Format)
		return
	}

	rate := ev.SampleRateHz
	if rate <= 0 {
		rate = s.negotiatedOutputSampleRate
	}
	if rate <= 0 {
		rate = liveClientDefaultOutputSampleRateHz
	}

	audioBytes, err := base64.StdEncoding.DecodeString(ev.Audio)
	if err != nil {
		fmt.Fprintf(s.errOut, "live audio decode warning: %v\n", err)
		return
	}
	if len(audioBytes) == 0 {
		return
	}
	s.enqueueAudio(liveAudioPacket{
		turnID:  strings.TrimSpace(ev.TurnID),
		rateHz:  rate,
		pcm:     append([]byte(nil), audioBytes...),
		isFinal: ev.IsFinal,
	})
}

func (s *liveModeSession) finalizeTurnAudio(label string, playbackState vai.LivePlaybackState) {
	if s == nil {
		return
	}
	s.audioMu.Lock()
	if s.player != nil {
		closePlayerWithDebug(s.player, label)
		s.player = nil
	}
	s.playerRate = 0
	s.turnAudioOpen = false
	s.audioMu.Unlock()
	if s.playback == nil {
		return
	}
	switch playbackState {
	case vai.LivePlaybackStateFinished:
		_ = s.playback.FinishTurn()
	case vai.LivePlaybackStateStopped:
		_ = s.playback.StopTurn()
	default:
		_ = s.playback.SendMarkNow()
		s.playback.ClearTurn()
	}
}

func (s *liveModeSession) hardStopTurnAudio(label string) {
	if s == nil {
		return
	}
	s.audioMu.Lock()
	if s.player != nil {
		killPlayerWithDebug(s.player, label)
		s.player = nil
	}
	s.playerRate = 0
	s.turnAudioOpen = false
	s.audioMu.Unlock()
	if s.playback != nil {
		_ = s.playback.StopTurn()
	}
}

func (s *liveModeSession) enqueueAudio(pkt liveAudioPacket) {
	if s == nil {
		return
	}
	if pkt.turnID == "" {
		pkt.turnID = s.currentPlaybackTurn()
		if pkt.turnID == "" {
			pkt.turnID = s.activeTurn()
		}
	}
	if pkt.turnID == "" || len(pkt.pcm) == 0 {
		return
	}

	s.audioQueueMu.Lock()
	if s.audioQueueClosed {
		s.audioQueueMu.Unlock()
		return
	}
	s.audioQueue = append(s.audioQueue, pkt)
	if s.audioQueueCond != nil {
		s.audioQueueCond.Signal()
	}
	s.audioQueueMu.Unlock()
}

func (s *liveModeSession) closeAudioQueue() {
	if s == nil {
		return
	}
	s.audioQueueMu.Lock()
	if s.audioQueueClosed {
		s.audioQueueMu.Unlock()
		return
	}
	s.audioQueueClosed = true
	if s.audioQueueCond != nil {
		s.audioQueueCond.Broadcast()
	}
	s.audioQueueMu.Unlock()
}

func (s *liveModeSession) popAudio() (liveAudioPacket, bool) {
	s.audioQueueMu.Lock()
	defer s.audioQueueMu.Unlock()
	for len(s.audioQueue) == 0 && !s.audioQueueClosed {
		if s.audioQueueCond == nil {
			return liveAudioPacket{}, false
		}
		s.audioQueueCond.Wait()
	}
	if len(s.audioQueue) == 0 {
		return liveAudioPacket{}, false
	}
	pkt := s.audioQueue[0]
	s.audioQueue[0] = liveAudioPacket{}
	s.audioQueue = s.audioQueue[1:]
	return pkt, true
}

func (s *liveModeSession) audioPlaybackLoop() {
	defer s.wg.Done()
	for {
		pkt, ok := s.popAudio()
		if !ok {
			return
		}
		if s.ctx.Err() != nil {
			return
		}
		if s.shouldIgnoreStreamingTurn(pkt.turnID) {
			continue
		}

		s.bargeMu.Lock()
		suppressed := pkt.turnID != "" && pkt.turnID == s.bargeTurnID && !s.bargeSuppressUntil.IsZero() && time.Now().Before(s.bargeSuppressUntil)
		s.bargeMu.Unlock()
		if suppressed {
			// Drop audio during the local suppression window. If the user is actually speaking,
			// the server will send an authoritative audio_reset soon, and we should not resume.
			// If it was a false positive, we allow a small gap and then continue.
			continue
		}

		rate := pkt.rateHz
		if rate <= 0 {
			rate = s.negotiatedOutputSampleRate
		}
		if rate <= 0 {
			rate = liveClientDefaultOutputSampleRateHz
		}

		if s.playback != nil {
			s.playback.StartTurn(pkt.turnID, rate)
		}

		s.audioMu.Lock()
		if s.player == nil || s.playerRate != rate {
			if s.player != nil {
				closePlayerWithDebug(s.player, "live player reconfigure")
			}
			player, err := newLivePCMPlayerFunc(rate)
			if err != nil {
				s.player = nil
				s.playerRate = 0
				s.turnAudioOpen = false
				s.audioMu.Unlock()
				fmt.Fprintf(s.errOut, "live audio player warning: %v\n", err)
				continue
			}
			s.player = player
			s.playerRate = rate
		}
		player := s.player
		s.turnAudioOpen = true
		s.audioMu.Unlock()

		if player == nil {
			continue
		}
		if _, err := player.Write(pkt.pcm); err != nil {
			fmt.Fprintf(s.errOut, "live audio playback warning: %v\n", err)
			continue
		}
		if s.playback != nil {
			s.playback.AddPCMBytes(int64(len(pkt.pcm)))
		}
		if pkt.isFinal {
			s.finalizeTurnAudio("live player close (audio_chunk final)", vai.LivePlaybackStateFinished)
		}
	}
}

func (s *liveModeSession) maybeLocalBargeIn(pcm []byte) {
	if s == nil || len(pcm) == 0 {
		return
	}
	// Only barge-in when we're actually playing assistant audio.
	if !s.isTurnAudioOpen() {
		s.bargeMu.Lock()
		s.bargeTurnID = ""
		s.bargeBaselineRMS = 0
		s.bargeAboveMS = 0
		s.bargeMu.Unlock()
		return
	}

	turnID := s.currentPlaybackTurn()
	if turnID == "" {
		turnID = s.activeTurn()
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	if s.shouldIgnoreStreamingTurn(turnID) {
		return
	}

	rms := rmsS16LE(pcm)
	if rms <= 0 {
		return
	}
	durMS := (len(pcm) * 1000) / (liveMicSampleRateHz * 2)
	if durMS <= 0 {
		durMS = 1
	}

	now := time.Now()
	trigger := false

	s.bargeMu.Lock()
	if s.bargeTurnID != turnID {
		s.bargeTurnID = turnID
		s.bargeBaselineRMS = 0
		s.bargeAboveMS = 0
		s.bargeSuppressUntil = time.Time{}
	}
	baseline := s.bargeBaselineRMS
	if baseline <= 0 {
		baseline = rms
	}
	adaptive := baseline * liveBargeInBaselineMul
	threshold := liveBargeInFixedRMS
	if adaptive > threshold {
		threshold = adaptive
	}

	if rms > threshold {
		s.bargeAboveMS += durMS
	} else {
		s.bargeAboveMS = 0
	}

	// Update baseline after applying the threshold so user speech doesn't immediately raise the bar.
	if s.bargeBaselineRMS <= 0 {
		s.bargeBaselineRMS = rms
	} else {
		s.bargeBaselineRMS = (1.0-liveBargeInBaselineAlpha)*s.bargeBaselineRMS + liveBargeInBaselineAlpha*rms
	}

	if s.bargeAboveMS >= liveBargeInMinAboveMS && now.Sub(s.bargeLastTriggered) >= liveBargeInRefractory {
		s.bargeLastTriggered = now
		s.bargeAboveMS = 0
		s.bargeSuppressUntil = now.Add(liveBargeInSuppressWindow)
		trigger = true
	}
	s.bargeMu.Unlock()

	if !trigger {
		return
	}

	// Send a best-effort playback mark immediately so the server has the freshest played_ms snapshot.
	if s.playback != nil {
		_ = s.playback.SendMarkNow()
	}

	// Hard cut local playback without telling the server playback is "stopped"/"finished"
	// (that would break grace-cancel semantics).
	s.audioMu.Lock()
	if s.player != nil {
		killPlayerWithDebug(s.player, "live player kill (local barge-in)")
		s.player = nil
	}
	s.playerRate = 0
	s.turnAudioOpen = false
	s.audioMu.Unlock()
	if s.playback != nil {
		s.playback.ClearTurn()
	}
}

func rmsS16LE(pcm []byte) float64 {
	// Expect little-endian signed 16-bit PCM mono.
	if len(pcm) < 2 {
		return 0
	}
	var sumSquares float64
	count := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		// Decode int16 without binary.Read overhead.
		v := int16(uint16(pcm[i]) | (uint16(pcm[i+1]) << 8))
		f := float64(v)
		sumSquares += f * f
		count++
	}
	if count == 0 {
		return 0
	}
	mean := sumSquares / float64(count)
	return math.Sqrt(mean)
}

func (s *liveModeSession) writeAssistantDelta(text string) {
	if text == "" {
		return
	}
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	writeLabeledTextDelta(s.out, &s.assistantLineOpen, "assistant: ", text)
}

func (s *liveModeSession) closeOpenLinesLocked() {
	closeOpenLabeledLine(s.out, &s.assistantLineOpen)
}

func (s *liveModeSession) writeAudioUnavailable(reason, message string) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	writeAudioUnavailableWarning(s.errOut, &s.audioUnavailableWarned, reason, message)
}

func (s *liveModeSession) writeToolExecution(name string) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	writeToolExecutionMarker(s.out, name, func() {
		s.closeOpenLinesLocked()
	})
}

func (s *liveModeSession) resetTurnOutputState() {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	s.closeOpenLinesLocked()
	s.audioUnavailableWarned = false
}

func (s *liveModeSession) setActiveTurn(turnID string) {
	if s == nil {
		return
	}
	s.ensureTurnTracker().Observe(vai.LiveUserTurnCommittedEvent{
		Type:   "user_turn_committed",
		TurnID: strings.TrimSpace(turnID),
	})
}

func (s *liveModeSession) activeTurn() string {
	if s == nil {
		return ""
	}
	return s.ensureTurnTracker().ActiveTurnID()
}

func (s *liveModeSession) currentPlaybackTurn() string {
	if s == nil || s.playback == nil {
		return ""
	}
	return s.playback.CurrentTurnID()
}

func (s *liveModeSession) shouldIgnoreStreamingTurn(turnID string) bool {
	if s == nil {
		return false
	}
	return s.ensureTurnTracker().ShouldIgnoreStreamingTurn(turnID)
}

func (s *liveModeSession) shouldIgnoreTerminalTurn(turnID string) bool {
	if s == nil {
		return false
	}
	return s.ensureTurnTracker().ShouldIgnoreTerminalTurn(turnID)
}

func (s *liveModeSession) observeTurnEvent(event vai.LiveEvent) {
	if s == nil || event == nil {
		return
	}
	s.ensureTurnTracker().Observe(event)
}

func (s *liveModeSession) ensureTurnTracker() *vai.LiveTurnTracker {
	if s.turns == nil {
		s.turns = vai.NewLiveTurnTracker()
	}
	return s.turns
}

func syncHistoryFromLiveSession(state *chatRuntime, session *liveModeSession) {
	if state == nil || session == nil {
		return
	}
	state.history = session.HistorySnapshot()
}

func reportClosedLiveSession(session *liveModeSession, errOut io.Writer) {
	if session == nil {
		return
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	if err := session.Err(); err != nil {
		fmt.Fprintf(errOut, "live session ended: %v\n", err)
	}
}

func validateModelForLive(model string, cfg chatConfig) error {
	provider, _, _, err := parseModelRef(model)
	if err != nil {
		return err
	}
	switch provider {
	case "anthropic", "openai", "oai-resp", "gem-dev", "gem-vert", "groq", "cerebras", "openrouter":
	default:
		return fmt.Errorf("unsupported live provider %q", provider)
	}
	if key := strings.TrimSpace(cfg.ProviderKeys[provider]); key == "" {
		if provider == "oai-resp" && strings.TrimSpace(cfg.ProviderKeys["openai"]) != "" {
			return nil
		}
		envHint, _ := requiredKeySpec(provider)
		return fmt.Errorf("missing provider key for %s (set %s)", provider, envHint)
	}
	return nil
}

func isLiveModeOffCommand(line string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(line))
	return trimmed == "/live off"
}

func isLiveModeOnCommand(line string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(line))
	return trimmed == "/live"
}

func maybeCloseFinishedLiveSession(state *chatRuntime, session **liveModeSession, out io.Writer, errOut io.Writer) {
	if session == nil || *session == nil {
		return
	}
	select {
	case <-(*session).Done():
		syncHistoryFromLiveSession(state, *session)
		announceNewImages(out, refreshImageStoreFromHistory(state))
		reportClosedLiveSession(*session, errOut)
		*session = nil
	default:
	}
}
