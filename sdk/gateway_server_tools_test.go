package vai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestGatewayVAIWebSearch_PostsExecuteRequestWithProviderHeader(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotProviderKey string
	var gotBody types.ServerToolExecuteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotProviderKey = r.Header.Get("X-Provider-Key-Tavily")
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ServerToolExecuteResponse{
			Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
		})
	}))
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL+"/gw"),
		WithProviderKey("tavily", "tvly-test"),
		WithHTTPClient(server.Client()),
	)

	tool := VAIWebSearch(Tavily)
	result, err := tool.Handler(contextWithToolExecutionClient(context.Background(), client), json.RawMessage(`{"query":"latest","max_results":3}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	if gotPath != "/gw/v1/server-tools:execute" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotProviderKey != "tvly-test" {
		t.Fatalf("X-Provider-Key-Tavily=%q", gotProviderKey)
	}
	if gotBody.Tool != "vai_web_search" {
		t.Fatalf("tool=%q", gotBody.Tool)
	}
	if gotBody.Input["query"] != "latest" {
		t.Fatalf("input.query=%v", gotBody.Input["query"])
	}
	if cfg, ok := gotBody.ServerToolConfig["vai_web_search"].(map[string]any); !ok || cfg["provider"] != "tavily" {
		t.Fatalf("server_tool_config=%#v", gotBody.ServerToolConfig)
	}

	content, ok := result.([]types.ContentBlock)
	if !ok {
		t.Fatalf("result type=%T, want []types.ContentBlock", result)
	}
	if len(content) != 1 {
		t.Fatalf("len(content)=%d", len(content))
	}
	tb, ok := content[0].(types.TextBlock)
	if !ok || tb.Text != "ok" {
		t.Fatalf("content[0]=%#v", content[0])
	}
}

func TestGatewayVAIWebTools_ProviderHeadersByTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		tool        ToolWithHandler
		headerName  string
		headerValue string
		input       string
		providerOpt ClientOption
	}{
		{
			name:        "search_exa",
			tool:        VAIWebSearch(Exa),
			headerName:  "X-Provider-Key-Exa",
			headerValue: "exa-test",
			input:       `{"query":"go"}`,
			providerOpt: WithProviderKey("exa", "exa-test"),
		},
		{
			name:        "fetch_firecrawl",
			tool:        VAIWebFetch(Firecrawl),
			headerName:  "X-Provider-Key-Firecrawl",
			headerValue: "fc-test",
			input:       `{"url":"https://example.com"}`,
			providerOpt: WithProviderKey("firecrawl", "fc-test"),
		},
		{
			name:        "image_gemdev",
			tool:        VAIImage(GemDev),
			headerName:  "X-Provider-Key-Gemini",
			headerValue: "gem-test",
			input:       `{"prompt":"make a poster"}`,
			providerOpt: WithProviderKey("gem-dev", "gem-test"),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get(tc.headerName)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(types.ServerToolExecuteResponse{
					Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
				})
			}))
			defer server.Close()

			client := NewClient(
				WithBaseURL(server.URL),
				tc.providerOpt,
				WithHTTPClient(server.Client()),
			)
			_, err := tc.tool.Handler(contextWithToolExecutionClient(context.Background(), client), json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if gotHeader != tc.headerValue {
				t.Fatalf("%s=%q", tc.headerName, gotHeader)
			}
		})
	}
}

func TestGatewayVAIWebTools_MissingProviderKeyFailsBeforeNetwork(t *testing.T) {
	t.Parallel()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL),
		WithHTTPClient(server.Client()),
	)

	_, err := VAIWebSearch(Tavily).Handler(contextWithToolExecutionClient(context.Background(), client), json.RawMessage(`{"query":"go"}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	if calls != 0 {
		t.Fatalf("expected no network calls, got %d", calls)
	}

	var coreErr *core.Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("error type=%T, want *core.Error", err)
	}
	if coreErr.Type != core.ErrAuthentication {
		t.Fatalf("type=%q", coreErr.Type)
	}
	if coreErr.Param != "X-Provider-Key-Tavily" {
		t.Fatalf("param=%q", coreErr.Param)
	}
}

func TestGatewayVAIImage_PostsExecuteRequestWithProviderHeader(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotProviderKey string
	var gotBody types.ServerToolExecuteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotProviderKey = r.Header.Get("X-Provider-Key-Gemini")
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ServerToolExecuteResponse{
			Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "ok"}},
		})
	}))
	defer server.Close()

	client := NewClient(
		WithBaseURL(server.URL+"/gw"),
		WithProviderKey("gem-dev", "gem-test"),
		WithHTTPClient(server.Client()),
	)

	tool := VAIImage(GemDev)
	result, err := tool.Handler(contextWithToolExecutionClient(context.Background(), client), json.RawMessage(`{"prompt":"latest album cover","size":"1K"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	if gotPath != "/gw/v1/server-tools:execute" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotProviderKey != "gem-test" {
		t.Fatalf("X-Provider-Key-Gemini=%q", gotProviderKey)
	}
	if gotBody.Tool != "vai_image" {
		t.Fatalf("tool=%q", gotBody.Tool)
	}
	if gotBody.Input["prompt"] != "latest album cover" {
		t.Fatalf("input.prompt=%v", gotBody.Input["prompt"])
	}
	if cfg, ok := gotBody.ServerToolConfig["vai_image"].(map[string]any); !ok || cfg["provider"] != "gem-dev" {
		t.Fatalf("server_tool_config=%#v", gotBody.ServerToolConfig)
	}

	content, ok := result.([]types.ContentBlock)
	if !ok {
		t.Fatalf("result type=%T, want []types.ContentBlock", result)
	}
	if len(content) != 1 {
		t.Fatalf("len(content)=%d", len(content))
	}
}

func TestGatewayVAIWebTools_RequireProxyMode(t *testing.T) {
	t.Parallel()

	client := NewClient(WithProviderKey("tavily", "tvly-test"))
	_, err := VAIWebSearch(Tavily).Handler(contextWithToolExecutionClient(context.Background(), client), json.RawMessage(`{"query":"go"}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("error type=%T", err)
	}
	if coreErr.Type != core.ErrInvalidRequest {
		t.Fatalf("type=%q", coreErr.Type)
	}
}

func TestGatewayVAIWebTools_UnsupportedProviderRejected(t *testing.T) {
	t.Parallel()

	tool := VAIWebSearch(Firecrawl)
	_, err := tool.Handler(context.Background(), json.RawMessage(`{"query":"go"}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	var coreErr *core.Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("error type=%T", err)
	}
	if coreErr.Type != core.ErrInvalidRequest {
		t.Fatalf("type=%q", coreErr.Type)
	}
	if coreErr.Code != "unsupported_tool_provider" {
		t.Fatalf("code=%q", coreErr.Code)
	}
}
