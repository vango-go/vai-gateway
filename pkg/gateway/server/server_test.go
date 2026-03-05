package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/gateway/config"
)

func TestServer_UnknownRoute_ReturnsJSON404(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	s := New(config.Config{
		AuthMode: config.AuthModeDisabled,
		APIKeys:  map[string]struct{}{},

		CORSAllowedOrigins:            map[string]struct{}{},
		ModelAllowlist:                map[string]struct{}{},
		UpstreamConnectTimeout:        time.Second,
		UpstreamResponseHeaderTimeout: time.Second,
	}, logger)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rr.Body.String(), `"type":"not_found_error"`) {
		t.Fatalf("unexpected body: %q", rr.Body.String())
	}
}

func TestServer_ModelsRoute_Reachable(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	s := New(config.Config{
		AuthMode: config.AuthModeDisabled,
		APIKeys:  map[string]struct{}{},

		CORSAllowedOrigins:            map[string]struct{}{},
		ModelAllowlist:                map[string]struct{}{},
		UpstreamConnectTimeout:        time.Second,
		UpstreamResponseHeaderTimeout: time.Second,
	}, logger)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rr.Body.String(), `"models"`) {
		t.Fatalf("unexpected body: %q", rr.Body.String())
	}
}

func TestServer_RunsRoutes_Reachable(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	s := New(config.Config{
		AuthMode: config.AuthModeDisabled,
		APIKeys:  map[string]struct{}{},

		CORSAllowedOrigins:            map[string]struct{}{},
		ModelAllowlist:                map[string]struct{}{},
		UpstreamConnectTimeout:        time.Second,
		UpstreamResponseHeaderTimeout: time.Second,
		TavilyBaseURL:                 "https://api.tavily.com",
		FirecrawlBaseURL:              "https://api.firecrawl.dev",
	}, logger)

	for _, path := range []string{"/v1/runs", "/v1/runs:stream", "/v1/server-tools:execute"} {
		rr := httptest.NewRecorder()
		body := `{"request":{"model":"anthropic/test","messages":[{"role":"user","content":"hi"}]}}`
		if path == "/v1/server-tools:execute" {
			body = `{"tool":"vai_web_search","input":{"query":"hi"}}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if path != "/v1/server-tools:execute" {
			req.Header.Set("X-Provider-Key-Anthropic", "sk-test")
		}
		s.Handler().ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Fatalf("path %s unexpectedly returned 404", path)
		}
	}
}

func TestServer_LiveRoute_Reachable(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	s := New(config.Config{
		AuthMode: config.AuthModeDisabled,
		APIKeys:  map[string]struct{}{},

		CORSAllowedOrigins:            map[string]struct{}{},
		ModelAllowlist:                map[string]struct{}{},
		UpstreamConnectTimeout:        time.Second,
		UpstreamResponseHeaderTimeout: time.Second,
		WSMaxSessionDuration:          time.Minute,
		WSMaxSessionsPerPrincipal:     2,
		TavilyBaseURL:                 "https://api.tavily.com",
		FirecrawlBaseURL:              "https://api.firecrawl.dev",
	}, logger)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/live", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusNotFound {
		t.Fatalf("path /v1/live unexpectedly returned 404")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ws_upgrade_required") {
		t.Fatalf("unexpected body: %q", rr.Body.String())
	}
}
