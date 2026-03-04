package gem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"iter"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	providerVert = "gem-vert"
	providerDev  = "gem-dev"
)

// ProviderCapabilities describes what a provider supports.
// This local copy avoids import cycles with pkg/core.
type ProviderCapabilities struct {
	Vision           bool
	AudioInput       bool
	AudioOutput      bool
	Video            bool
	Tools            bool
	ToolStreaming    bool
	Thinking         bool
	StructuredOutput bool
	NativeTools      []string
}

// EventStream is an iterator over provider stream events.
type EventStream interface {
	Next() (types.StreamEvent, error)
	Close() error
}

type backendMode int

const (
	backendModeVertex backendMode = iota
	backendModeDeveloper
)

// Provider implements Gemini (Vertex + Developer API) on the GenAI SDK.
type Provider struct {
	name    string
	backend genai.Backend
	mode    backendMode
	apiKey  string

	httpClient *http.Client
	baseURL    string

	clientMu sync.Mutex
	client   *genai.Client

	// first-request initialization data for API-keyless Vertex mode
	vertexProject  string
	vertexLocation string

	streamSeq uint64

	streamArgsMu             sync.RWMutex
	streamArgsSupportByModel map[string]streamArgsSupport
}

type streamArgsSupport uint8

const (
	streamArgsSupportUnknown streamArgsSupport = iota
	streamArgsSupportSupported
	streamArgsSupportUnsupported
)

const streamArgsFallbackWarning = "stream_function_call_arguments disabled after Vertex INVALID_ARGUMENT; using non-streamed tool args for this model"

func newProvider(name string, mode backendMode, apiKey string, opts ...Option) *Provider {
	p := &Provider{
		name:                     name,
		mode:                     mode,
		apiKey:                   apiKey,
		backend:                  genai.BackendGeminiAPI,
		streamArgsSupportByModel: make(map[string]streamArgsSupport),
	}
	if mode == backendModeVertex {
		p.backend = genai.BackendVertexAI
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return p.name }

// Capabilities returns provider-level capabilities.
func (p *Provider) Capabilities() ProviderCapabilities {
	caps := ProviderCapabilities{
		Vision:           true,
		AudioInput:       true,
		AudioOutput:      true,
		Video:            true,
		Tools:            true,
		Thinking:         true,
		StructuredOutput: true,
		NativeTools:      []string{"google_search", "code_execution", "image_generation"},
	}
	caps.ToolStreaming = p.mode == backendModeVertex
	return caps
}

// CreateMessage executes a non-streaming request.
func (p *Provider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("request must not be nil")
	}
	build, err := p.buildGenerateContentRequest(req, defaultBuildRequestOptions())
	if err != nil {
		return nil, err
	}

	client, err := p.getClient(ctx, build.ext)
	if err != nil {
		return nil, err
	}

	resp, err := client.Models.GenerateContent(ctx, req.Model, build.contents, build.config)
	if err != nil {
		return nil, mapError(err)
	}

	out, err := p.translateGenerateContentResponse(req.Model, resp, build)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StreamMessage executes a streaming request.
func (p *Provider) StreamMessage(ctx context.Context, req *types.MessageRequest) (EventStream, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("request must not be nil")
	}
	buildOpts := buildRequestOptions{isStream: true}
	if p.mode == backendModeVertex {
		if p.streamArgsSupportForModel(req.Model) == streamArgsSupportUnsupported {
			disabled := false
			buildOpts.autoStreamArgsOverride = &disabled
		}
	}
	build, err := p.buildGenerateContentRequest(req, buildOpts)
	if err != nil {
		return nil, err
	}

	client, err := p.getClient(ctx, build.ext)
	if err != nil {
		return nil, err
	}

	attemptedFallback := false
	for {
		nextFn, stopFn, primed := p.openPrimedStream(ctx, client, req.Model, build)
		if primed.hasErr() {
			mappedErr := mapError(primed.err)
			stopFn()
			if p.shouldFallbackStreamArgs(build, mappedErr, attemptedFallback) {
				attemptedFallback = true
				disabled := false
				fallbackBuild, buildErr := p.buildGenerateContentRequest(req, buildRequestOptions{
					isStream:               true,
					autoStreamArgsOverride: &disabled,
				})
				if buildErr != nil {
					return nil, buildErr
				}
				fallbackBuild.warnings = append(fallbackBuild.warnings, streamArgsFallbackWarning)
				build = fallbackBuild
				continue
			}
			return nil, mappedErr
		}

		if p.mode == backendModeVertex && build.streamArgsAuto {
			if build.streamArgsEnabled {
				p.setStreamArgsSupportForModel(req.Model, streamArgsSupportSupported)
			} else {
				p.setStreamArgsSupportForModel(req.Model, streamArgsSupportUnsupported)
			}
		}

		streamID := atomic.AddUint64(&p.streamSeq, 1)
		return newEventStreamFromPull(ctx, p, req.Model, streamID, nextFn, stopFn, build, primed), nil
	}
}

func (p *Provider) prefixedModel(model string) string {
	return fmt.Sprintf("%s/%s", p.name, model)
}

func (p *Provider) openPrimedStream(
	ctx context.Context,
	client *genai.Client,
	model string,
	build *requestBuild,
) (func() (*genai.GenerateContentResponse, error, bool), func(), streamPrimedResult) {
	seq := client.Models.GenerateContentStream(ctx, model, build.contents, build.config)
	nextFn, stopFn := iter.Pull2(seq)
	resp, iterErr, ok := nextFn()
	return nextFn, stopFn, streamPrimedResult{
		has:  true,
		resp: resp,
		err:  iterErr,
		ok:   ok,
	}
}

func (p *Provider) shouldFallbackStreamArgs(build *requestBuild, err error, attempted bool) bool {
	if attempted || p.mode != backendModeVertex || build == nil {
		return false
	}
	if !build.streamArgsAuto || !build.streamArgsEnabled {
		return false
	}
	var gemErr *Error
	if !errors.As(err, &gemErr) || gemErr == nil {
		return false
	}
	if gemErr.Type != ErrInvalidRequest {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(gemErr.Code), "INVALID_ARGUMENT")
}

func (p *Provider) streamArgsSupportForModel(model string) streamArgsSupport {
	key := normalizeStreamArgsModelKey(model)
	if key == "" {
		return streamArgsSupportUnknown
	}
	p.streamArgsMu.RLock()
	defer p.streamArgsMu.RUnlock()
	if st, ok := p.streamArgsSupportByModel[key]; ok {
		return st
	}
	return streamArgsSupportUnknown
}

func (p *Provider) setStreamArgsSupportForModel(model string, st streamArgsSupport) {
	key := normalizeStreamArgsModelKey(model)
	if key == "" {
		return
	}
	p.streamArgsMu.Lock()
	defer p.streamArgsMu.Unlock()
	p.streamArgsSupportByModel[key] = st
}

func normalizeStreamArgsModelKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
