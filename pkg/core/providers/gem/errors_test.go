package gem

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/genai"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		code   int
		status string
		want   ErrorType
	}{
		{name: "400", code: 400, want: ErrInvalidRequest},
		{name: "401", code: 401, want: ErrAuthentication},
		{name: "403", code: 403, want: ErrPermission},
		{name: "404", code: 404, want: ErrNotFound},
		{name: "409", code: 409, want: ErrAPI},
		{name: "429", code: 429, want: ErrRateLimit},
		{name: "503 overloaded", code: 503, status: "UNAVAILABLE", want: ErrOverloaded},
		{name: "500 api", code: 500, status: "INTERNAL", want: ErrAPI},
		{name: "grpc invalid", status: "INVALID_ARGUMENT", want: ErrInvalidRequest},
		{name: "grpc unauth", status: "UNAUTHENTICATED", want: ErrAuthentication},
		{name: "grpc exhausted", status: "RESOURCE_EXHAUSTED", want: ErrRateLimit},
		{name: "grpc unavailable", status: "UNAVAILABLE", want: ErrOverloaded},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.code, tc.status); got != tc.want {
				t.Fatalf("classifyError(%d,%q)=%q want %q", tc.code, tc.status, got, tc.want)
			}
		})
	}
}

func TestMapError(t *testing.T) {
	t.Parallel()

	t.Run("api error", func(t *testing.T) {
		err := mapError(genai.APIError{
			Code:    429,
			Message: "quota exceeded",
			Status:  "RESOURCE_EXHAUSTED",
		})
		ge, ok := err.(*Error)
		if !ok {
			t.Fatalf("expected *Error, got %T", err)
		}
		if ge.Type != ErrRateLimit {
			t.Fatalf("type=%q", ge.Type)
		}
		if !ge.IsRetryable() {
			t.Fatalf("rate limit should be retryable")
		}
	})

	t.Run("context canceled passthrough", func(t *testing.T) {
		err := mapError(context.Canceled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled passthrough, got %v", err)
		}
	})

	t.Run("unknown error", func(t *testing.T) {
		err := mapError(errors.New("boom"))
		ge, ok := err.(*Error)
		if !ok {
			t.Fatalf("expected *Error, got %T", err)
		}
		if ge.Type != ErrProvider {
			t.Fatalf("type=%q", ge.Type)
		}
	})
}
