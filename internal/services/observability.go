package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vango-go/vai-lite/pkg/core/types"
	neon "github.com/vango-go/vango-neon"
)

type ValidatedGatewayAPIKey struct {
	Organization Organization
	APIKeyID     string
	APIKeyName   string
	TokenPrefix  string
}

type GatewayRequestFilter struct {
	EndpointKind string
	Model        string
	Status       string
	APIKeyID     string
	SessionID    string
	ChainID      string
	Hours        int
}

type GatewayRequestListEntry struct {
	RequestID         string
	GatewayAPIKeyID   string
	GatewayAPIKeyName string
	GatewayAPIKeyPref string
	SessionID         string
	ChainID           string
	ParentRequestID   string
	EndpointKind      string
	Model             string
	KeySource         KeySource
	AccessCredential  AccessCredential
	StatusCode        int
	DurationMS        int64
	ErrorSummary      string
	StartedAt         time.Time
	CompletedAt       time.Time
}

type GatewayRunToolCall struct {
	ID           string
	StepIndex    int
	ToolCallID   string
	Name         string
	InputJSON    string
	ResultBody   string
	IsError      bool
	ErrorSummary string
}

type GatewayRunStepDetail struct {
	ID              string
	StepIndex       int
	DurationMS      int64
	ResponseBody    string
	ResponseSummary string
	ToolCalls       []GatewayRunToolCall
}

type GatewayRunTraceDetail struct {
	RequestID       string
	SessionID       string
	ChainID         string
	ParentRequestID string
	RunConfig       string
	Usage           string
	StopReason      string
	TurnCount       int
	ToolCallCount   int
	Steps           []GatewayRunStepDetail
}

type GatewayRequestDetail struct {
	RequestID                string
	GatewayAPIKeyID          string
	GatewayAPIKeyName        string
	GatewayAPIKeyPrefix      string
	SessionID                string
	ChainID                  string
	ParentRequestID          string
	EndpointKind             string
	EndpointFamily           string
	Method                   string
	Path                     string
	Provider                 string
	Model                    string
	KeySource                KeySource
	AccessCredential         AccessCredential
	StatusCode               int
	DurationMS               int64
	InputContextFingerprint  string
	OutputContextFingerprint string
	RequestSummary           string
	ResponseSummary          string
	RequestBody              string
	ResponseBody             string
	ErrorSummary             string
	ErrorJSON                string
	StartedAt                time.Time
	CompletedAt              time.Time
	RunTrace                 *GatewayRunTraceDetail
}

type GatewayObservationPrepareInput struct {
	RequestID               string
	OrgID                   string
	GatewayAPIKeyID         string
	SessionID               string
	EndpointFamily          string
	InputContextFingerprint string
}

type PreparedGatewayObservation struct {
	RequestID       string
	SessionID       string
	ChainID         string
	ParentRequestID string
}

type GatewayRunToolCallRecord struct {
	ToolCallID   string
	Name         string
	InputJSON    string
	ResultBody   string
	IsError      bool
	ErrorSummary string
}

type GatewayRunStepRecord struct {
	StepIndex       int
	DurationMS      int64
	ResponseBody    string
	ResponseSummary string
	ToolCalls       []GatewayRunToolCallRecord
}

type GatewayRunTraceRecord struct {
	RunConfig     string
	Usage         string
	StopReason    string
	TurnCount     int
	ToolCallCount int
	Steps         []GatewayRunStepRecord
}

type GatewayObservationRecordInput struct {
	RequestID                string
	OrgID                    string
	GatewayAPIKeyID          string
	SessionID                string
	ChainID                  string
	ParentRequestID          string
	EndpointKind             string
	EndpointFamily           string
	Method                   string
	Path                     string
	Provider                 string
	Model                    string
	KeySource                KeySource
	AccessCredential         AccessCredential
	StatusCode               int
	DurationMS               int64
	InputContextFingerprint  string
	OutputContextFingerprint string
	RequestSummary           string
	ResponseSummary          string
	RequestBody              string
	ResponseBody             string
	ErrorSummary             string
	ErrorJSON                string
	StartedAt                time.Time
	CompletedAt              time.Time
	RunTrace                 *GatewayRunTraceRecord
}

