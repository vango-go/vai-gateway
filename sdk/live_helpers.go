package vai

import (
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	defaultLivePlaybackInterval     = 250 * time.Millisecond
	defaultLivePlaybackSafetyMargin = 100 * time.Millisecond
)

// LiveTurnTracker tracks the current live turn and superseded-turn suppression state.
type LiveTurnTracker struct {
	mu              sync.RWMutex
	activeTurnID    string
	cancelledTurns  map[string]struct{}
	audioResetTurns map[string]struct{}
}

// NewLiveTurnTracker creates a new live turn tracker.
func NewLiveTurnTracker() *LiveTurnTracker {
	return &LiveTurnTracker{
		cancelledTurns:  make(map[string]struct{}),
		audioResetTurns: make(map[string]struct{}),
	}
}

// Observe updates tracker state from a live event.
func (t *LiveTurnTracker) Observe(event LiveEvent) {
	if t == nil || event == nil {
		return
	}

	switch e := event.(type) {
	case LiveUserTurnCommittedEvent:
		turnID := strings.TrimSpace(e.TurnID)
		if turnID == "" {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ensureMapsLocked()
		t.activeTurnID = turnID
		delete(t.cancelledTurns, turnID)
		delete(t.audioResetTurns, turnID)
	case LiveTurnCancelledEvent:
		turnID := strings.TrimSpace(e.TurnID)
		if turnID == "" {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ensureMapsLocked()
		t.cancelledTurns[turnID] = struct{}{}
		if t.activeTurnID == turnID {
			t.activeTurnID = ""
		}
	case LiveAudioResetEvent:
		turnID := strings.TrimSpace(e.TurnID)
		if turnID == "" {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ensureMapsLocked()
		t.audioResetTurns[turnID] = struct{}{}
	}
}

// ActiveTurnID returns the currently active user turn, if any.
func (t *LiveTurnTracker) ActiveTurnID() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.activeTurnID
}

// ShouldIgnore returns whether the event should be ignored based on current tracker state.
func (t *LiveTurnTracker) ShouldIgnore(event LiveEvent) bool {
	if t == nil || event == nil {
		return false
	}

	switch e := event.(type) {
	case LiveAssistantTextDeltaEvent:
		return t.ShouldIgnoreStreamingTurn(e.TurnID)
	case LiveAudioChunkEvent:
		return t.ShouldIgnoreStreamingTurn(e.TurnID)
	case LiveToolCallEvent:
		return t.ShouldIgnoreStreamingTurn(e.TurnID)
	case LiveAudioUnavailableEvent:
		return t.ShouldIgnoreStreamingTurn(e.TurnID)
	case LiveTurnCompleteEvent:
		return t.ShouldIgnoreTerminalTurn(e.TurnID)
	default:
		return false
	}
}

// ShouldIgnoreStreamingTurn returns whether a streaming event for turnID should be ignored.
func (t *LiveTurnTracker) ShouldIgnoreStreamingTurn(turnID string) bool {
	return t.shouldIgnoreTurn(turnID, true)
}

// ShouldIgnoreTerminalTurn returns whether a terminal event for turnID should be ignored.
func (t *LiveTurnTracker) ShouldIgnoreTerminalTurn(turnID string) bool {
	return t.shouldIgnoreTurn(turnID, false)
}

// Reset clears all tracked turn state.
func (t *LiveTurnTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeTurnID = ""
	t.cancelledTurns = make(map[string]struct{})
	t.audioResetTurns = make(map[string]struct{})
}

func (t *LiveTurnTracker) shouldIgnoreTurn(turnID string, includeAudioReset bool) bool {
	if t == nil {
		return false
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if _, cancelled := t.cancelledTurns[turnID]; cancelled {
		return true
	}
	if includeAudioReset {
		if _, reset := t.audioResetTurns[turnID]; reset {
			return true
		}
	}
	if strings.TrimSpace(t.activeTurnID) == "" {
		return false
	}
	return turnID != t.activeTurnID
}

func (t *LiveTurnTracker) ensureMapsLocked() {
	if t.cancelledTurns == nil {
		t.cancelledTurns = make(map[string]struct{})
	}
	if t.audioResetTurns == nil {
		t.audioResetTurns = make(map[string]struct{})
	}
}

// LivePlaybackReporterOptions configures playback mark scheduling.
type LivePlaybackReporterOptions struct {
	Interval     time.Duration
	SafetyMargin time.Duration
}

// LivePlaybackReporterOption configures a live playback reporter.
type LivePlaybackReporterOption func(*LivePlaybackReporterOptions)

// WithLivePlaybackInterval overrides the default periodic playback mark interval.
func WithLivePlaybackInterval(d time.Duration) LivePlaybackReporterOption {
	return func(opts *LivePlaybackReporterOptions) {
		if d > 0 {
			opts.Interval = d
		}
	}
}

// WithLivePlaybackSafetyMargin overrides the default playback safety margin.
func WithLivePlaybackSafetyMargin(d time.Duration) LivePlaybackReporterOption {
	return func(opts *LivePlaybackReporterOptions) {
		if d >= 0 {
			opts.SafetyMargin = d
		}
	}
}

// LivePlaybackReporter tracks played PCM and emits playback marks/states for one live turn at a time.
type LivePlaybackReporter struct {
	session *LiveSession

	mu           sync.Mutex
	interval     time.Duration
	safetyMargin time.Duration
	cancel       contextCancelFunc
	turnID       string
	sampleRateHz int
	start        time.Time
	bytesPCM     int64
	lastSent     int
}

type contextCancelFunc func()

// NewLivePlaybackReporter creates a new playback reporter for a live session.
func NewLivePlaybackReporter(session *LiveSession, opts ...LivePlaybackReporterOption) *LivePlaybackReporter {
	cfg := LivePlaybackReporterOptions{
		Interval:     defaultLivePlaybackInterval,
		SafetyMargin: defaultLivePlaybackSafetyMargin,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultLivePlaybackInterval
	}
	if cfg.SafetyMargin < 0 {
		cfg.SafetyMargin = 0
	}
	return &LivePlaybackReporter{
		session:      session,
		interval:     cfg.Interval,
		safetyMargin: cfg.SafetyMargin,
		lastSent:     -1,
	}
}

// StartTurn begins playback tracking for a turn and starts periodic playback marks.
func (r *LivePlaybackReporter) StartTurn(turnID string, sampleRateHz int) {
	if r == nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" || sampleRateHz <= 0 {
		r.ClearTurn()
		return
	}

	r.mu.Lock()
	if r.turnID == turnID && r.sampleRateHz == sampleRateHz && !r.start.IsZero() {
		r.mu.Unlock()
		return
	}
	r.clearLocked()
	r.turnID = turnID
	r.sampleRateHz = sampleRateHz
	r.start = time.Now()
	r.bytesPCM = 0
	r.lastSent = -1
	stopCh := make(chan struct{})
	r.cancel = func() { close(stopCh) }
	interval := r.interval
	r.mu.Unlock()

	go r.loop(turnID, stopCh, interval)
}

// AddPCMBytes records successfully played PCM bytes for the current turn.
func (r *LivePlaybackReporter) AddPCMBytes(bytesPCM int64) {
	if r == nil || bytesPCM <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(r.turnID) == "" || r.sampleRateHz <= 0 {
		return
	}
	r.bytesPCM += bytesPCM
}

// SendMarkNow sends an immediate best-effort playback mark for the current turn.
func (r *LivePlaybackReporter) SendMarkNow() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	turnID, playedMS, ok := r.nextMarkLocked()
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return r.sendMark(turnID, playedMS)
}

// SnapshotPlayedMS returns the current best-effort played_ms estimate.
func (r *LivePlaybackReporter) SnapshotPlayedMS() int {
	if r == nil {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotPlayedMSLocked()
}

// CurrentTurnID returns the currently tracked playback turn, if any.
func (r *LivePlaybackReporter) CurrentTurnID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnID
}

// FinishTurn sends a final mark and playback_state=finished, then clears tracking state.
func (r *LivePlaybackReporter) FinishTurn() error {
	return r.finishWithState(LivePlaybackStateFinished)
}

// StopTurn sends a final mark and playback_state=stopped, then clears tracking state.
func (r *LivePlaybackReporter) StopTurn() error {
	return r.finishWithState(LivePlaybackStateStopped)
}

// ClearTurn stops tracking without sending playback_state.
func (r *LivePlaybackReporter) ClearTurn() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
}

