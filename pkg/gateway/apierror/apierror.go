package apierror

import (
	"context"
	"errors"
	"net/http"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/providers/anthropic"
	"github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/providers/oai_resp"
	"github.com/vango-go/vai-lite/pkg/core/providers/openai"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type Envelope struct {
	Error *core.Error `json:"error"`
}

func FromError(err error, requestID string) (*core.Error, int) {
	if err == nil {
		return nil, http.StatusOK
	}

	// Context timeouts/cancellation.
	if errors.Is(err, context.DeadlineExceeded) {
		return &core.Error{
			Type:      core.ErrAPI,
			Message:   "request timeout",
			RequestID: requestID,
		}, http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return &core.Error{
			Type:      core.ErrAPI,
			Message:   "request cancelled",
			Code:      "cancelled",
			RequestID: requestID,
		}, http.StatusRequestTimeout
	}

	// Already canonical.
	var coreErr *core.Error
	if errors.As(err, &coreErr) && coreErr != nil {
		out := *coreErr
		out.RequestID = requestID
		return &out, statusFromType(coreErr.Type)
	}

	// Strict decode errors (proxy request bodies).
	var decodeErr *types.StrictDecodeError
	if errors.As(err, &decodeErr) && decodeErr != nil {
		return &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   decodeErr.Message,
			Param:     decodeErr.Param,
			RequestID: requestID,
		}, http.StatusBadRequest
	}

	// Provider errors.
	var anthErr *anthropic.Error
	if errors.As(err, &anthErr) && anthErr != nil {
		return &core.Error{
			Type:          core.ErrorType(anthErr.Type),
			Message:       anthErr.Message,
			Code:          anthErr.Code,
			RequestID:     requestID,
			ProviderError: anthErr.ProviderError,
			RetryAfter:    anthErr.RetryAfter,
		}, statusFromType(core.ErrorType(anthErr.Type))
	}

	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) && openaiErr != nil {
		return &core.Error{
			Type:          core.ErrorType(openaiErr.Type),
			Message:       openaiErr.Message,
			Code:          openaiErr.Code,
			Param:         openaiErr.Param,
			RequestID:     requestID,
			ProviderError: openaiErr.ProviderError,
			RetryAfter:    openaiErr.RetryAfter,
		}, statusFromType(core.ErrorType(openaiErr.Type))
	}

	var oaiRespErr *oai_resp.Error
	if errors.As(err, &oaiRespErr) && oaiRespErr != nil {
		return &core.Error{
			Type:          core.ErrorType(oaiRespErr.Type),
			Message:       oaiRespErr.Message,
			Code:          oaiRespErr.Code,
			Param:         oaiRespErr.Param,
			RequestID:     requestID,
			ProviderError: oaiRespErr.ProviderError,
			RetryAfter:    oaiRespErr.RetryAfter,
		}, statusFromType(core.ErrorType(oaiRespErr.Type))
	}

	var gemErr *gem.Error
	if errors.As(err, &gemErr) && gemErr != nil {
		return &core.Error{
			Type:          core.ErrorType(gemErr.Type),
			Message:       gemErr.Message,
			Code:          gemErr.Code,
			RequestID:     requestID,
			ProviderError: gemErr.ProviderError,
			RetryAfter:    gemErr.RetryAfter,
		}, statusFromType(core.ErrorType(gemErr.Type))
	}

	// Unknown errors: treat as internal API error (do not leak details by default).
	return &core.Error{
		Type:      core.ErrAPI,
		Message:   "internal error",
		RequestID: requestID,
	}, http.StatusInternalServerError
}

func statusFromType(t core.ErrorType) int {
	switch t {
	case core.ErrInvalidRequest:
		return http.StatusBadRequest
	case core.ErrAuthentication:
		return http.StatusUnauthorized
	case core.ErrPermission:
		return http.StatusForbidden
	case core.ErrNotFound:
		return http.StatusNotFound
	case core.ErrRateLimit:
		return http.StatusTooManyRequests
	case core.ErrOverloaded:
		return 529
	case core.ErrProvider:
		return http.StatusBadGateway
	case core.ErrAPI:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
