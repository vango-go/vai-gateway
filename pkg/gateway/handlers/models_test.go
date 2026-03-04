package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vango-go/vai-lite/pkg/gateway/config"
)

func TestModelsHandler_GETCuratedCatalog(t *testing.T) {
	h := ModelsHandler{Config: config.Config{ModelAllowlist: map[string]struct{}{}}}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control=%q, want public, max-age=300", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	models, ok := body["models"].([]any)
	if !ok {
		t.Fatalf("models missing or wrong type: %#v", body["models"])
	}
	if len(models) == 0 {
		t.Fatalf("models empty")
	}

	first, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("first model wrong type: %T", models[0])
	}
	if first["id"] != "anthropic/claude-sonnet-4" {
		t.Fatalf("first id=%v, want anthropic/claude-sonnet-4", first["id"])
	}
	auth, ok := first["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth missing on first model: %#v", first)
	}
	if auth["requires_byok_header"] != "X-Provider-Key-Anthropic" {
		t.Fatalf("requires_byok_header=%v, want X-Provider-Key-Anthropic", auth["requires_byok_header"])
	}
	caps, ok := first["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing on first model: %#v", first)
	}
	if _, ok := caps["streaming"]; !ok {
		t.Fatalf("streaming capability missing: %#v", caps)
	}
}

func TestModelsHandler_AllowlistFiltersAndSynthesizesUnknown(t *testing.T) {
	h := ModelsHandler{Config: config.Config{
		ModelAllowlist: map[string]struct{}{
			"openai/gpt-4o":           {},
			"gem-dev/gemini-2.5":      {},
			"unknown/custom-123":      {},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("Cache-Control=%q, want private, max-age=300", got)
	}

	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(body.Models) != 3 {
		t.Fatalf("models len=%d, want 3", len(body.Models))
	}

	modelByID := make(map[string]map[string]any, len(body.Models))
	for _, m := range body.Models {
		id, _ := m["id"].(string)
		modelByID[id] = m
	}

	known := modelByID["openai/gpt-4o"]
	if known == nil {
		t.Fatalf("missing allowlisted known model")
	}
	if _, ok := known["auth"].(map[string]any); !ok {
		t.Fatalf("known model auth missing: %#v", known)
	}

	gemDev := modelByID["gem-dev/gemini-2.5"]
	if gemDev == nil {
		t.Fatalf("missing synthesized gem-dev allowlist model")
	}
	geminiAuth, ok := gemDev["auth"].(map[string]any)
	if !ok {
		t.Fatalf("gem-dev model auth missing: %#v", gemDev)
	}
	if geminiAuth["requires_byok_header"] != "X-Provider-Key-Gemini" {
		t.Fatalf("requires_byok_header=%v, want X-Provider-Key-Gemini", geminiAuth["requires_byok_header"])
	}

	unknown := modelByID["unknown/custom-123"]
	if unknown == nil {
		t.Fatalf("missing synthesized unknown allowlist model")
	}
	if _, ok := unknown["auth"]; ok {
		t.Fatalf("unknown provider auth should be omitted: %#v", unknown["auth"])
	}
	caps, ok := unknown["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("unknown model capabilities should be present as object: %#v", unknown)
	}
	if len(caps) != 0 {
		t.Fatalf("unknown model capabilities should be empty object, got: %#v", caps)
	}
}

func TestModelsHandler_MethodNotAllowed(t *testing.T) {
	h := ModelsHandler{Config: config.Config{ModelAllowlist: map[string]struct{}{}}}

	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var body map[string]map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	errObj := body["error"]
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type=%v, want invalid_request_error", errObj["type"])
	}
	if errObj["code"] != "method_not_allowed" {
		t.Fatalf("error.code=%v, want method_not_allowed", errObj["code"])
	}
}
