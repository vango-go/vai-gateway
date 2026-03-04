package vai

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestMessagesRun_InputVoicePreprocessesHistory(t *testing.T) {
	provider := newScriptedProvider("test").withCreateResponses(textResponse("done"))
	svc := newMessagesServiceForMessagesTest(provider)
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{transcripts: []string{"from audio"}}, &fakeSDKTTSProvider{})

	_, err := svc.Run(context.Background(), &MessageRequest{
		Model: "test/model",
		Messages: []Message{{
			Role:    "user",
			Content: ContentBlocks(Audio([]byte("bytes"), "audio/wav")),
		}},
		Voice: VoiceInput(),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(provider.createRequests) == 0 {
		t.Fatalf("expected create request")
	}
	blocks := provider.createRequests[0].Messages[0].ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(blocks))
	}
	if tb, ok := blocks[0].(types.TextBlock); !ok || tb.Text != "from audio" {
		t.Fatalf("expected transcribed text block, got %#v", blocks[0])
	}
}

func TestMessagesRun_OutputVoiceAppendsFinalAudioBlock(t *testing.T) {
	provider := newScriptedProvider("test").withCreateResponses(textResponse("final answer"))
	svc := newMessagesServiceForMessagesTest(provider)
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{}, &fakeSDKTTSProvider{synthAudio: []byte("voice")})

	result, err := svc.Run(context.Background(), &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id"),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Response == nil || result.Response.AudioContent() == nil {
		t.Fatalf("expected final run response to contain synthesized audio block")
	}
}

func TestMessagesRun_VoiceRequestedWithoutPipelineErrors(t *testing.T) {
	provider := newScriptedProvider("test").withCreateResponses(textResponse("done"))
	svc := newMessagesServiceForMessagesTest(provider)

	_, err := svc.Run(context.Background(), &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id"),
	})
	if err == nil {
		t.Fatalf("expected missing Cartesia setup error")
	}
	if !strings.Contains(err.Error(), "Cartesia") {
		t.Fatalf("error = %v, expected Cartesia setup hint", err)
	}
}

func TestRunStream_EmitsAudioChunkEventAndAppendsFinalAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := newScriptedProvider("test", finalTextTurnEventsForRunStream("hello world"))
	svc := newMessagesServiceForRunStreamTest(provider)
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{}, &fakeSDKTTSProvider{})

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id", WithAudioFormat(AudioFormatWAV)),
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	var audioChunks int
	for event := range stream.Events() {
		if _, ok := event.(AudioChunkEvent); ok {
			audioChunks++
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() = %v", err)
	}
	if audioChunks == 0 {
		t.Fatalf("expected at least one AudioChunkEvent")
	}
	result := stream.Result()
	if result == nil || result.Response == nil || result.Response.AudioContent() == nil {
		t.Fatalf("expected final run stream response audio block")
	}
}

func TestRunStream_VoiceWithoutTextDeltas_DoesNotFail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn
	provider := newScriptedProvider("test", []types.StreamEvent{
		types.MessageStartEvent{
			Type: "message_start",
			Message: types.MessageResponse{
				Type:  "message",
				Role:  "assistant",
				Model: "fake-model",
			},
		},
		delta,
		types.MessageStopEvent{Type: "message_stop"},
	})
	svc := newMessagesServiceForRunStreamTest(provider)
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{}, &fakeSDKTTSProvider{failOnEmptyFinal: true})

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id", WithAudioFormat(AudioFormatWAV)),
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	for range stream.Events() {
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() = %v", err)
	}
	result := stream.Result()
	if result == nil || result.Response == nil {
		t.Fatalf("expected non-nil run result and response")
	}
	if result.Response.AudioContent() != nil {
		t.Fatalf("expected no audio block when no spoken text was produced")
	}
}

func TestRunStream_VoiceFallsBackToFinalResponseText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = types.StopReasonEndTurn
	provider := newScriptedProvider("test", []types.StreamEvent{
		types.MessageStartEvent{
			Type: "message_start",
			Message: types.MessageResponse{
				Type:  "message",
				Role:  "assistant",
				Model: "fake-model",
			},
		},
		types.ContentBlockStartEvent{
			Type:         "content_block_start",
			Index:        0,
			ContentBlock: types.TextBlock{Type: "text", Text: "final answer from block start"},
		},
		types.ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: 0,
		},
		delta,
		types.MessageStopEvent{Type: "message_stop"},
	})
	svc := newMessagesServiceForRunStreamTest(provider)
	fakeTTS := &fakeSDKTTSProvider{failOnEmptyFinal: true}
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{}, fakeTTS)

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id", WithAudioFormat(AudioFormatWAV)),
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	for range stream.Events() {
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() = %v", err)
	}
	result := stream.Result()
	if result == nil || result.Response == nil {
		t.Fatalf("expected non-nil run result and response")
	}
	if result.Response.AudioContent() == nil {
		t.Fatalf("expected audio block from final response text fallback")
	}
	fakeTTS.mu.Lock()
	streamedCount := len(fakeTTS.streamedTexts)
	fakeTTS.mu.Unlock()
	if streamedCount == 0 {
		t.Fatalf("expected TTS streamed text calls")
	}
}

func TestRunStream_VoiceRequestedWithoutPipelineErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := newScriptedProvider("test", finalTextTurnEventsForRunStream("hello"))
	svc := newMessagesServiceForRunStreamTest(provider)

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("hello")}},
		Voice:    VoiceOutput("voice-id"),
	})
	if err != nil {
		t.Fatalf("RunStream() setup error = %v", err)
	}
	defer stream.Close()

	if runErr := stream.Err(); runErr == nil {
		t.Fatalf("expected stream error")
	} else if !strings.Contains(runErr.Error(), "Cartesia") {
		t.Fatalf("stream.Err() = %v, expected Cartesia setup hint", runErr)
	}
}

type delayedEventStream struct {
	events []types.StreamEvent
	delay  time.Duration
	index  int
	closed atomic.Bool
}

func (s *delayedEventStream) Next() (types.StreamEvent, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}
	time.Sleep(s.delay)
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	ev := s.events[s.index]
	s.index++
	return ev, nil
}

func (s *delayedEventStream) Close() error {
	s.closed.Store(true)
	return nil
}

type delayedStreamProvider struct {
	name   string
	events []types.StreamEvent
	delay  time.Duration
}

func (p *delayedStreamProvider) Name() string { return p.name }

func (p *delayedStreamProvider) CreateMessage(context.Context, *types.MessageRequest) (*types.MessageResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *delayedStreamProvider) StreamMessage(context.Context, *types.MessageRequest) (core.EventStream, error) {
	return &delayedEventStream{
		events: p.events,
		delay:  p.delay,
	}, nil
}

func (p *delayedStreamProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{
		Tools:         true,
		ToolStreaming: true,
	}
}

func TestRunStream_CancelWithVoice_TerminatesWithoutHanging(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := &delayedStreamProvider{
		name:   "test",
		events: singleTurnTextStream(strings.Repeat("hello ", 20)),
		delay:  40 * time.Millisecond,
	}
	svc := newMessagesServiceForRunStreamTest(provider)
	attachVoicePipelineForSDKTests(svc, &fakeSDKSTTProvider{}, &fakeSDKTTSProvider{})

	stream, err := svc.RunStream(ctx, &MessageRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: Text("start")}},
		Voice:    VoiceOutput("voice-id"),
	})
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	defer stream.Close()

	if err := stream.Cancel(); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	result := stream.Result()
	if result == nil {
		t.Fatalf("expected non-nil result")
	}
	if result.StopReason != RunStopCancelled {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, RunStopCancelled)
	}
}
