package vai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	vaiVersionHeader                  = "X-VAI-Version"
	vaiVersionValue                   = "1"
	defaultGatewayNonStreamingTimeout = 2 * time.Minute
)

var providerByokHeaders = map[string]string{
	"anthropic":  "X-Provider-Key-Anthropic",
	"openai":     "X-Provider-Key-OpenAI",
	"oai-resp":   "X-Provider-Key-OpenAI",
	"gem-dev":    "X-Provider-Key-Gemini",
	"gem-vert":   "X-Provider-Key-VertexAI",
	"groq":       "X-Provider-Key-Groq",
	"cerebras":   "X-Provider-Key-Cerebras",
	"openrouter": "X-Provider-Key-OpenRouter",
	"cartesia":   "X-Provider-Key-Cartesia",
	"elevenlabs": "X-Provider-Key-ElevenLabs",
	"tavily":     "X-Provider-Key-Tavily",
	"exa":        "X-Provider-Key-Exa",
	"firecrawl":  "X-Provider-Key-Firecrawl",
}

func (c *Client) initProxyProviders() {
	for _, providerName := range []string{
		"anthropic",
		"openai",
		"oai-resp",
		"groq",
		"cerebras",
		"openrouter",
		"gem-dev",
		"gem-vert",
	} {
		c.core.RegisterProvider(&gatewayProxyProvider{
			client:       c,
			providerName: providerName,
		})
	}
}

type gatewayProxyProvider struct {
	client       *Client
	providerName string
}

func (p *gatewayProxyProvider) Name() string {
	return p.providerName
}

func (p *gatewayProxyProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}

	proxyReq := *req
	proxyReq.Model = p.publicModel(req.Model)
	proxyReq.Stream = false

	return p.client.proxyCreateMessage(ctx, &proxyReq)
}

func (p *gatewayProxyProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}

	proxyReq := *req
	proxyReq.Model = p.publicModel(req.Model)
	proxyReq.Stream = true

	return p.client.proxyStreamMessage(ctx, &proxyReq)
}

func (p *gatewayProxyProvider) Capabilities() core.ProviderCapabilities {
	// Proxy mode forwards to the gateway which enforces compatibility.
	return core.ProviderCapabilities{
		Vision:           true,
		AudioInput:       true,
		AudioOutput:      true,
		Video:            true,
		Tools:            true,
		ToolStreaming:    true,
		Thinking:         true,
		StructuredOutput: true,
	}
}

func (p *gatewayProxyProvider) publicModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return p.providerName + "/"
	}
	return p.providerName + "/" + model
}

func (c *Client) proxyCreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	ctx, cancel := withDefaultGatewayTimeout(ctx)
	defer cancel()

	headers, err := c.buildGatewayHeaders(req, false)
	if err != nil {
		return nil, err
	}

	resp, endpoint, err := c.postGatewayJSON(ctx, "/v1/messages", req, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &core.Error{
			Type:      core.ErrAPI,
			Message:   "failed to read gateway response",
			RequestID: requestIDFromHeader(resp.Header),
		}
	}
	decoded, err := types.UnmarshalMessageResponse(body)
	if err != nil {
		return nil, &core.Error{
			Type:      core.ErrAPI,
			Message:   "failed to decode gateway response",
			RequestID: requestIDFromHeader(resp.Header),
		}
	}
	return decoded, nil
}

func (c *Client) proxyStreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	headers, err := c.buildGatewayHeaders(req, true)
	if err != nil {
		return nil, err
	}

	resp, endpoint, err := c.postGatewayJSON(ctx, "/v1/messages", req, headers)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}

	return newGatewayMessageEventStream(resp.Body, endpoint), nil
}

func (c *Client) postGatewayJSON(ctx context.Context, endpointPath string, payload any, headers http.Header) (*http.Response, string, error) {
	endpoint, err := c.gatewayEndpoint(endpointPath)
	if err != nil {
		return nil, "", err
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, endpoint, core.NewInvalidRequestError("failed to marshal request body")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, endpoint, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}

	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, endpoint, &TransportError{Op: http.MethodPost, URL: endpoint, Err: err}
	}

	return resp, endpoint, nil
}

func (c *Client) buildGatewayHeaders(req *types.MessageRequest, stream bool) (http.Header, error) {
	if !c.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}

	providerName, _, err := core.ParseModelString(req.Model)
	if err != nil {
		return nil, err
	}

	headers := make(http.Header)
	headers.Set(vaiVersionHeader, vaiVersionValue)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	}
	if c.gatewayAPIKey != "" {
		headers.Set("Authorization", "Bearer "+c.gatewayAPIKey)
	}

	if byokHeader, ok := providerByokHeaders[strings.ToLower(providerName)]; ok {
		if key := c.providerKeyForProvider(strings.ToLower(providerName)); key != "" {
			headers.Set(byokHeader, key)
		}
	}
	needsSTT := types.RequestHasAudioSTT(req)
	needsTTS := (req.Voice != nil && req.Voice.Output != nil) || strings.TrimSpace(req.TTSModel) != ""
	if needsSTT || needsTTS {
		if key := c.getCartesiaAPIKey(); key != "" {
			headers.Set("X-Provider-Key-Cartesia", key)
		}
	}
	return headers, nil
}

