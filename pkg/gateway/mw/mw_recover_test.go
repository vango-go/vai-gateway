package mw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core"
)

func TestRecover_PanicReturnsCanonicalJSON(t *testing.T) {
	h := Recover(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	h = RequestID(h)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct == "" {
		t.Fatalf("expected content-type header to be set")
	}
	var env struct {
		Error core.Error `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Type != core.ErrAPI {
		t.Fatalf("type=%q", env.Error.Type)
	}
	if env.Error.Code != "panic" {
		t.Fatalf("code=%q", env.Error.Code)
	}
	panicObj, ok := env.Error.ProviderError.(map[string]any)
	if !ok {
		t.Fatalf("provider_error=%T, want map[string]any", env.Error.ProviderError)
	}
	if panicObj["panic"] != "boom" {
		t.Fatalf("provider_error.panic=%v", panicObj["panic"])
	}
	if env.Error.RequestID == "" {
		t.Fatalf("expected request_id to be set")
	}
	if got := rr.Header().Get("X-Request-ID"); got == "" {
		t.Fatalf("expected X-Request-ID header")
	}
}
