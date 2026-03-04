package upstream

import (
	"fmt"
	"net/http"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/anthropic"
	"github.com/vango-go/vai-lite/pkg/core/providers/cerebras"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/providers/groq"
	"github.com/vango-go/vai-lite/pkg/core/providers/oai_resp"
	"github.com/vango-go/vai-lite/pkg/core/providers/openai"
	"github.com/vango-go/vai-lite/pkg/core/providers/openrouter"
)

type Factory struct {
	HTTPClient *http.Client
}

func (f Factory) New(providerName, apiKey string) (core.Provider, error) {
	client := f.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	switch providerName {
	case "anthropic":
		return &anthropicAdapter{provider: anthropic.New(apiKey, anthropic.WithHTTPClient(client))}, nil
	case "openai":
		return &openaiAdapter{provider: openai.New(apiKey, openai.WithHTTPClient(client))}, nil
	case "oai-resp":
		return &oaiRespAdapter{provider: oai_resp.New(apiKey, oai_resp.WithHTTPClient(client))}, nil
	case "groq":
		return &groqAdapter{provider: groq.New(apiKey, groq.WithHTTPClient(client))}, nil
	case "cerebras":
		return &cerebrasAdapter{provider: cerebras.New(apiKey, cerebras.WithHTTPClient(client))}, nil
	case "openrouter":
		return &openrouterAdapter{provider: openrouter.New(apiKey, openrouter.WithHTTPClient(client))}, nil
	case "gem-dev":
		return &gemAdapter{provider: gem.NewDeveloper(apiKey, gem.WithHTTPClient(client))}, nil
	case "gem-vert":
		return &gemAdapter{provider: gem.NewVertex(apiKey, gem.WithHTTPClient(client))}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
}
