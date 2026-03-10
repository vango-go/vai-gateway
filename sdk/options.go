package vai

import (
	"log/slog"
	"net/http"
	"strings"
)

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithProviderKey sets an API key for a specific provider.
// Used to override environment variables in direct mode.
func WithProviderKey(provider, key string) ClientOption {
	return func(c *Client) {
		if c.providerKeys == nil {
			c.providerKeys = make(map[string]string)
		}
		normalized := strings.ToLower(strings.TrimSpace(provider))
		if normalized == "" {
			return
		}
		c.providerKeys[normalized] = key
	}
}

// WithBaseURL enables gateway proxy mode by routing SDK calls to the given base URL.
// Supports values with or without trailing slash and optional base path prefix.
func WithBaseURL(url string) ClientOption {
	return func(c *Client) {
		c.baseURL = strings.TrimSpace(url)
	}
}

// WithGatewayAPIKey sets the gateway bearer token used in proxy mode.
func WithGatewayAPIKey(key string) ClientOption {
	return func(c *Client) {
		c.gatewayAPIKey = strings.TrimSpace(key)
	}
}

// WithHeader adds an additional header to proxy-mode HTTP and WebSocket requests.
func WithHeader(name, value string) ClientOption {
	return func(c *Client) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			return
		}
		if c.extraHeaders == nil {
			c.extraHeaders = make(map[string]string)
		}
		c.extraHeaders[name] = value
	}
}

// WithHTTPClient sets the HTTP client used for proxy mode requests.
// In direct mode, this client is also passed to provider clients when available.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithLogger sets the logger for the client.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}
