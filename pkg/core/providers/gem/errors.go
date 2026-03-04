package gem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// ErrorType categorizes provider errors.
type ErrorType string

const (
	ErrInvalidRequest ErrorType = "invalid_request_error"
	ErrAuthentication ErrorType = "authentication_error"
	ErrPermission     ErrorType = "permission_error"
	ErrNotFound       ErrorType = "not_found_error"
	ErrRateLimit      ErrorType = "rate_limit_error"
	ErrAPI            ErrorType = "api_error"
	ErrOverloaded     ErrorType = "overloaded_error"
	ErrProvider       ErrorType = "provider_error"
)

// Error is a Gemini provider error mapped to canonical categories.
type Error struct {
	Type          ErrorType `json:"type"`
	Message       string    `json:"message"`
	Code          string    `json:"code,omitempty"`
	ProviderError any       `json:"provider_error,omitempty"`
	RetryAfter    *int      `json:"retry_after,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "gem: <nil>"
	}
	if e.Code != "" {
		return fmt.Sprintf("gem: %s: %s (code: %s)", e.Type, e.Message, e.Code)
	}
	return fmt.Sprintf("gem: %s: %s", e.Type, e.Message)
}

func (e *Error) IsRetryable() bool {
	if e == nil {
		return false
	}
	switch e.Type {
	case ErrRateLimit, ErrOverloaded, ErrAPI:
		return true
	default:
		return false
	}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		typeMapped := classifyError(apiErr.Code, apiErr.Status)
		return &Error{
			Type:          typeMapped,
			Message:       firstNonEmpty(apiErr.Message, err.Error()),
			Code:          firstNonEmpty(apiErr.Status, http.StatusText(apiErr.Code)),
			ProviderError: apiErr,
			RetryAfter:    nil,
		}
	}

	return &Error{
		Type:          ErrProvider,
		Message:       err.Error(),
		ProviderError: err,
	}
}

func classifyError(httpCode int, status string) ErrorType {
	status = strings.ToUpper(strings.TrimSpace(status))

	if httpCode > 0 {
		switch {
		case httpCode == http.StatusBadRequest:
			return ErrInvalidRequest
		case httpCode == http.StatusUnauthorized:
			return ErrAuthentication
		case httpCode == http.StatusForbidden:
			return ErrPermission
		case httpCode == http.StatusNotFound:
			return ErrNotFound
		case httpCode == http.StatusTooManyRequests:
			return ErrRateLimit
		case httpCode >= 500:
			if status == "UNAVAILABLE" || strings.Contains(status, "OVERLOAD") || strings.Contains(status, "UNAVAILABLE") {
				return ErrOverloaded
			}
			return ErrAPI
		case httpCode == http.StatusConflict:
			return ErrAPI
		}
	}

	switch status {
	case "INVALID_ARGUMENT":
		return ErrInvalidRequest
	case "UNAUTHENTICATED":
		return ErrAuthentication
	case "PERMISSION_DENIED":
		return ErrPermission
	case "NOT_FOUND":
		return ErrNotFound
	case "RESOURCE_EXHAUSTED":
		return ErrRateLimit
	case "UNAVAILABLE", "DEADLINE_EXCEEDED":
		return ErrOverloaded
	}
	if strings.Contains(status, "RATE") || strings.Contains(status, "QUOTA") {
		return ErrRateLimit
	}
	if strings.Contains(status, "UNAVAILABLE") {
		return ErrOverloaded
	}
	return ErrProvider
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
