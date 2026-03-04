package gem

import "net/http"

// Option configures a Gemini provider instance.
type Option func(*Provider)

// WithHTTPClient injects a custom HTTP client used by the GenAI SDK.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// WithBaseURL overrides the upstream base URL (primarily for tests).
func WithBaseURL(baseURL string) Option {
	return func(p *Provider) {
		p.baseURL = baseURL
	}
}