func (s *AppServices) ValidateGatewayAPIKey(ctx context.Context, token string) (*ValidatedGatewayAPIKey, error) {
	hash := hashToken(token)
	row := s.DB.QueryRow(ctx, `
SELECT o.id, o.name, o.allow_byok_override, o.hosted_usage_enabled, o.default_model, g.id, g.name, g.token_prefix
FROM gateway_api_keys g
JOIN app_orgs o ON o.id = g.org_id
WHERE g.token_hash = $1 AND g.revoked_at IS NULL`,
		hash,
	)
	var validated ValidatedGatewayAPIKey
	if err := row.Scan(
		&validated.Organization.ID,
		&validated.Organization.Name,
		&validated.Organization.AllowBYOKOverride,
		&validated.Organization.HostedUsageEnabled,
		&validated.Organization.DefaultModel,
		&validated.APIKeyID,
		&validated.APIKeyName,
		&validated.TokenPrefix,
	); err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(ctx, `UPDATE gateway_api_keys SET last_used_at = now() WHERE id = $1`, validated.APIKeyID)
	return &validated, nil
}

func (s *AppServices) ValidateAPIKey(ctx context.Context, token string) (*Organization, error) {
	validated, err := s.ValidateGatewayAPIKey(ctx, token)
	if err != nil {
		return nil, err
	}
	return &validated.Organization, nil
}

func (s *AppServices) PrepareGatewayObservation(ctx context.Context, in GatewayObservationPrepareInput) (*PreparedGatewayObservation, error) {
	requestID := strings.TrimSpace(in.RequestID)
	if requestID == "" {
		return nil, errors.New("request_id is required")
	}
	chainID := newID("chain")
	parentRequestID := ""
	sessionID := strings.TrimSpace(in.SessionID)
	inputFingerprint := strings.TrimSpace(in.InputContextFingerprint)
	if inputFingerprint != "" {
		var lookupErr error
		switch strings.TrimSpace(in.EndpointFamily) {
		case "runs":
			parentRequestID, chainID, lookupErr = s.lookupManagedObservedParent(ctx, in.OrgID, in.GatewayAPIKeyID, in.EndpointFamily, sessionID, inputFingerprint)
		default:
			parentRequestID, chainID, lookupErr = s.lookupLoggedObservedParent(ctx, in.OrgID, in.GatewayAPIKeyID, in.EndpointFamily, sessionID, inputFingerprint)
		}
		switch {
		case lookupErr == nil:
		case errors.Is(lookupErr, pgx.ErrNoRows):
			chainID = newID("chain")
			parentRequestID = ""
		default:
			return nil, lookupErr
		}
	}
	return &PreparedGatewayObservation{
		RequestID:       requestID,
		SessionID:       sessionID,
		ChainID:         chainID,
		ParentRequestID: parentRequestID,
	}, nil
}

func (s *AppServices) lookupLoggedObservedParent(ctx context.Context, orgID, gatewayAPIKeyID, endpointFamily, sessionID, inputFingerprint string) (string, string, error) {
	if strings.TrimSpace(sessionID) != "" {
		row := s.DB.QueryRow(ctx, `
SELECT request_id, chain_id
FROM gateway_request_logs
WHERE org_id = $1
  AND gateway_api_key_id = $2
  AND endpoint_family = $3
  AND session_id = $4
  AND output_context_fingerprint = $5
ORDER BY completed_at DESC
LIMIT 1`,
			orgID, gatewayAPIKeyID, endpointFamily, sessionID, inputFingerprint,
		)
		var parentRequestID string
		var chainID string
		return parentRequestID, chainID, row.Scan(&parentRequestID, &chainID)
	}
	row := s.DB.QueryRow(ctx, `
SELECT request_id, chain_id
FROM gateway_request_logs
WHERE org_id = $1
  AND gateway_api_key_id = $2
  AND endpoint_family = $3
  AND session_id IS NULL
  AND output_context_fingerprint = $4
  AND completed_at >= now() - interval '24 hours'
ORDER BY completed_at DESC
LIMIT 1`,
		orgID, gatewayAPIKeyID, endpointFamily, inputFingerprint,
	)
	var parentRequestID string
	var chainID string
	return parentRequestID, chainID, row.Scan(&parentRequestID, &chainID)
}

