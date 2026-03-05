package vai

import (
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestLiveTurnTracker(t *testing.T) {
	t.Run("user turn committed sets active turn", func(t *testing.T) {
		tracker := NewLiveTurnTracker()
		tracker.Observe(LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_1"})
		if got := tracker.ActiveTurnID(); got != "turn_1" {
			t.Fatalf("ActiveTurnID=%q, want turn_1", got)
		}
	})

	t.Run("audio reset suppresses later streaming events", func(t *testing.T) {
		tracker := NewLiveTurnTracker()
		tracker.Observe(LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_1"})
		tracker.Observe(LiveAudioResetEvent{Type: "audio_reset", TurnID: "turn_1"})
		if !tracker.ShouldIgnoreStreamingTurn("turn_1") {
			t.Fatal("expected reset turn streaming events to be ignored")
		}
		if tracker.ShouldIgnoreTerminalTurn("turn_1") {
			t.Fatal("expected turn_complete for reset turn to remain allowed")
		}
	})

	t.Run("turn cancelled suppresses streaming and terminal events", func(t *testing.T) {
		tracker := NewLiveTurnTracker()
		tracker.Observe(LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_1"})
		tracker.Observe(LiveTurnCancelledEvent{Type: "turn_cancelled", TurnID: "turn_1"})
		if !tracker.ShouldIgnoreStreamingTurn("turn_1") {
			t.Fatal("expected cancelled streaming events to be ignored")
		}
		if !tracker.ShouldIgnoreTerminalTurn("turn_1") {
			t.Fatal("expected cancelled terminal events to be ignored")
		}
	})

	t.Run("mismatched active turn streaming events are ignored", func(t *testing.T) {
		tracker := NewLiveTurnTracker()
		tracker.Observe(LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_2"})
		if !tracker.ShouldIgnoreStreamingTurn("turn_1") {
			t.Fatal("expected stale streaming event to be ignored")
		}
		if tracker.ShouldIgnoreStreamingTurn("turn_2") {
			t.Fatal("did not expect active turn to be ignored")
		}
	})

	t.Run("should ignore dispatches by event type", func(t *testing.T) {
		tracker := NewLiveTurnTracker()
		tracker.Observe(LiveUserTurnCommittedEvent{Type: "user_turn_committed", TurnID: "turn_1"})
		tracker.Observe(LiveAudioResetEvent{Type: "audio_reset", TurnID: "turn_1"})
		if !tracker.ShouldIgnore(LiveAudioChunkEvent{Type: "audio_chunk", TurnID: "turn_1"}) {
			t.Fatal("expected audio chunk to be ignored")
		}
		if tracker.ShouldIgnore(LiveTurnCompleteEvent{Type: "turn_complete", TurnID: "turn_1"}) {
			t.Fatal("did not expect turn_complete to be ignored after audio_reset")
		}
	})
}

func TestLivePlaybackReporter(t *testing.T) {
	t.Run("finish turn sends final mark and finished state", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 4)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		time.Sleep(10 * time.Millisecond)

		if err := reporter.FinishTurn(); err != nil {
			t.Fatalf("FinishTurn error: %v", err)
		}

		mark := <-frames
		if _, ok := mark.(types.LivePlaybackMarkFrame); !ok {
			t.Fatalf("first frame=%T, want LivePlaybackMarkFrame", mark)
		}
		state := <-frames
		gotState, ok := state.(types.LivePlaybackStateFrame)
		if !ok {
			t.Fatalf("second frame=%T, want LivePlaybackStateFrame", state)
		}
		if gotState.State != string(LivePlaybackStateFinished) {
			t.Fatalf("state=%q, want finished", gotState.State)
		}
	})

	t.Run("stop turn sends final mark and stopped state", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 4)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		time.Sleep(10 * time.Millisecond)

		if err := reporter.StopTurn(); err != nil {
			t.Fatalf("StopTurn error: %v", err)
		}

		<-frames
		state := <-frames
		gotState := state.(types.LivePlaybackStateFrame)
		if gotState.State != string(LivePlaybackStateStopped) {
			t.Fatalf("state=%q, want stopped", gotState.State)
		}
	})

	t.Run("clear turn stops tracking without sending playback state", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 4)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackInterval(10*time.Millisecond), WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		reporter.ClearTurn()
		time.Sleep(25 * time.Millisecond)
		select {
		case frame := <-frames:
			t.Fatalf("unexpected frame after ClearTurn: %T", frame)
		default:
		}
	})

	t.Run("send mark now works mid turn", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 2)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		time.Sleep(10 * time.Millisecond)

		if err := reporter.SendMarkNow(); err != nil {
			t.Fatalf("SendMarkNow error: %v", err)
		}
		frame := <-frames
		if _, ok := frame.(types.LivePlaybackMarkFrame); !ok {
			t.Fatalf("frame=%T, want LivePlaybackMarkFrame", frame)
		}
	})

	t.Run("periodic marks are monotonic", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 8)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackInterval(10*time.Millisecond), WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		defer reporter.Close()

		var marks []types.LivePlaybackMarkFrame
		for len(marks) < 2 {
			select {
			case frame := <-frames:
				if mark, ok := frame.(types.LivePlaybackMarkFrame); ok {
					marks = append(marks, mark)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for periodic playback marks")
			}
		}
		if marks[1].PlayedMS <= marks[0].PlayedMS {
			t.Fatalf("played_ms not monotonic: %d then %d", marks[0].PlayedMS, marks[1].PlayedMS)
		}
	})

	t.Run("starting new turn resets prior tracking state", func(t *testing.T) {
		frames := make(chan LiveClientFrame, 4)
		session := &LiveSession{
			sendFrameFn: func(frame LiveClientFrame) error {
				frames <- frame
				return nil
			},
		}
		reporter := NewLivePlaybackReporter(session, WithLivePlaybackSafetyMargin(0))
		reporter.StartTurn("turn_1", 16000)
		reporter.AddPCMBytes(int64(16000 * 2))
		reporter.StartTurn("turn_2", 24000)
		if got := reporter.CurrentTurnID(); got != "turn_2" {
			t.Fatalf("CurrentTurnID=%q, want turn_2", got)
		}
		if played := reporter.SnapshotPlayedMS(); played != 0 {
			t.Fatalf("SnapshotPlayedMS=%d, want 0 after reset", played)
		}
		reporter.Close()
	})
}
