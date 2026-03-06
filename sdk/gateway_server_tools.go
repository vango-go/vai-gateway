package vai

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

func (c *Client) executeGatewayServerTool(ctx context.Context, toolName string, provider string, input map[string]any) ([]types.ContentBlock, error) {
	if c == nil || !c.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, core.NewInvalidRequestErrorWithParam("tool is required", "tool")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, core.NewInvalidRequestErrorWithParam("provider is required", "provider")
	}
	if input == nil {
		return nil, core.NewInvalidRequestErrorWithParam("input must be an object", "input")
	}

	headerName, ok := providerByokHeaders[provider]
	if !ok {
		return nil, core.NewInvalidRequestErrorWithParam("unsupported provider", "provider")
	}
	providerKey := c.providerKeyForProvider(provider)
	if providerKey == "" {
		return nil, &core.Error{
			Type:    core.ErrAuthentication,
			Message: "missing server tool provider key header",
			Param:   headerName,
			Code:    "provider_key_missing",
		}
	}

	headers := make(http.Header)
	headers.Set(vaiVersionHeader, vaiVersionValue)
	headers.Set("Content-Type", "application/json")
	if c.gatewayAPIKey != "" {
		headers.Set("Authorization", "Bearer "+c.gatewayAPIKey)
	}
	headers.Set(headerName, providerKey)

	ctx, cancel := withDefaultGatewayTimeout(ctx)
	defer cancel()

	payload := types.ServerToolExecuteRequest{
		Tool:  toolName,
		Input: input,
		ServerToolConfig: map[string]any{
			toolName: map[string]any{"provider": provider},
		},
	}

	resp, endpoint, err := c.postGatewayJSON(ctx, "/v1/server-tools:execute", payload, headers)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeGatewayErrorResponse(resp, endpoint, http.MethodPost)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &core.Error{
			Type:      core.ErrAPI,
			Message:   "failed to read server tool response",
			RequestID: requestIDFromHeader(resp.Header),
		}
	}
	decoded, err := types.UnmarshalServerToolExecuteResponse(body)
	if err != nil {
		return nil, &core.Error{
			Type:      core.ErrAPI,
			Message:   "failed to decode server tool response",
			RequestID: requestIDFromHeader(resp.Header),
		}
	}
	return decoded.Content, nil
}
