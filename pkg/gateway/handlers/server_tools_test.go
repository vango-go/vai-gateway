package handlers

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func decodeSingleTextBlock(t *testing.T, payload string) string {
	t.Helper()
	resp, err := types.UnmarshalServerToolExecuteResponse([]byte(payload))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("len(content)=%d, want 1", len(resp.Content))
	}
	tb, ok := resp.Content[0].(types.TextBlock)
	if !ok {
		t.Fatalf("content[0] type=%T, want types.TextBlock", resp.Content[0])
	}
	return tb.Text
}

func TestServerToolsHandler_MethodNotAllowed(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodGet, "/v1/server-tools:execute", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"method_not_allowed"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_StrictValidation(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"hi"},
		"extra":true
	}`)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"extra"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_MissingProviderKey(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"hi"},
		"server_tool_config":{"vai_web_search":{"provider":"tavily"}}
	}`)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Tavily"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_AmbiguousProviderInference(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"hi"}
	}`)))
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	req.Header.Set("X-Provider-Key-Exa", "exa-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"tool_provider_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_ConfigForDisabledToolRejected(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"hi"},
		"server_tool_config":{"vai_web_fetch":{"provider":"tavily"}}
	}`)))
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"server_tool_config.vai_web_fetch"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_ImageMissingProviderKey(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_image",
		"input":{"prompt":"make a poster"},
		"server_tool_config":{"vai_image":{"provider":"gem-dev"}}
	}`)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"X-Provider-Key-Gemini"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_ImageAmbiguousProviderInference(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_image",
		"input":{"prompt":"make a poster"}
	}`)))
	req.Header.Set("X-Provider-Key-Gemini", "gem-test")
	req.Header.Set("X-Provider-Key-VertexAI", "vertex-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"tool_provider_missing"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_ImageIDEditingRejectedForStatelessExecute(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_image",
		"input":{"prompt":"edit this","images":[{"id":"img-01"}]}
	}`)))
	req.Header.Set("X-Provider-Key-Gemini", "gem-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "/v1/runs, /v1/runs:stream, or /v1/live") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"param":"images[0].id"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestServerToolsHandler_SearchTavilySuccess(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.TavilyBaseURL = "https://tavily.test"
	h := ServerToolsHandler{
		Config: cfg,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://tavily.test/search" {
				t.Fatalf("unexpected url %q", req.URL.String())
			}
			if got := req.Header.Get("Authorization"); got != "Bearer tvly-test" {
				t.Fatalf("auth header=%q", got)
			}
			return jsonResponse(http.StatusOK, `{"results":[{"title":"Example","url":"https://example.com","content":"snippet"}]}`), nil
		})},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"latest"},
		"server_tool_config":{"vai_web_search":{"provider":"tavily"}}
	}`)))
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	textPayload := decodeSingleTextBlock(t, rr.Body.String())
	if !strings.Contains(textPayload, `"provider":"tavily"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
	if !strings.Contains(textPayload, `"https://example.com"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
}

func TestServerToolsHandler_SearchExaSuccess(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.ExaBaseURL = "https://exa.test"
	h := ServerToolsHandler{
		Config: cfg,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://exa.test/search" {
				t.Fatalf("unexpected url %q", req.URL.String())
			}
			if got := req.Header.Get("x-api-key"); got != "exa-test" {
				t.Fatalf("x-api-key=%q", got)
			}
			return jsonResponse(http.StatusOK, `{"results":[{"title":"Exa","url":"https://example.org","summary":"sum","text":"full","publishedDate":"2026-03-01"}]}`), nil
		})},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"latest"},
		"server_tool_config":{"vai_web_search":{"provider":"exa"}}
	}`)))
	req.Header.Set("X-Provider-Key-Exa", "exa-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	textPayload := decodeSingleTextBlock(t, rr.Body.String())
	if !strings.Contains(textPayload, `"provider":"exa"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
	if !strings.Contains(textPayload, `"https://example.org"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
}

func TestServerToolsHandler_FetchTavilySuccess(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.TavilyBaseURL = "https://tavily.test"
	h := ServerToolsHandler{
		Config: cfg,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://tavily.test/extract" {
				t.Fatalf("unexpected url %q", req.URL.String())
			}
			if got := req.Header.Get("Authorization"); got != "Bearer tvly-test" {
				t.Fatalf("auth header=%q", got)
			}
			return jsonResponse(http.StatusOK, `{"results":[{"url":"https://93.184.216.34/a","title":"Doc","raw_content":"hello"}]}`), nil
		})},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_fetch",
		"input":{"url":"https://93.184.216.34/a"},
		"server_tool_config":{"vai_web_fetch":{"provider":"tavily"}}
	}`)))
	req.Header.Set("X-Provider-Key-Tavily", "tvly-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	textPayload := decodeSingleTextBlock(t, rr.Body.String())
	if !strings.Contains(textPayload, `"provider":"tavily"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
	if !strings.Contains(textPayload, `"content":"hello"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
}

func TestServerToolsHandler_FetchFirecrawlSuccess(t *testing.T) {
	cfg := baseRunsConfig()
	cfg.FirecrawlBaseURL = "https://firecrawl.test"
	h := ServerToolsHandler{
		Config: cfg,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://firecrawl.test/v2/scrape" {
				t.Fatalf("unexpected url %q", req.URL.String())
			}
			if got := req.Header.Get("Authorization"); got != "Bearer fc-test" {
				t.Fatalf("auth header=%q", got)
			}
			return jsonResponse(http.StatusOK, `{"success":true,"data":{"markdown":"body","metadata":{"title":"Doc"}}}`), nil
		})},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", bytes.NewReader([]byte(`{
		"tool":"vai_web_fetch",
		"input":{"url":"https://93.184.216.34/a"},
		"server_tool_config":{"vai_web_fetch":{"provider":"firecrawl"}}
	}`)))
	req.Header.Set("X-Provider-Key-Firecrawl", "fc-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	textPayload := decodeSingleTextBlock(t, rr.Body.String())
	if !strings.Contains(textPayload, `"provider":"firecrawl"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
	if !strings.Contains(textPayload, `"content":"body"`) {
		t.Fatalf("text payload=%s", textPayload)
	}
}

func TestServerToolsHandler_UnsupportedToolRejected(t *testing.T) {
	h := ServerToolsHandler{Config: baseRunsConfig()}
	req := httptest.NewRequest(http.MethodPost, "/v1/server-tools:execute", strings.NewReader(`{"tool":"nope","input":{}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"unsupported_server_tool"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestTypesToolErrToCore_DefaultsType(t *testing.T) {
	coreErr := typesToolErrToCore(nil, "req_x")
	if coreErr.Type != "api_error" {
		t.Fatalf("type=%q", coreErr.Type)
	}
	if coreErr.RequestID != "req_x" {
		t.Fatalf("request_id=%q", coreErr.RequestID)
	}
}
