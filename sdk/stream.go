package vai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

// AudioChunk is a streaming audio payload emitted by Stream.AudioEvents().
type AudioChunk struct {
	Data   []byte
	Format string
}

type streamVoiceOutput struct {
	ttsCtx *tts.StreamingContext
	format string
	voice  string
}

type streamOption func(*Stream)

func withStreamUserTranscript(transcript string) streamOption {
	return func(s *Stream) {
		s.userTranscript = transcript
	}
}

func withStreamVoiceOutput(ttsCtx *tts.StreamingContext, format, voice string) streamOption {
	return func(s *Stream) {
		s.voiceOutput = &streamVoiceOutput{
			ttsCtx: ttsCtx,
			format: format,
			voice:  voice,
		}
	}
}

func withStreamGatewayAudioPassthrough() streamOption {
	return func(s *Stream) {
		s.gatewayAudio = true
	}
}

// Stream wraps a streaming response from the Messages API.
type Stream struct {
	eventStream core.EventStream
	events      <-chan types.StreamEvent
	audioEvents chan AudioChunk
	response    *types.MessageResponse
	err         error
	closed      atomic.Bool
	done        chan struct{}

	userTranscript string
	voiceOutput    *streamVoiceOutput
	gatewayAudio   bool
}

// newStreamFromEventStream creates a Stream from a core.EventStream.
func newStreamFromEventStream(eventStream core.EventStream, opts ...streamOption) *Stream {
	closedAudio := make(chan AudioChunk)
	close(closedAudio)

	s := &Stream{
		eventStream: eventStream,
		done:        make(chan struct{}),
		audioEvents: closedAudio,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.voiceOutput != nil || s.gatewayAudio {
		s.audioEvents = make(chan AudioChunk, 100)
	}

	events := make(chan types.StreamEvent)
	s.events = events

	// Start goroutine to read events
	go s.readEvents(events)

	return s
}

// readEvents reads events from the core event stream and sends them to the channel.
func (s *Stream) readEvents(events chan<- types.StreamEvent) {
	defer close(events)
	defer close(s.done)

	audioEvents := s.audioEvents
	audioEventsOpen := s.voiceOutput != nil || s.gatewayAudio
	var closeAudioOnce sync.Once
	closeAudioEvents := func() {
		if !audioEventsOpen {
			return
		}
		closeAudioOnce.Do(func() {
			close(audioEvents)
		})
	}
	if audioEventsOpen {
		defer closeAudioEvents()
	}

	var response types.MessageResponse
	var currentContent []types.ContentBlock
	toolInputBuffers := make(map[int]string)
	var ttsStream *voice.StreamingTTS
	audioDone := make(chan struct{})

	if s.voiceOutput != nil {
		ttsStream = voice.NewStreamingTTS(s.voiceOutput.ttsCtx, voice.StreamingTTSOptions{BufferAudio: true})
		go func() {
			defer close(audioDone)
			for chunk := range ttsStream.Audio() {
				select {
				case audioEvents <- AudioChunk{Data: chunk, Format: s.voiceOutput.format}:
				case <-s.done:
					return
				}
			}
		}()
	} else {
		close(audioDone)
	}

	applyToolInput := func(index int) {
		raw, ok := toolInputBuffers[index]
		if !ok {
			return
		}
		delete(toolInputBuffers, index)

		if raw == "" || index < 0 || index >= len(currentContent) {
			return
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			// Invalid partial JSON should not fail streaming.
			return
		}

		switch block := currentContent[index].(type) {
		case types.ToolUseBlock:
			block.Input = parsed
			currentContent[index] = block
		case *types.ToolUseBlock:
			if block != nil {
				block.Input = parsed
			}
		case types.ServerToolUseBlock:
			block.Input = parsed
			currentContent[index] = block
		case *types.ServerToolUseBlock:
			if block != nil {
				block.Input = parsed
			}
		}
	}

	voiceAborted := false
	gatewayAudioClosed := false

streamLoop:
	for {
		event, err := s.eventStream.Next()
		if event == nil && err != nil {
			// io.EOF means normal end.
			s.err = err
			break
		}
		if event == nil {
			break
		}

		// Build up the response
		switch e := event.(type) {
		case types.MessageStartEvent:
			response = e.Message
		case types.ContentBlockStartEvent:
			// Ensure we have enough capacity
			for len(currentContent) <= e.Index {
				currentContent = append(currentContent, nil)
			}
			currentContent[e.Index] = e.ContentBlock
		case types.ContentBlockDeltaEvent:
			// Apply delta to current content block
			if e.Index < len(currentContent) {
				currentContent[e.Index] = applyDelta(currentContent[e.Index], e.Delta)
			}

			if s.voiceOutput != nil {
				if textDelta, ok := e.Delta.(types.TextDelta); ok {
					if err := ttsStream.OnTextDelta(textDelta.Text); err != nil {
						voiceAborted = true
						if s.err == nil || errors.Is(s.err, io.EOF) {
							s.err = fmt.Errorf("voice output stream send failed: %w", err)
						}
						_ = s.eventStream.Close()
						break streamLoop
					}
				}
			}
			if inputDelta, ok := e.Delta.(types.InputJSONDelta); ok {
				toolInputBuffers[e.Index] += inputDelta.PartialJSON
			}
		case types.ContentBlockStopEvent:
			// Apply buffered tool input when the block is complete.
			applyToolInput(e.Index)
		case types.MessageDeltaEvent:
			response.StopReason = e.Delta.StopReason
			response.Usage = e.Usage
		case types.AudioChunkEvent:
			if s.gatewayAudio {
				if gatewayAudioClosed {
					break
				}
				audioBytes, err := base64.StdEncoding.DecodeString(e.Audio)
				if err != nil {
					if s.err == nil || errors.Is(s.err, io.EOF) {
						s.err = fmt.Errorf("failed to decode gateway audio chunk: %w", err)
					}
					break
				}
				select {
				case audioEvents <- AudioChunk{Data: audioBytes, Format: e.Format}:
				case <-s.done:
					return
				}
			}
		case types.AudioUnavailableEvent:
			if s.gatewayAudio && !gatewayAudioClosed {
				gatewayAudioClosed = true
				closeAudioEvents()
			}
		}

		// Don't block if stream is closed
		if s.closed.Load() {
			break
		}

		select {
		case events <- event:
		case <-s.done:
			return
		}

		// Some providers may return a terminal event with io.EOF in the same Next() call.
		// Preserve the event, then terminate the loop.
		if err != nil {
			s.err = err
			break
		}
	}

	if s.voiceOutput != nil {
		if !voiceAborted {
			if flushErr := ttsStream.Flush(); flushErr != nil && (s.err == nil || errors.Is(s.err, io.EOF)) {
				s.err = fmt.Errorf("voice output flush failed: %w", flushErr)
			}
		}
		_ = ttsStream.Close()
		<-audioDone
		if err := ttsStream.Err(); err != nil && (s.err == nil || errors.Is(s.err, io.EOF)) {
			s.err = fmt.Errorf("voice output stream failed: %w", err)
		}
	}

	// Some providers don't emit content_block_stop for tool blocks; flush any remaining input.
	for index := range toolInputBuffers {
		applyToolInput(index)
	}

	response.Content = compactContentBlocks(currentContent)
	if s.userTranscript != "" {
		if response.Metadata == nil {
			response.Metadata = make(map[string]any)
		}
		response.Metadata["user_transcript"] = s.userTranscript
	}

	if s.voiceOutput != nil {
		audioBytes := ttsStream.AudioBytes()
		if len(audioBytes) > 0 {
			transcript := strings.TrimSpace(response.TextContent())
			response.Content = append(response.Content, types.AudioBlock{
				Type: "audio",
				Source: types.AudioSource{
					Type:      "base64",
					MediaType: mediaTypeForAudioFormat(s.voiceOutput.format),
					Data:      base64.StdEncoding.EncodeToString(audioBytes),
				},
				Transcript: &transcript,
			})
		}
	}

	s.response = &response
}

// applyDelta applies a delta to a content block.
func applyDelta(block types.ContentBlock, delta types.Delta) types.ContentBlock {
	switch d := delta.(type) {
	case types.TextDelta:
		if tb, ok := block.(types.TextBlock); ok {
			tb.Text += d.Text
			return tb
		}
	case types.ThinkingDelta:
		if tb, ok := block.(types.ThinkingBlock); ok {
			tb.Thinking += d.Thinking
			return tb
		}
	}
	return block
}

// compactContentBlocks removes nil holes caused by sparse stream indices.
// Providers can emit non-zero content block indices (e.g., tool block at index 1
// before any text delta at index 0). Persisting those holes into response content
// produces invalid request history when reused in follow-up turns.
func compactContentBlocks(blocks []types.ContentBlock) []types.ContentBlock {
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

// Events returns the channel of stream events.
func (s *Stream) Events() <-chan types.StreamEvent {
	return s.events
}

// AudioEvents returns a channel of synthesized audio chunks for this stream.
// For non-voice requests this channel is already closed.
func (s *Stream) AudioEvents() <-chan AudioChunk {
	return s.audioEvents
}

// Response returns the final response after the stream ends.
// This will block until the stream is complete.
func (s *Stream) Response() *types.MessageResponse {
	<-s.done
	return s.response
}

// Err returns any error that occurred during streaming.
func (s *Stream) Err() error {
	<-s.done
	return s.err
}

// Close closes the stream and releases resources.
func (s *Stream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		if s.eventStream != nil {
			if err := s.eventStream.Close(); err != nil {
				return err
			}
		}
		if s.voiceOutput != nil && s.voiceOutput.ttsCtx != nil {
			return s.voiceOutput.ttsCtx.Close()
		}
	}
	return nil
}

// TextContent returns the accumulated text content from the stream.
// This will block until the stream is complete.
func (s *Stream) TextContent() string {
	resp := s.Response()
	if resp == nil {
		return ""
	}
	return resp.TextContent()
}
