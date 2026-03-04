// Package vai provides the Vai SDK for Go.
//
// This repo is intentionally trimmed down to focus on `Run` / `RunStream` and tools.
// It supports:
//   - direct mode (SDK calls providers directly), and
//   - gateway proxy mode (SDK calls /v1/messages and /v1/runs endpoints).
package vai

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/anthropic"
	"github.com/vango-go/vai-lite/pkg/core/providers/cerebras"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/providers/groq"
	"github.com/vango-go/vai-lite/pkg/core/providers/oai_resp"
	"github.com/vango-go/vai-lite/pkg/core/providers/openai"
	"github.com/vango-go/vai-lite/pkg/core/providers/openrouter"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

// Client is the main entry point for the SDK.
type Client struct {
	Messages *MessagesService
	Runs     *RunsService

	// Internal
	core          *core.Engine
	providerKeys  map[string]string
	logger        *slog.Logger
	voicePipeline *voice.Pipeline
	httpClient    *http.Client
	baseURL       string
	gatewayAPIKey string
}

// NewClient creates a new client.
// Provider keys are loaded from env by default (e.g. ANTHROPIC_API_KEY).
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		providerKeys: make(map[string]string),
		logger:       slog.Default(),
		httpClient:   newDefaultHTTPClient(),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = newDefaultHTTPClient()
	}

	c.core = core.NewEngine(c.providerKeys)
	if c.isProxyMode() {
		c.initProxyProviders()
	} else {
		c.initProviders()
		c.initVoicePipeline()
	}

	c.Messages = &MessagesService{client: c}
	c.Runs = &RunsService{client: c}
	return c
}

func (c *Client) isProxyMode() bool {
	return c != nil && c.baseURL != ""
}

func (c *Client) initProviders() {
	// Anthropic
	if key := c.core.GetAPIKey("anthropic"); key != "" {
		c.core.RegisterProvider(newAnthropicAdapter(anthropic.New(key, anthropic.WithHTTPClient(c.httpClient))))
	}

	// OpenAI Chat Completions + Responses (shared key)
	if key := c.core.GetAPIKey("openai"); key != "" {
		c.core.RegisterProvider(newOpenAIAdapter(openai.New(key, openai.WithHTTPClient(c.httpClient))))
		c.core.RegisterProvider(newOaiRespAdapter(oai_resp.New(key, oai_resp.WithHTTPClient(c.httpClient))))
	}

	// Groq
	if key := c.core.GetAPIKey("groq"); key != "" {
		c.core.RegisterProvider(newGroqAdapter(groq.New(key, groq.WithHTTPClient(c.httpClient))))
	}

	// Cerebras
	if key := c.core.GetAPIKey("cerebras"); key != "" {
		c.core.RegisterProvider(newCerebrasAdapter(cerebras.New(key, cerebras.WithHTTPClient(c.httpClient))))
	}

	// OpenRouter
	if key := c.core.GetAPIKey("openrouter"); key != "" {
		c.core.RegisterProvider(newOpenRouterAdapter(openrouter.New(key, openrouter.WithHTTPClient(c.httpClient))))
	}

	// Gemini Developer API backend (gem-dev)
	gemDevKey := c.core.GetAPIKey("gem-dev")
	if gemDevKey == "" {
		gemDevKey = os.Getenv("GEMINI_API_KEY")
	}
	if gemDevKey == "" {
		gemDevKey = os.Getenv("GOOGLE_API_KEY")
	}
	if gemDevKey != "" {
		c.core.RegisterProvider(newGemAdapter(gem.NewDeveloper(gemDevKey, gem.WithHTTPClient(c.httpClient))))
	}

	// Gemini Vertex backend (gem-vert)
	gemVertKey := c.core.GetAPIKey("gem-vert")
	if gemVertKey == "" {
		gemVertKey = os.Getenv("VERTEXAI_API_KEY")
	}
	if gemVertKey != "" {
		c.core.RegisterProvider(newGemAdapter(gem.NewVertex(gemVertKey, gem.WithHTTPClient(c.httpClient))))
	}
}

func (c *Client) initVoicePipeline() {
	cartesiaKey := c.getCartesiaAPIKey()
	if cartesiaKey != "" {
		c.voicePipeline = voice.NewPipelineWithProviders(
			stt.NewCartesiaWithClient(cartesiaKey, c.httpClient),
			tts.NewCartesiaWithClient(cartesiaKey, c.httpClient),
		)
	}
}

func (c *Client) getCartesiaAPIKey() string {
	if key, ok := c.providerKeys["cartesia"]; ok && key != "" {
		return key
	}
	return os.Getenv("CARTESIA_API_KEY")
}

func (c *Client) getSTTProvider() stt.Provider {
	if c.voicePipeline != nil {
		return c.voicePipeline.STTProvider()
	}
	cartesiaKey := c.getCartesiaAPIKey()
	if cartesiaKey == "" {
		return nil
	}
	return stt.NewCartesiaWithClient(cartesiaKey, c.httpClient)
}

func (c *Client) getTTSProvider() tts.Provider {
	if c.voicePipeline != nil {
		return c.voicePipeline.TTSProvider()
	}
	cartesiaKey := c.getCartesiaAPIKey()
	if cartesiaKey == "" {
		return nil
	}
	return tts.NewCartesiaWithClient(cartesiaKey, c.httpClient)
}

// VoicePipeline returns the voice pipeline when initialized.
func (c *Client) VoicePipeline() *voice.Pipeline {
	return c.voicePipeline
}

// Engine returns the core engine.
func (c *Client) Engine() *core.Engine {
	return c.core
}