// Close stops playback tracking without sending terminal playback frames.
func (r *LivePlaybackReporter) Close() {
	r.ClearTurn()
}

func (r *LivePlaybackReporter) finishWithState(state LivePlaybackState) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	turnID := strings.TrimSpace(r.turnID)
	r.mu.Unlock()
	if turnID == "" {
		return nil
	}

	var firstErr error
	if err := r.SendMarkNow(); err != nil {
		firstErr = err
	}
	if err := r.sendState(turnID, state); err != nil && firstErr == nil {
		firstErr = err
	}
	r.ClearTurn()
	return firstErr
}

func (r *LivePlaybackReporter) nextMarkLocked() (string, int, bool) {
	turnID := strings.TrimSpace(r.turnID)
	if turnID == "" {
		return "", 0, false
	}
	playedMS := r.snapshotPlayedMSLocked()
	if playedMS < 0 {
		return "", 0, false
	}
	if playedMS <= r.lastSent {
		return "", 0, false
	}
	r.lastSent = playedMS
	return turnID, playedMS, true
}

func (r *LivePlaybackReporter) snapshotPlayedMSLocked() int {
	if strings.TrimSpace(r.turnID) == "" || r.sampleRateHz <= 0 || r.start.IsZero() {
		return -1
	}

	audioDurationMS := int((r.bytesPCM * 1000) / int64(r.sampleRateHz*2))
	elapsedMS := int(time.Since(r.start).Milliseconds())
	playedMS := elapsedMS
	if audioDurationMS < playedMS {
		playedMS = audioDurationMS
	}
	playedMS -= int(r.safetyMargin.Milliseconds())
	if playedMS < 0 {
		playedMS = 0
	}
	if playedMS < r.lastSent {
		playedMS = r.lastSent
	}
	return playedMS
}

func (r *LivePlaybackReporter) clearLocked() {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.turnID = ""
	r.sampleRateHz = 0
	r.start = time.Time{}
	r.bytesPCM = 0
	r.lastSent = -1
}

func (r *LivePlaybackReporter) loop(turnID string, stopCh <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			currentTurnID, playedMS, ok := r.nextMarkLocked()
			r.mu.Unlock()
			if !ok || currentTurnID != turnID {
				continue
			}
			_ = r.sendMark(currentTurnID, playedMS)
		}
	}
}

func (r *LivePlaybackReporter) sendMark(turnID string, playedMS int) error {
	if r == nil {
		return nil
	}
	if r.session == nil {
		return errors.New("live session is not initialized")
	}
	return r.session.SendPlaybackMark(turnID, playedMS)
}

func (r *LivePlaybackReporter) sendState(turnID string, state LivePlaybackState) error {
	if r == nil {
		return nil
	}
	if r.session == nil {
		return errors.New("live session is not initialized")
	}
	return r.session.SendPlaybackState(turnID, state)
}