func (s *AppServices) lookupManagedObservedParent(ctx context.Context, orgID, gatewayAPIKeyID, endpointFamily, externalSessionID, inputFingerprint string) (string, string, error) {
	query := `
SELECT r.id, r.chain_id
FROM vai_runs r
LEFT JOIN vai_sessions sess ON sess.id = r.session_id
WHERE r.org_id = $1
  AND COALESCE(r.metadata_json->'observability'->>'gateway_api_key_id', '') = $2
  AND COALESCE(r.metadata_json->'observability'->>'endpoint_family', '') = $3
  AND COALESCE(r.metadata_json->'observability'->>'output_context_fingerprint', '') = $4`
	args := []any{orgID, gatewayAPIKeyID, endpointFamily, inputFingerprint}
	if strings.TrimSpace(externalSessionID) != "" {
		query += `
  AND COALESCE(sess.external_session_id, '') = $5`
		args = append(args, externalSessionID)
	} else {
		query += `
  AND r.session_id IS NULL
  AND r.completed_at >= now() - interval '24 hours'`
	}
	query += `
ORDER BY r.completed_at DESC
LIMIT 1`
	row := s.DB.QueryRow(ctx, query, args...)
	var parentRequestID string
	var chainID string
	return parentRequestID, chainID, row.Scan(&parentRequestID, &chainID)
}

