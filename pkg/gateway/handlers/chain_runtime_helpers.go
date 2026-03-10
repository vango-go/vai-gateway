package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	assetsvc "github.com/vango-go/vai-lite/pkg/gateway/assets"
	chainrt "github.com/vango-go/vai-lite/pkg/gateway/chains"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/principal"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

const (
	headerInternalOrgID         = "X-VAI-Internal-Org-ID"
	headerInternalPrincipalID   = "X-VAI-Internal-Principal-ID"
	headerInternalPrincipalType = "X-VAI-Internal-Principal-Type"
)

func chainPrincipalFromRequest(r *http.Request, cfg config.Config) chainrt.Principal {
	if internalOrgID := strings.TrimSpace(r.Header.Get(headerInternalOrgID)); internalOrgID != "" {
		internalPrincipalID := strings.TrimSpace(r.Header.Get(headerInternalPrincipalID))
		if internalPrincipalID == "" {
			internalPrincipalID = internalOrgID
		}
		internalPrincipalType := strings.TrimSpace(r.Header.Get(headerInternalPrincipalType))
		if internalPrincipalType == "" {
			internalPrincipalType = "app_session"
		}
		return chainrt.Principal{
			OrgID:         internalOrgID,
			PrincipalID:   internalPrincipalID,
			PrincipalType: internalPrincipalType,
			ActorID:       strings.TrimSpace(r.Header.Get("X-VAI-Actor-ID")),
		}
	}
	resolved := principal.Resolve(r, cfg)
	principalID := resolved.Key
	if principalID == "" {
		principalID = "anonymous"
	}
	return chainrt.Principal{
		OrgID:         principalID,
		PrincipalID:   principalID,
		PrincipalType: string(resolved.Kind),
		ActorID:       strings.TrimSpace(r.Header.Get("X-VAI-Actor-ID")),
	}
}

func chainScopeFromHeaders(headers http.Header) chainrt.CredentialScope {
	scope := chainrt.CredentialScope{
		ProviderKeys:        map[string]string{},
		AllowedGatewayTools: map[string]struct{}{},
	}
	for provider, header := range map[string]string{
		"anthropic":  "X-Provider-Key-Anthropic",
		"openai":     "X-Provider-Key-OpenAI",
		"oai-resp":   "X-Provider-Key-OpenAI",
		"gem-dev":    "X-Provider-Key-Gemini",
		"gem-vert":   "X-Provider-Key-VertexAI",
		"groq":       "X-Provider-Key-Groq",
		"cerebras":   "X-Provider-Key-Cerebras",
		"openrouter": "X-Provider-Key-OpenRouter",
	} {
		if key := strings.TrimSpace(headers.Get(header)); key != "" {
			scope.ProviderKeys[provider] = key
		}
	}
	if strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyTavily)) != "" || strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyExa)) != "" {
		scope.AllowedGatewayTools[servertools.ToolWebSearch] = struct{}{}
	}
	if strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyTavily)) != "" || strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyFirecrawl)) != "" {
		scope.AllowedGatewayTools[servertools.ToolWebFetch] = struct{}{}
	}
	if strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyGemini)) != "" || strings.TrimSpace(headers.Get(servertools.HeaderProviderKeyVertexAI)) != "" {
		scope.AllowedGatewayTools[servertools.ToolImage] = struct{}{}
	}
	return scope
}

type httpConfigProvider interface {
	getConfig() config.Config
	getHTTPClient() *http.Client
	getUpstreams() ProviderFactory
	getAssets() *assetsvc.Service
}

func chainEnv(h httpConfigProvider, principalValue chainrt.Principal, headers http.Header, mode types.AttachmentMode, protocol string, allowClientTools bool, send chainrt.AttachmentSink) chainrt.RuntimeEnvironment {
	scope := chainScopeFromHeaders(headers)
	assetResolver := h.getAssets()
	return chainrt.RuntimeEnvironment{
		Principal:        principalValue,
		Mode:             mode,
		Protocol:         protocol,
		Scope:            scope,
		AllowClientTools: allowClientTools,
		Send:             send,
		NewProvider: func(providerName, apiKey string) (core.Provider, error) {
			return h.getUpstreams().New(providerName, apiKey)
		},
		BuildGatewayTools: func(enabled []string, rawConfig map[string]any) (*servertools.Registry, error) {
			return newServerToolsRegistryFromHeaders(h.getConfig(), h.getHTTPClient(), headers, enabled, rawConfig)
		},
		ResolveMessageRequest: func(ctx context.Context, req *types.MessageRequest) (*types.MessageRequest, error) {
			return resolveAssetBackedRequestWithOrg(ctx, assetResolver, principalValue.OrgID, req)
		},
	}
}

func resolveAssetBackedRequest(ctx context.Context, svc *assetsvc.Service, cfg config.Config, r *http.Request, req *types.MessageRequest) (*types.MessageRequest, error) {
	if req == nil || !types.RequestHasAssetReferences(req) {
		return req, nil
	}
	if svc == nil {
		return nil, core.NewInvalidRequestError("asset references require gateway asset storage to be configured")
	}
	pr := chainPrincipalFromRequest(r, cfg)
	return svc.ResolveMessageRequest(ctx, pr.OrgID, req)
}

func resolveAssetBackedRequestWithOrg(ctx context.Context, svc *assetsvc.Service, orgID string, req *types.MessageRequest) (*types.MessageRequest, error) {
	if req == nil || !types.RequestHasAssetReferences(req) {
		return req, nil
	}
	if svc == nil {
		return nil, core.NewInvalidRequestError("asset references require gateway asset storage to be configured")
	}
	return svc.ResolveMessageRequest(ctx, orgID, req)
}

func writeCanonicalErrorJSON(w http.ResponseWriter, err *types.CanonicalError) {
	if err == nil {
		err = types.NewCanonicalError(types.ErrorCodeProtocolUnknownFrame, "invalid request")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(types.HTTPStatusForCanonicalError(err.Code))
	_ = json.NewEncoder(w).Encode(types.CanonicalErrorEnvelope{Error: err})
}
