package upstream

import (
	"context"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/anthropic"
	"github.com/vango-go/vai-lite/pkg/core/providers/cerebras"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/providers/groq"
	"github.com/vango-go/vai-lite/pkg/core/providers/oai_resp"
	"github.com/vango-go/vai-lite/pkg/core/providers/openai"
	"github.com/vango-go/vai-lite/pkg/core/providers/openrouter"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

// The provider packages define their own ProviderCapabilities/EventStream interfaces to avoid import cycles.
// These adapters normalize them into the core.Provider/core.EventStream interfaces used by the gateway.

type anthropicAdapter struct{ provider *anthropic.Provider }

func (a *anthropicAdapter) Name() string { return a.provider.Name() }
func (a *anthropicAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *anthropicAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &anthropicEventStreamAdapter{stream: stream}, nil
}
func (a *anthropicAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type anthropicEventStreamAdapter struct{ stream anthropic.EventStream }

func (a *anthropicEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *anthropicEventStreamAdapter) Close() error                     { return a.stream.Close() }

type openaiAdapter struct{ provider *openai.Provider }

func (a *openaiAdapter) Name() string { return a.provider.Name() }
func (a *openaiAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *openaiAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &openaiEventStreamAdapter{stream: stream}, nil
}
func (a *openaiAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type openaiEventStreamAdapter struct{ stream openai.EventStream }

func (a *openaiEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *openaiEventStreamAdapter) Close() error                     { return a.stream.Close() }

type oaiRespAdapter struct{ provider *oai_resp.Provider }

func (a *oaiRespAdapter) Name() string { return a.provider.Name() }
func (a *oaiRespAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *oaiRespAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &oaiRespEventStreamAdapter{stream: stream}, nil
}
func (a *oaiRespAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type oaiRespEventStreamAdapter struct{ stream oai_resp.EventStream }

func (a *oaiRespEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *oaiRespEventStreamAdapter) Close() error                     { return a.stream.Close() }

type groqAdapter struct{ provider *groq.Provider }

func (a *groqAdapter) Name() string { return a.provider.Name() }
func (a *groqAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *groqAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &groqEventStreamAdapter{stream: stream}, nil
}
func (a *groqAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type groqEventStreamAdapter struct{ stream groq.EventStream }

func (a *groqEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *groqEventStreamAdapter) Close() error                     { return a.stream.Close() }

type cerebrasAdapter struct{ provider *cerebras.Provider }

func (a *cerebrasAdapter) Name() string { return a.provider.Name() }
func (a *cerebrasAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *cerebrasAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &cerebrasEventStreamAdapter{stream: stream}, nil
}
func (a *cerebrasAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type cerebrasEventStreamAdapter struct{ stream cerebras.EventStream }

func (a *cerebrasEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *cerebrasEventStreamAdapter) Close() error                     { return a.stream.Close() }

type openrouterAdapter struct{ provider *openrouter.Provider }

func (a *openrouterAdapter) Name() string { return a.provider.Name() }
func (a *openrouterAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *openrouterAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &openrouterEventStreamAdapter{stream: stream}, nil
}
func (a *openrouterAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type openrouterEventStreamAdapter struct{ stream openrouter.EventStream }

func (a *openrouterEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *openrouterEventStreamAdapter) Close() error                     { return a.stream.Close() }

type gemAdapter struct{ provider *gem.Provider }

func (a *gemAdapter) Name() string { return a.provider.Name() }
func (a *gemAdapter) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return a.provider.CreateMessage(ctx, req)
}
func (a *gemAdapter) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	stream, err := a.provider.StreamMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &gemEventStreamAdapter{stream: stream}, nil
}
func (a *gemAdapter) Capabilities() core.ProviderCapabilities {
	caps := a.provider.Capabilities()
	return core.ProviderCapabilities{
		Vision:           caps.Vision,
		AudioInput:       caps.AudioInput,
		AudioOutput:      caps.AudioOutput,
		Video:            caps.Video,
		Tools:            caps.Tools,
		ToolStreaming:    caps.ToolStreaming,
		Thinking:         caps.Thinking,
		StructuredOutput: caps.StructuredOutput,
		NativeTools:      caps.NativeTools,
	}
}

type gemEventStreamAdapter struct{ stream gem.EventStream }

func (a *gemEventStreamAdapter) Next() (types.StreamEvent, error) { return a.stream.Next() }
func (a *gemEventStreamAdapter) Close() error                     { return a.stream.Close() }
