package vai

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

type fakeSDKSTTProvider struct {
	transcripts []string
	err         error
	calls       int
}

func (f *fakeSDKSTTProvider) Name() string { return "fake-stt" }

func (f *fakeSDKSTTProvider) Transcribe(_ context.Context, _ io.Reader, _ stt.TranscribeOptions) (*stt.Transcript, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.calls >= len(f.transcripts) {
		return &stt.Transcript{Text: ""}, nil
	}
	text := f.transcripts[f.calls]
	f.calls++
	return &stt.Transcript{Text: text}, nil
}

func (f *fakeSDKSTTProvider) TranscribeStream(context.Context, io.Reader, stt.TranscribeOptions) (<-chan stt.TranscriptDelta, error) {
	return nil, nil
}

func (f *fakeSDKSTTProvider) NewStreamingSTT(context.Context, stt.TranscribeOptions) (*stt.StreamingSTT, error) {
	return nil, nil
}

type fakeSDKTTSProvider struct {
	mu sync.Mutex

	synthAudio []byte
	synthErr   error

	streamSendErr    error
	newStreamErr     error
	failOnEmptyFinal bool

	synthCalls    int
	streamedTexts []string
}

func (f *fakeSDKTTSProvider) Name() string { return "fake-tts" }

func (f *fakeSDKTTSProvider) Synthesize(_ context.Context, _ string, opts tts.SynthesizeOptions) (*tts.Synthesis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synthCalls++
	if f.synthErr != nil {
		return nil, f.synthErr
	}
	format := opts.Format
	if format == "" {
		format = "wav"
	}
	return &tts.Synthesis{Audio: f.synthAudio, Format: format}, nil
}

func (f *fakeSDKTTSProvider) SynthesizeStream(context.Context, string, tts.SynthesizeOptions) (*tts.SynthesisStream, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeSDKTTSProvider) NewStreamingContext(_ context.Context, _ tts.StreamingContextOptions) (*tts.StreamingContext, error) {
	if f.newStreamErr != nil {
		return nil, f.newStreamErr
	}

	sc := tts.NewStreamingContext()
	var closeOnce sync.Once

	sc.SendFunc = func(text string, isFinal bool) error {
		f.mu.Lock()
		f.streamedTexts = append(f.streamedTexts, text)
		f.mu.Unlock()

		if f.streamSendErr != nil {
			sc.SetError(f.streamSendErr)
			closeOnce.Do(func() { sc.FinishAudio() })
			return f.streamSendErr
		}

		trimmed := strings.TrimSpace(text)
		if isFinal && trimmed == "" && f.failOnEmptyFinal {
			err := errors.New("invalid transcript: empty")
			sc.SetError(err)
			closeOnce.Do(func() { sc.FinishAudio() })
			return err
		}
		if trimmed != "" {
			sc.PushAudio([]byte("aud:" + trimmed))
		}
		if isFinal {
			closeOnce.Do(func() { sc.FinishAudio() })
		}
		return nil
	}

	sc.CloseFunc = func() error {
		closeOnce.Do(func() { sc.FinishAudio() })
		return nil
	}

	return sc, nil
}

func attachVoicePipelineForSDKTests(svc *MessagesService, sttProvider stt.Provider, ttsProvider tts.Provider) {
	svc.client.voicePipeline = voice.NewPipelineWithProviders(sttProvider, ttsProvider)
}