func (c *Client) providerKeyForProvider(providerName string) string {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "" {
		return ""
	}

	switch providerName {
	case "openai":
		if key := c.core.GetAPIKey("openai"); key != "" {
			return key
		}
		return c.core.GetAPIKey("oai-resp")
	case "oai-resp":
		if key := c.core.GetAPIKey("oai-resp"); key != "" {
			return key
		}
		return c.core.GetAPIKey("openai")
	case "gem-dev":
		if key := c.core.GetAPIKey("gem-dev"); key != "" {
			return key
		}
		return ""
	case "gem-vert":
		if key := c.core.GetAPIKey("gem-vert"); key != "" {
			return key
		}
		return ""
	default:
		return c.core.GetAPIKey(providerName)
	}
}

func (c *Client) gatewayEndpoint(path string) (string, error) {
	rawBaseURL := strings.TrimSpace(c.baseURL)
	if rawBaseURL == "" {
		return "", core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}

	base, err := url.Parse(rawBaseURL)
	if err != nil || strings.TrimSpace(base.Scheme) == "" || strings.TrimSpace(base.Host) == "" {
		return "", core.NewInvalidRequestError("invalid gateway base URL")
	}
	if base.User != nil {
		return "", core.NewInvalidRequestError("gateway base URL must not include credentials")
	}

	base.RawQuery = ""
	base.Fragment = ""

	cleanPath := "/" + strings.TrimLeft(path, "/")
	basePath := strings.TrimSuffix(base.Path, "/")
	if basePath == "" || basePath == "/" {
		base.Path = cleanPath
	} else {
		base.Path = basePath + cleanPath
	}
	base.RawPath = ""

	return base.String(), nil
}

func decodeGatewayErrorResponse(resp *http.Response, endpoint, method string) error {
	defer resp.Body.Close()

	requestID := requestIDFromHeader(resp.Header)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &TransportError{Op: method, URL: endpoint, Err: err}
	}

	var env struct {
		Error *core.Error `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		if env.Error.RequestID == "" {
			env.Error.RequestID = requestID
		}
		if env.Error.RetryAfter == nil {
			if retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After")); retryAfter != nil {
				env.Error.RetryAfter = retryAfter
			}
		}
		if env.Error.Type == "" {
			env.Error.Type = inferErrorType(resp.StatusCode)
		}
		if env.Error.Message == "" {
			env.Error.Message = http.StatusText(resp.StatusCode)
		}
		return env.Error
	}

	msg := "gateway request failed"
	if resp.StatusCode > 0 {
		msg = fmt.Sprintf("gateway request failed with status %d", resp.StatusCode)
	}
	return &core.Error{
		Type:      inferErrorType(resp.StatusCode),
		Message:   msg,
		RequestID: requestID,
	}
}

func inferErrorType(statusCode int) core.ErrorType {
	switch statusCode {
	case http.StatusBadRequest:
		return core.ErrInvalidRequest
	case http.StatusUnauthorized:
		return core.ErrAuthentication
	case http.StatusForbidden:
		return core.ErrPermission
	case http.StatusNotFound:
		return core.ErrNotFound
	case http.StatusTooManyRequests:
		return core.ErrRateLimit
	case 529:
		return core.ErrOverloaded
	default:
		return core.ErrAPI
	}
}

func requestIDFromHeader(h http.Header) string {
	if h == nil {
		return ""
	}
	if reqID := strings.TrimSpace(h.Get("X-Request-Id")); reqID != "" {
		return reqID
	}
	return strings.TrimSpace(h.Get("X-Request-ID"))
}

func parseRetryAfterHeader(raw string) *int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &seconds
}

func withDefaultGatewayTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), defaultGatewayNonStreamingTimeout)
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultGatewayNonStreamingTimeout)
}

func typesErrorToCoreError(err types.Error) *core.Error {
	coreErr := &core.Error{
		Type:          core.ErrorType(err.Type),
		Message:       err.Message,
		Param:         err.Param,
		Code:          err.Code,
		RequestID:     err.RequestID,
		ProviderError: err.ProviderError,
		RetryAfter:    err.RetryAfter,
	}
	if len(err.CompatIssues) > 0 {
		coreErr.CompatIssues = make([]core.CompatibilityIssue, 0, len(err.CompatIssues))
		for _, issue := range err.CompatIssues {
			coreErr.CompatIssues = append(coreErr.CompatIssues, core.CompatibilityIssue{
				Severity: issue.Severity,
				Param:    issue.Param,
				Code:     issue.Code,
				Message:  issue.Message,
			})
		}
	}
	if coreErr.Type == "" {
		coreErr.Type = core.ErrAPI
	}
	if strings.TrimSpace(coreErr.Message) == "" {
		coreErr.Message = "gateway error"
	}
	return coreErr
}
