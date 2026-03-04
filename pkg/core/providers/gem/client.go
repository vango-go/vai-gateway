package gem

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core"
)

func (p *Provider) getClient(ctx context.Context, ext gemExtensions) (*genai.Client, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()

	if p.client != nil {
		if p.mode == backendModeVertex && p.apiKey == "" {
			if ext.VertexProject != "" && p.vertexProject != "" && ext.VertexProject != p.vertexProject {
				return nil, core.NewInvalidRequestError("gem.vertex_project cannot change within a provider instance")
			}
			if ext.VertexLocation != "" && p.vertexLocation != "" && ext.VertexLocation != p.vertexLocation {
				return nil, core.NewInvalidRequestError("gem.vertex_location cannot change within a provider instance")
			}
		}
		return p.client, nil
	}

	cfg := &genai.ClientConfig{Backend: p.backend}
	if p.httpClient != nil {
		cfg.HTTPClient = p.httpClient
	}
	if strings.TrimSpace(p.baseURL) != "" {
		cfg.HTTPOptions.BaseURL = p.baseURL
	}

	apiKey := strings.TrimSpace(p.apiKey)
	if apiKey != "" {
		cfg.APIKey = apiKey
	}

	if p.mode == backendModeDeveloper {
		if cfg.APIKey == "" {
			return nil, &Error{Type: ErrAuthentication, Message: "gem-dev requires GEMINI_API_KEY (or GOOGLE_API_KEY fallback in SDK init)"}
		}
	}

	if p.mode == backendModeVertex {
		if cfg.APIKey == "" {
			project := strings.TrimSpace(ext.VertexProject)
			location := strings.TrimSpace(ext.VertexLocation)
			if project == "" || location == "" {
				return nil, &Error{Type: ErrAuthentication, Message: "gem-vert requires VERTEXAI_API_KEY or gem.vertex_project + gem.vertex_location"}
			}
			cfg.Project = project
			cfg.Location = location
			p.vertexProject = project
			p.vertexLocation = location
		}
	}

	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, mapError(err)
	}
	p.client = client
	return client, nil
}
