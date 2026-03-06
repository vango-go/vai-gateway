package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/mw"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

// ServerToolsHandler handles POST /v1/server-tools:execute.
type ServerToolsHandler struct {
	Config     config.Config
	HTTPClient *http.Client
}

func (h ServerToolsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID, _ := mw.RequestIDFrom(r.Context())
	if r.Method != http.MethodPost {
		writeCoreErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "method not allowed",
			Code:      "method_not_allowed",
			RequestID: reqID,
		}, http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.Config.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeCoreErrorJSON(w, reqID, core.NewInvalidRequestError("failed to read request body"), http.StatusBadRequest)
		return
	}

	req, err := types.UnmarshalServerToolExecuteRequestStrict(body)
	if err != nil {
		h.writeErr(w, reqID, err)
		return
	}

	registry, err := newServerToolsRegistry(h.Config, h.HTTPClient, r, []string{req.Tool}, req.ServerToolConfig)
	if err != nil {
		h.writeErr(w, reqID, err)
		return
	}

	content, toolErr := registry.Execute(r.Context(), req.Tool, req.Input)
	if toolErr != nil {
		h.writeErr(w, reqID, typesToolErrToCore(toolErr, reqID))
		return
	}
	if req.Tool == servertools.ToolImage {
		content = servertools.FinalizeImageToolResultContent(content, servertools.NewImageRefRegistry())
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(types.ServerToolExecuteResponse{Content: content})
}

func (h ServerToolsHandler) writeErr(w http.ResponseWriter, reqID string, err error) {
	coreErr, status := coreErrorFrom(err, reqID)
	writeCoreErrorJSON(w, reqID, coreErr, status)
}

func typesToolErrToCore(err *types.Error, reqID string) *core.Error {
	if err == nil {
		return &core.Error{Type: core.ErrAPI, Message: "internal error", RequestID: reqID}
	}
	coreType := core.ErrorType(strings.TrimSpace(err.Type))
	if coreType == "" {
		coreType = core.ErrAPI
	}
	return &core.Error{
		Type:          coreType,
		Message:       err.Message,
		Param:         err.Param,
		Code:          err.Code,
		RequestID:     reqID,
		ProviderError: err.ProviderError,
		RetryAfter:    err.RetryAfter,
	}
}
