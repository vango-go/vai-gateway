package apierror

import (
	"context"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
)

func TestFromError_ContextCanceled_Is408Cancelled(t *testing.T) {
	ce, status := FromError(context.Canceled, "req_test")
	if status != 408 {
		t.Fatalf("status=%d", status)
	}
	if ce.Type != core.ErrAPI {
		t.Fatalf("type=%q", ce.Type)
	}
	if ce.Code != "cancelled" {
		t.Fatalf("code=%q", ce.Code)
	}
	if ce.RequestID != "req_test" {
		t.Fatalf("request_id=%q", ce.RequestID)
	}
}

func TestFromError_Overloaded_Is529(t *testing.T) {
	ce, status := FromError(&core.Error{Type: core.ErrOverloaded, Message: "overloaded"}, "req_test")
	if status != 529 {
		t.Fatalf("status=%d", status)
	}
	if ce.Type != core.ErrOverloaded {
		t.Fatalf("type=%q", ce.Type)
	}
}

func TestFromError_GemError_Preserved(t *testing.T) {
	providerErr := map[string]any{"status": "RESOURCE_EXHAUSTED", "message": "quota exceeded"}
	ce, status := FromError(&gem.Error{
		Type:          gem.ErrRateLimit,
		Message:       "quota exceeded",
		Code:          "RESOURCE_EXHAUSTED",
		ProviderError: providerErr,
	}, "req_gemini")
	if status != 429 {
		t.Fatalf("status=%d", status)
	}
	if ce.Type != core.ErrRateLimit {
		t.Fatalf("type=%q", ce.Type)
	}
	if ce.Message != "quota exceeded" {
		t.Fatalf("message=%q", ce.Message)
	}
	if ce.Code != "RESOURCE_EXHAUSTED" {
		t.Fatalf("code=%q", ce.Code)
	}
	if ce.RequestID != "req_gemini" {
		t.Fatalf("request_id=%q", ce.RequestID)
	}
	if ce.ProviderError == nil {
		t.Fatal("provider_error should be preserved")
	}
}

func TestFromError_GemErrorAPI_Preserved(t *testing.T) {
	providerErr := map[string]any{"status": "INTERNAL", "message": "backend failure"}
	ce, status := FromError(&gem.Error{
		Type:          gem.ErrAPI,
		Message:       "backend failure",
		Code:          "INTERNAL",
		ProviderError: providerErr,
	}, "req_gem")
	if status != 500 {
		t.Fatalf("status=%d", status)
	}
	if ce.Type != core.ErrAPI {
		t.Fatalf("type=%q", ce.Type)
	}
	if ce.Message != "backend failure" {
		t.Fatalf("message=%q", ce.Message)
	}
	if ce.Code != "INTERNAL" {
		t.Fatalf("code=%q", ce.Code)
	}
	if ce.RequestID != "req_gem" {
		t.Fatalf("request_id=%q", ce.RequestID)
	}
	if ce.ProviderError == nil {
		t.Fatal("provider_error should be preserved")
	}
}
