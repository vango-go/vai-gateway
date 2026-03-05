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

	liveMu          sync.Mutex
	activeTurnID    string
	audioTurnID     string
	cancelledTurns  map[string]struct{}
	audioResetTurns map[string]struct{}

	playbackMarkCancel   context.CancelFunc
	playbackMarkTurnID   string
	playbackMarkRateHz   int
	playbackMarkStart    time.Time
	playbackMarkBytesPCM int64
	playbackMarkLastSent int

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
		cancelledTurns:             make(map[string]struct{}),
		audioResetTurns:            make(map[string]struct{}),
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
	serverToolConfig := buildLiveServerToolConfig(serverTools)

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
			System:     composeSystemPrompt(cfg.SystemPrompt, false),
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

func buildLiveServerToolConfig(serverTools []string) map[string]any {
	if len(serverTools) == 0 {
		return nil
	}
	cfg := make(map[string]any, len(serverTools))
	for _, name := range serverTools {
		switch strings.TrimSpace(name) {
		case "vai_web_search", "vai_web_fetch":
			cfg[name] = map[string]any{"provider": "tavily"}
		}
	}
	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

func partitionLiveTools(chatTools []vai.ToolWithHandler) ([]types.Tool, map[string]vai.ToolHandler, []string) {
	requestTools := make([]types.Tool, 0, len(chatTools))
	handlers := make(map[string]vai.ToolHandler, len(chatTools))
	serverTools := make([]string, 0, 2)
	seenServerTools := make(map[string]struct{}, 2)

	for _, tool := range chatTools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}

		switch name {
		case "vai_web_search", "vai_web_fetch":
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
		s.setActiveTurn(ev.TurnID)
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (user_turn_committed)", "stopped")
		}
	case vai.LiveTurnCompleteEvent:
		if s.shouldIgnoreTurn(ev.TurnID) {
			return nil
		}
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (turn_complete)", "stopped")
		}
		s.history = append([]vai.Message(nil), ev.History...)
	case vai.LiveTurnCancelledEvent:
		s.markTurnCancelled(ev.TurnID)
		s.resetTurnOutputState()
		if s.isTurnAudioOpen() {
			s.hardStopTurnAudio(ev.TurnID, "live player kill (turn_cancelled)")
		}
	case vai.LiveAudioResetEvent:
		s.markTurnAudioReset(ev.TurnID)
		s.resetTurnOutputState()
		s.hardStopTurnAudio(ev.TurnID, "live player kill (audio_reset)")
	case vai.LiveAudioUnavailableEvent:
		if s.shouldIgnoreStreamingTurn(ev.TurnID) {
			return nil
		}
		s.writeAudioUnavailable(ev.Reason, ev.Message)
		if s.isTurnAudioOpen() {
			s.finalizeTurnAudio("live player close (audio_unavailable)", "stopped")
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
	if strings.TrimSpace(ev.TurnID) != "" {
		s.setAudioTurn(ev.TurnID)
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

func (s *liveModeSession) finalizeTurnAudio(label, playbackState string) {
	if s == nil {
		return
	}
	turnID := s.audioTurn()
	if turnID == "" {
		turnID = s.activeTurn()
	}
	playedMS := s.snapshotPlayedMS(turnID)
	if playedMS >= 0 && turnID != "" {
		_ = s.sendJSON(types.LivePlaybackMarkFrame{
			Type:     "playback_mark",
			TurnID:   turnID,
			PlayedMS: playedMS,
		})
	}
	s.audioMu.Lock()
	if s.player != nil {
		closePlayerWithDebug(s.player, label)
		s.player = nil
	}
	s.playerRate = 0
	s.turnAudioOpen = false
	s.audioMu.Unlock()
	s.stopPlaybackMarkLoop(turnID)
	if playbackState != "" && turnID != "" {
		_ = s.sendJSON(types.LivePlaybackStateFrame{
			Type:   "playback_state",
			TurnID: turnID,
			State:  playbackState,
		})
	}
	s.clearAudioTurn(turnID)
}

func (s *liveModeSession) hardStopTurnAudio(turnID, label string) {
	if s == nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		turnID = s.audioTurn()
	}
	playedMS := s.snapshotPlayedMS(turnID)
	if playedMS >= 0 && turnID != "" {
		_ = s.sendJSON(types.LivePlaybackMarkFrame{
			Type:     "playback_mark",
			TurnID:   turnID,
			PlayedMS: playedMS,
		})
	}
	s.audioMu.Lock()
	if s.player != nil {
		killPlayerWithDebug(s.player, label)
		s.player = nil
	}
	s.playerRate = 0
	s.turnAudioOpen = false
	s.audioMu.Unlock()
	s.stopPlaybackMarkLoop(turnID)
	if turnID != "" {
		_ = s.sendJSON(types.LivePlaybackStateFrame{
			Type:   "playback_state",
			TurnID: turnID,
			State:  "stopped",
		})
	}
	s.clearAudioTurn(turnID)
}

func (s *liveModeSession) enqueueAudio(pkt liveAudioPacket) {
	if s == nil {
		return
	}
	if pkt.turnID == "" {
		pkt.turnID = s.audioTurn()
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

		// Start/maintain playback marks based on PCM successfully written to the player.
		s.ensurePlaybackMarkLoop(pkt.turnID, rate)

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
		s.addPlaybackBytes(pkt.turnID, rate, int64(len(pkt.pcm)))
		if pkt.isFinal {
			s.finalizeTurnAudio("live player close (audio_chunk final)", "finished")
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

	turnID := s.audioTurn()
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
	if playedMS := s.snapshotPlayedMS(turnID); playedMS >= 0 {
		_ = s.sendJSON(types.LivePlaybackMarkFrame{
			Type:     "playback_mark",
			TurnID:   turnID,
			PlayedMS: playedMS,
		})
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
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.cancelledTurns == nil {
		s.cancelledTurns = make(map[string]struct{})
	}
	if s.audioResetTurns == nil {
		s.audioResetTurns = make(map[string]struct{})
	}
	s.activeTurnID = turnID
	delete(s.cancelledTurns, turnID)
	delete(s.audioResetTurns, turnID)
}

func (s *liveModeSession) activeTurn() string {
	if s == nil {
		return ""
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.activeTurnID
}

func (s *liveModeSession) setAudioTurn(turnID string) {
	if s == nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	s.audioTurnID = turnID
}

func (s *liveModeSession) audioTurn() string {
	if s == nil {
		return ""
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.audioTurnID
}

func (s *liveModeSession) clearAudioTurn(turnID string) {
	if s == nil {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if strings.TrimSpace(turnID) == "" || s.audioTurnID == turnID {
		s.audioTurnID = ""
	}
}

func (s *liveModeSession) markTurnCancelled(turnID string) {
	if s == nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.cancelledTurns == nil {
		s.cancelledTurns = make(map[string]struct{})
	}
	s.cancelledTurns[turnID] = struct{}{}
	if s.activeTurnID == turnID {
		s.activeTurnID = ""
	}
	if s.audioTurnID == turnID {
		s.audioTurnID = ""
	}
}

func (s *liveModeSession) markTurnAudioReset(turnID string) {
	if s == nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.audioResetTurns == nil {
		s.audioResetTurns = make(map[string]struct{})
	}
	s.audioResetTurns[turnID] = struct{}{}
}

func (s *liveModeSession) shouldIgnoreTurn(turnID string) bool {
	return s.shouldIgnoreTurnWithMode(turnID, false)
}

func (s *liveModeSession) shouldIgnoreStreamingTurn(turnID string) bool {
	return s.shouldIgnoreTurnWithMode(turnID, true)
}

func (s *liveModeSession) shouldIgnoreTurnWithMode(turnID string, includeAudioReset bool) bool {
	if s == nil {
		return false
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return false
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if _, cancelled := s.cancelledTurns[turnID]; cancelled {
		return true
	}
	if includeAudioReset {
		if _, reset := s.audioResetTurns[turnID]; reset {
			return true
		}
	}
	if strings.TrimSpace(s.activeTurnID) == "" {
		return false
	}
	return turnID != s.activeTurnID
}

func (s *liveModeSession) ensurePlaybackMarkLoop(turnID string, sampleRateHz int) {
	if s == nil || s.ctx == nil || strings.TrimSpace(turnID) == "" || sampleRateHz <= 0 {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	if s.playbackMarkTurnID == turnID && s.playbackMarkRateHz == sampleRateHz && !s.playbackMarkStart.IsZero() {
		return
	}

	if s.playbackMarkCancel != nil {
		s.playbackMarkCancel()
		s.playbackMarkCancel = nil
	}

	ctx, cancel := context.WithCancel(s.ctx)
	s.playbackMarkCancel = cancel
	s.playbackMarkTurnID = turnID
	s.playbackMarkRateHz = sampleRateHz
	s.playbackMarkStart = time.Now()
	s.playbackMarkBytesPCM = 0
	s.playbackMarkLastSent = -1

	go s.playbackMarkLoop(ctx, turnID)
}

func (s *liveModeSession) stopPlaybackMarkLoop(turnID string) {
	if s == nil {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if strings.TrimSpace(turnID) != "" && s.playbackMarkTurnID != "" && turnID != s.playbackMarkTurnID {
		return
	}
	if s.playbackMarkCancel != nil {
		s.playbackMarkCancel()
		s.playbackMarkCancel = nil
	}
	s.playbackMarkTurnID = ""
	s.playbackMarkRateHz = 0
	s.playbackMarkStart = time.Time{}
	s.playbackMarkBytesPCM = 0
	s.playbackMarkLastSent = -1
}

func (s *liveModeSession) addPlaybackBytes(turnID string, sampleRateHz int, bytesPCM int64) {
	if s == nil || strings.TrimSpace(turnID) == "" || bytesPCM <= 0 || sampleRateHz <= 0 {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.playbackMarkTurnID != turnID || s.playbackMarkRateHz != sampleRateHz {
		return
	}
	s.playbackMarkBytesPCM += bytesPCM
}

func (s *liveModeSession) snapshotPlayedMS(turnID string) int {
	if s == nil || strings.TrimSpace(turnID) == "" {
		return -1
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.playbackMarkTurnID != turnID || s.playbackMarkRateHz <= 0 || s.playbackMarkStart.IsZero() {
		return -1
	}
	audioDurationMS := int((s.playbackMarkBytesPCM * 1000) / int64(s.playbackMarkRateHz*2))
	elapsedMS := int(time.Since(s.playbackMarkStart).Milliseconds())
	playedMS := elapsedMS
	if audioDurationMS < playedMS {
		playedMS = audioDurationMS
	}
	playedMS -= int(livePlaybackMarkSafetyMargin.Milliseconds())
	if playedMS < 0 {
		playedMS = 0
	}
	// Monotonic best-effort.
	if playedMS < s.playbackMarkLastSent {
		playedMS = s.playbackMarkLastSent
	}
	return playedMS
}

func (s *liveModeSession) playbackMarkLoop(ctx context.Context, turnID string) {
	if s == nil || strings.TrimSpace(turnID) == "" {
		return
	}
	ticker := time.NewTicker(livePlaybackMarkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			playedMS := s.snapshotPlayedMS(turnID)
			if playedMS < 0 {
				continue
			}
			s.liveMu.Lock()
			if s.playbackMarkTurnID != turnID {
				s.liveMu.Unlock()
				return
			}
			if playedMS <= s.playbackMarkLastSent {
				s.liveMu.Unlock()
				continue
			}
			s.playbackMarkLastSent = playedMS
			s.liveMu.Unlock()

			_ = s.sendJSON(types.LivePlaybackMarkFrame{
				Type:     "playback_mark",
				TurnID:   turnID,
				PlayedMS: playedMS,
			})
		}
	}
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

func maybeCloseFinishedLiveSession(state *chatRuntime, session **liveModeSession, errOut io.Writer) {
	if session == nil || *session == nil {
		return
	}
	select {
	case <-(*session).Done():
		syncHistoryFromLiveSession(state, *session)
		reportClosedLiveSession(*session, errOut)
		*session = nil
	default:
	}
}