func (s *AppServices) RecordGatewayObservation(ctx context.Context, in GatewayObservationRecordInput) error {
	requestSummary := normalizeJSONText(in.RequestSummary)
	responseSummary := normalizeJSONText(in.ResponseSummary)
	errorJSON := normalizeJSONText(in.ErrorJSON)
	requestBody := strings.TrimSpace(in.RequestBody)
	responseBody := strings.TrimSpace(in.ResponseBody)

	return neon.WithTx(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO gateway_request_logs (
	request_id,
	org_id,
	gateway_api_key_id,
	session_id,
	chain_id,
	parent_request_id,
	endpoint_kind,
	endpoint_family,
	method,
	path,
	provider,
	model,
	key_source,
	access_credential,
	status_code,
	duration_ms,
	input_context_fingerprint,
	output_context_fingerprint,
	request_summary,
	response_summary,
	request_body,
	response_body,
	error_summary,
	error_json,
	started_at,
	completed_at
)
VALUES (
	$1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12, $13, $14,
	$15, $16, NULLIF($17, ''), NULLIF($18, ''), $19::jsonb, $20::jsonb, $21, $22, $23, $24::jsonb, $25, $26
)`,
			in.RequestID, in.OrgID, in.GatewayAPIKeyID, in.SessionID, in.ChainID, in.ParentRequestID,
			in.EndpointKind, in.EndpointFamily, in.Method, in.Path, in.Provider, in.Model,
			string(in.KeySource), string(in.AccessCredential), in.StatusCode, in.DurationMS,
			in.InputContextFingerprint, in.OutputContextFingerprint, requestSummary, responseSummary,
			requestBody, responseBody, strings.TrimSpace(in.ErrorSummary), errorJSON, in.StartedAt, in.CompletedAt,
		); err != nil {
			return err
		}
		if in.RunTrace == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO gateway_run_traces (
	request_id,
	session_id,
	chain_id,
	parent_request_id,
	run_config,
	usage,
	stop_reason,
	turn_count,
	tool_call_count
)
VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5::jsonb, $6::jsonb, $7, $8, $9)`,
			in.RequestID, in.SessionID, in.ChainID, in.ParentRequestID,
			normalizeJSONText(in.RunTrace.RunConfig), normalizeJSONText(in.RunTrace.Usage),
			in.RunTrace.StopReason, in.RunTrace.TurnCount, in.RunTrace.ToolCallCount,
		); err != nil {
			return err
		}
		for _, step := range in.RunTrace.Steps {
			stepID := newID("rstep")
			if _, err := tx.Exec(ctx, `
INSERT INTO gateway_run_steps (id, request_id, step_index, duration_ms, response_body, response_summary)
VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
				stepID, in.RequestID, step.StepIndex, step.DurationMS, strings.TrimSpace(step.ResponseBody), normalizeJSONText(step.ResponseSummary),
			); err != nil {
				return err
			}
			for _, toolCall := range step.ToolCalls {
				if _, err := tx.Exec(ctx, `
INSERT INTO gateway_run_tool_calls (id, request_id, step_index, tool_call_id, name, input_json, result_body, is_error, error_summary)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)`,
					newID("rtcall"), in.RequestID, step.StepIndex, toolCall.ToolCallID, toolCall.Name,
					normalizeJSONText(toolCall.InputJSON), strings.TrimSpace(toolCall.ResultBody), toolCall.IsError, strings.TrimSpace(toolCall.ErrorSummary),
				); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *AppServices) ListGatewayRequestLogs(ctx context.Context, orgID string, filter GatewayRequestFilter) ([]GatewayRequestListEntry, error) {
	args := []any{orgID}
	where := []string{"l.org_id = $1"}

	if endpointKind := strings.TrimSpace(filter.EndpointKind); endpointKind != "" {
		args = append(args, endpointKind)
		where = append(where, fmt.Sprintf("l.endpoint_kind = $%d", len(args)))
	}
	if model := strings.TrimSpace(filter.Model); model != "" {
		args = append(args, model)
		where = append(where, fmt.Sprintf("l.model = $%d", len(args)))
	}
	switch strings.TrimSpace(filter.Status) {
	case "success":
		where = append(where, "l.status_code < 400 AND l.error_summary = ''")
	case "error":
		where = append(where, "(l.status_code >= 400 OR l.error_summary <> '')")
	}
	if apiKeyID := strings.TrimSpace(filter.APIKeyID); apiKeyID != "" {
		args = append(args, apiKeyID)
		where = append(where, fmt.Sprintf("l.gateway_api_key_id = $%d", len(args)))
	}
	if sessionID := strings.TrimSpace(filter.SessionID); sessionID != "" {
		args = append(args, sessionID)
		where = append(where, fmt.Sprintf("l.session_id = $%d", len(args)))
	}
	if chainID := strings.TrimSpace(filter.ChainID); chainID != "" {
		args = append(args, chainID)
		where = append(where, fmt.Sprintf("l.chain_id = $%d", len(args)))
	}
	if filter.Hours > 0 {
		args = append(args, filter.Hours)
		where = append(where, fmt.Sprintf("l.completed_at >= now() - ($%d::int * interval '1 hour')", len(args)))
	}

	query := `
SELECT
	l.request_id,
	l.gateway_api_key_id,
	k.name,
	k.token_prefix,
	COALESCE(l.session_id, ''),
	l.chain_id,
	COALESCE(l.parent_request_id, ''),
	l.endpoint_kind,
	l.model,
	l.key_source,
	l.access_credential,
	l.status_code,
	l.duration_ms,
	l.error_summary,
	l.started_at,
	l.completed_at
FROM gateway_request_logs l
JOIN gateway_api_keys k ON k.id = l.gateway_api_key_id
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY l.completed_at DESC
LIMIT 200`

	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GatewayRequestListEntry
	for rows.Next() {
		var item GatewayRequestListEntry
		if err := rows.Scan(
			&item.RequestID,
			&item.GatewayAPIKeyID,
			&item.GatewayAPIKeyName,
			&item.GatewayAPIKeyPref,
			&item.SessionID,
			&item.ChainID,
			&item.ParentRequestID,
			&item.EndpointKind,
			&item.Model,
			&item.KeySource,
			&item.AccessCredential,
			&item.StatusCode,
			&item.DurationMS,
			&item.ErrorSummary,
			&item.StartedAt,
			&item.CompletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *AppServices) GatewayRequestDetail(ctx context.Context, orgID, requestID string) (*GatewayRequestDetail, error) {
	row := s.DB.QueryRow(ctx, `
SELECT
	l.request_id,
	l.gateway_api_key_id,
	k.name,
	k.token_prefix,
	COALESCE(l.session_id, ''),
	l.chain_id,
	COALESCE(l.parent_request_id, ''),
	l.endpoint_kind,
	l.endpoint_family,
	l.method,
	l.path,
	l.provider,
	l.model,
	l.key_source,
	l.access_credential,
	l.status_code,
	l.duration_ms,
	COALESCE(l.input_context_fingerprint, ''),
	COALESCE(l.output_context_fingerprint, ''),
	l.request_summary::text,
	l.response_summary::text,
	l.request_body,
	l.response_body,
	l.error_summary,
	l.error_json::text,
	l.started_at,
	l.completed_at
FROM gateway_request_logs l
JOIN gateway_api_keys k ON k.id = l.gateway_api_key_id
WHERE l.org_id = $1 AND l.request_id = $2`,
		orgID, requestID,
	)
	var detail GatewayRequestDetail
	if err := row.Scan(
		&detail.RequestID,
		&detail.GatewayAPIKeyID,
		&detail.GatewayAPIKeyName,
		&detail.GatewayAPIKeyPrefix,
		&detail.SessionID,
		&detail.ChainID,
		&detail.ParentRequestID,
		&detail.EndpointKind,
		&detail.EndpointFamily,
		&detail.Method,
		&detail.Path,
		&detail.Provider,
		&detail.Model,
		&detail.KeySource,
		&detail.AccessCredential,
		&detail.StatusCode,
		&detail.DurationMS,
		&detail.InputContextFingerprint,
		&detail.OutputContextFingerprint,
		&detail.RequestSummary,
		&detail.ResponseSummary,
		&detail.RequestBody,
		&detail.ResponseBody,
		&detail.ErrorSummary,
		&detail.ErrorJSON,
		&detail.StartedAt,
		&detail.CompletedAt,
	); err != nil {
		return nil, err
	}

	traceRow := s.DB.QueryRow(ctx, `
SELECT session_id, chain_id, COALESCE(parent_request_id, ''), run_config::text, usage::text, stop_reason, turn_count, tool_call_count
FROM gateway_run_traces
WHERE request_id = $1`,
		requestID,
	)
	var trace GatewayRunTraceDetail
	switch err := traceRow.Scan(
		&trace.SessionID,
		&trace.ChainID,
		&trace.ParentRequestID,
		&trace.RunConfig,
		&trace.Usage,
		&trace.StopReason,
		&trace.TurnCount,
		&trace.ToolCallCount,
	); err {
	case nil:
		trace.RequestID = requestID
	case pgx.ErrNoRows:
		return &detail, nil
	default:
		return nil, err
	}

	stepRows, err := s.DB.Query(ctx, `
SELECT id, step_index, duration_ms, response_body, response_summary::text
FROM gateway_run_steps
WHERE request_id = $1
ORDER BY step_index`,
		requestID,
	)
	if err != nil {
		return nil, err
	}
	defer stepRows.Close()

	stepByIndex := make(map[int]*GatewayRunStepDetail)
	for stepRows.Next() {
		var step GatewayRunStepDetail
		if err := stepRows.Scan(&step.ID, &step.StepIndex, &step.DurationMS, &step.ResponseBody, &step.ResponseSummary); err != nil {
			return nil, err
		}
		trace.Steps = append(trace.Steps, step)
		stepByIndex[step.StepIndex] = &trace.Steps[len(trace.Steps)-1]
	}
	if err := stepRows.Err(); err != nil {
		return nil, err
	}

	toolRows, err := s.DB.Query(ctx, `
SELECT id, step_index, tool_call_id, name, input_json::text, result_body, is_error, error_summary
FROM gateway_run_tool_calls
WHERE request_id = $1
ORDER BY step_index, created_at`,
		requestID,
	)
	if err != nil {
		return nil, err
	}
	defer toolRows.Close()

	for toolRows.Next() {
		var toolCall GatewayRunToolCall
		if err := toolRows.Scan(
			&toolCall.ID,
			&toolCall.StepIndex,
			&toolCall.ToolCallID,
			&toolCall.Name,
			&toolCall.InputJSON,
			&toolCall.ResultBody,
			&toolCall.IsError,
			&toolCall.ErrorSummary,
		); err != nil {
			return nil, err
		}
		if step := stepByIndex[toolCall.StepIndex]; step != nil {
			step.ToolCalls = append(step.ToolCalls, toolCall)
		}
	}
	if err := toolRows.Err(); err != nil {
		return nil, err
	}

	detail.RunTrace = &trace
	return &detail, nil
}

func ContextFingerprint(system any, messages []types.Message) (string, error) {
	payload := map[string]any{
		"system":   system,
		"messages": messages,
	}
	raw, err := canonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func AssistantResponseFingerprint(system any, messages []types.Message, response *types.MessageResponse) (string, error) {
	if response == nil {
		return "", nil
	}
	nextMessages := append([]types.Message(nil), messages...)
	nextMessages = append(nextMessages, types.Message{
		Role:    response.Role,
		Content: response.Content,
	})
	return ContextFingerprint(system, nextMessages)
}

func normalizeJSONText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return "{}"
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, decoded); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) error {
	switch value := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		encoded, _ := json.Marshal(value)
		buf.Write(encoded)
	case float64:
		encoded, _ := json.Marshal(value)
		buf.Write(encoded)
	case []any:
		buf.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			buf.Write(encodedKey)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, value[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buf.Write(encoded)
	}
	return nil
}
