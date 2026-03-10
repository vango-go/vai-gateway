package chains

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	neon "github.com/vango-go/vango-neon"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	tableSessions      = "vai_sessions"
	tableChains        = "vai_chains"
	tableChainMessages = "vai_chain_messages"
	tableRuns          = "vai_runs"
	tableRunItems      = "vai_run_items"
	tableAttachments   = "vai_attachments"
	tableIdempotency   = "vai_idempotency_records"
)

type PostgresStore struct {
	db neon.DB
}

func NewPostgresStore(db neon.DB) *PostgresStore {
	if db == nil {
		return nil
	}
	return &PostgresStore{db: db}
}

func (s *PostgresStore) UpsertSession(ctx context.Context, session *types.SessionRecord) error {
	if session == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	metadataJSON, err := marshalJSONValue(session.Metadata, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO `+tableSessions+` (
	id,
	org_id,
	external_session_id,
	created_by_principal_id,
	created_by_principal_type,
	actor_id,
	metadata_json,
	created_at,
	updated_at,
	latest_chain_id
)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7::jsonb, $8, $9, NULLIF($10, ''))
ON CONFLICT (id) DO UPDATE
SET org_id = EXCLUDED.org_id,
	external_session_id = EXCLUDED.external_session_id,
	created_by_principal_id = EXCLUDED.created_by_principal_id,
	created_by_principal_type = EXCLUDED.created_by_principal_type,
	actor_id = EXCLUDED.actor_id,
	metadata_json = EXCLUDED.metadata_json,
	updated_at = EXCLUDED.updated_at,
	latest_chain_id = CASE
		WHEN EXCLUDED.latest_chain_id IS NULL THEN `+tableSessions+`.latest_chain_id
		ELSE EXCLUDED.latest_chain_id
	END`,
		session.ID,
		session.OrgID,
		session.ExternalSessionID,
		session.CreatedByPrincipalID,
		session.CreatedByPrincipalType,
		session.ActorID,
		string(metadataJSON),
		session.CreatedAt.UTC(),
		session.UpdatedAt.UTC(),
		session.LatestChainID,
	)
	return err
}

func (s *PostgresStore) GetSession(ctx context.Context, sessionID string) (*types.SessionRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	record, err := scanSessionRow(s.db.QueryRow(ctx, `
SELECT
	id,
	org_id,
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	metadata_json,
	created_at,
	updated_at,
	COALESCE(latest_chain_id, '')
FROM `+tableSessions+`
WHERE id = $1`, sessionID))
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (s *PostgresStore) ListSessions(ctx context.Context, orgID string) ([]types.SessionRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT
	id,
	org_id,
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	metadata_json,
	created_at,
	updated_at,
	COALESCE(latest_chain_id, '')
FROM `+tableSessions+`
WHERE org_id = $1
ORDER BY updated_at DESC, created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]types.SessionRecord, 0)
	for rows.Next() {
		record, scanErr := scanSessionFromRows(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) GetSessionByExternal(ctx context.Context, orgID, externalSessionID string) (*types.SessionRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	record, err := scanSessionRow(s.db.QueryRow(ctx, `
SELECT
	id,
	org_id,
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	metadata_json,
	created_at,
	updated_at,
	COALESCE(latest_chain_id, '')
FROM `+tableSessions+`
WHERE org_id = $1 AND external_session_id = $2
LIMIT 1`, orgID, externalSessionID))
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (s *PostgresStore) ListSessionChains(ctx context.Context, sessionID string) ([]types.ChainRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT
	id,
	org_id,
	COALESCE(session_id, ''),
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	status,
	chain_version,
	COALESCE(parent_chain_id, ''),
	COALESCE(forked_from_run_id, ''),
	message_count_cached,
	token_estimate_cached,
	current_defaults_json,
	metadata_json,
	created_at,
	updated_at
FROM `+tableChains+`
WHERE session_id = $1
ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]types.ChainRecord, 0)
	for rows.Next() {
		record, scanErr := scanChainFromRows(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) SaveChain(ctx context.Context, chain *types.ChainRecord, history []types.Message) error {
	if chain == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	defaultsJSON, err := marshalJSONValue(chain.Defaults, "{}")
	if err != nil {
		return err
	}
	metadataJSON, err := marshalJSONValue(chain.Metadata, "{}")
	if err != nil {
		return err
	}

	return neon.WithTx(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO `+tableChains+` (
	id,
	org_id,
	session_id,
	external_session_id,
	created_by_principal_id,
	created_by_principal_type,
	actor_id,
	status,
	chain_version,
	parent_chain_id,
	forked_from_run_id,
	message_count_cached,
	token_estimate_cached,
	current_defaults_json,
	metadata_json,
	created_at,
	updated_at
)
VALUES (
	$1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, NULLIF($7, ''), $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12, $13, $14::jsonb, $15::jsonb, $16, $17
)
ON CONFLICT (id) DO UPDATE
SET org_id = EXCLUDED.org_id,
	session_id = EXCLUDED.session_id,
	external_session_id = EXCLUDED.external_session_id,
	created_by_principal_id = EXCLUDED.created_by_principal_id,
	created_by_principal_type = EXCLUDED.created_by_principal_type,
	actor_id = EXCLUDED.actor_id,
	status = EXCLUDED.status,
	chain_version = EXCLUDED.chain_version,
	parent_chain_id = EXCLUDED.parent_chain_id,
	forked_from_run_id = EXCLUDED.forked_from_run_id,
	message_count_cached = EXCLUDED.message_count_cached,
	token_estimate_cached = EXCLUDED.token_estimate_cached,
	current_defaults_json = EXCLUDED.current_defaults_json,
	metadata_json = EXCLUDED.metadata_json,
	updated_at = EXCLUDED.updated_at`,
			chain.ID,
			chain.OrgID,
			chain.SessionID,
			chain.ExternalSessionID,
			chain.CreatedByPrincipalID,
			chain.CreatedByPrincipalType,
			chain.ActorID,
			string(chain.Status),
			chain.ChainVersion,
			chain.ParentChainID,
			chain.ForkedFromRunID,
			chain.MessageCountCached,
			chain.TokenEstimateCached,
			string(defaultsJSON),
			string(metadataJSON),
			chain.CreatedAt.UTC(),
			chain.UpdatedAt.UTC(),
		); err != nil {
			return err
		}
		if chain.SessionID != "" {
			if _, err := tx.Exec(ctx, `
UPDATE `+tableSessions+`
SET latest_chain_id = $2, updated_at = $3
WHERE id = $1`, chain.SessionID, chain.ID, chain.UpdatedAt.UTC()); err != nil {
				return err
			}
		}
		if len(history) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+tableChainMessages+` WHERE chain_id = $1`, chain.ID); err != nil {
			return err
		}
		for i := range history {
			contentJSON, marshalErr := marshalMessageContent(history[i].Content)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO `+tableChainMessages+` (
	id,
	chain_id,
	run_id,
	role,
	sequence_in_chain,
	content_json,
	created_at
)
VALUES ($1, $2, NULL, $3, $4, $5::jsonb, $6)`,
				newDBID("cmsg"),
				chain.ID,
				history[i].Role,
				i+1,
				string(contentJSON),
				chain.CreatedAt.UTC(),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PostgresStore) UpdateChain(ctx context.Context, chain *types.ChainRecord) error {
	if chain == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	defaultsJSON, err := marshalJSONValue(chain.Defaults, "{}")
	if err != nil {
		return err
	}
	metadataJSON, err := marshalJSONValue(chain.Metadata, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
UPDATE `+tableChains+`
SET org_id = $2,
	session_id = NULLIF($3, ''),
	external_session_id = NULLIF($4, ''),
	created_by_principal_id = $5,
	created_by_principal_type = $6,
	actor_id = NULLIF($7, ''),
	status = $8,
	chain_version = $9,
	parent_chain_id = NULLIF($10, ''),
	forked_from_run_id = NULLIF($11, ''),
	message_count_cached = $12,
	token_estimate_cached = $13,
	current_defaults_json = $14::jsonb,
	metadata_json = $15::jsonb,
	updated_at = $16
WHERE id = $1`,
		chain.ID,
		chain.OrgID,
		chain.SessionID,
		chain.ExternalSessionID,
		chain.CreatedByPrincipalID,
		chain.CreatedByPrincipalType,
		chain.ActorID,
		string(chain.Status),
		chain.ChainVersion,
		chain.ParentChainID,
		chain.ForkedFromRunID,
		chain.MessageCountCached,
		chain.TokenEstimateCached,
		string(defaultsJSON),
		string(metadataJSON),
		chain.UpdatedAt.UTC(),
	)
	return err
}

func (s *PostgresStore) GetChain(ctx context.Context, chainID string) (*types.ChainRecord, []types.Message, error) {
	if s == nil || s.db == nil {
		return nil, nil, errors.New("postgres store is not configured")
	}
	record, err := scanChainRow(s.db.QueryRow(ctx, `
SELECT
	id,
	org_id,
	COALESCE(session_id, ''),
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	status,
	chain_version,
	COALESCE(parent_chain_id, ''),
	COALESCE(forked_from_run_id, ''),
	message_count_cached,
	token_estimate_cached,
	current_defaults_json,
	metadata_json,
	created_at,
	updated_at
FROM `+tableChains+`
WHERE id = $1`, chainID))
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.Query(ctx, `
SELECT role, content_json
FROM `+tableChainMessages+`
WHERE chain_id = $1
ORDER BY sequence_in_chain ASC, created_at ASC`, chainID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	messages := make([]types.Message, 0)
	for rows.Next() {
		var role string
		var raw []byte
		if err := rows.Scan(&role, &raw); err != nil {
			return nil, nil, err
		}
		content, err := decodeMessageContent(raw)
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, types.Message{Role: role, Content: content})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return record, messages, nil
}

func (s *PostgresStore) ListChains(ctx context.Context, orgID, sessionID string, unsessionedOnly bool) ([]types.ChainRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	query := `
SELECT
	id,
	org_id,
	COALESCE(session_id, ''),
	COALESCE(external_session_id, ''),
	created_by_principal_id,
	created_by_principal_type,
	COALESCE(actor_id, ''),
	status,
	chain_version,
	COALESCE(parent_chain_id, ''),
	COALESCE(forked_from_run_id, ''),
	message_count_cached,
	token_estimate_cached,
	current_defaults_json,
	metadata_json,
	created_at,
	updated_at
FROM ` + tableChains + `
WHERE org_id = $1`
	args := []any{orgID}
	if sessionID != "" {
		query += ` AND session_id = $2`
		args = append(args, sessionID)
	} else if unsessionedOnly {
		query += ` AND session_id IS NULL`
	}
	query += `
ORDER BY updated_at DESC, created_at DESC`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]types.ChainRecord, 0)
	for rows.Next() {
		record, scanErr := scanChainFromRows(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) ListChainMessages(ctx context.Context, chainID string) ([]types.ChainMessageRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT
	id,
	chain_id,
	COALESCE(run_id, ''),
	role,
	sequence_in_chain,
	content_json,
	created_at
FROM `+tableChainMessages+`
WHERE chain_id = $1
ORDER BY sequence_in_chain ASC, created_at ASC`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]types.ChainMessageRecord, 0)
	for rows.Next() {
		var (
			record types.ChainMessageRecord
			raw    []byte
		)
		if err := rows.Scan(
			&record.ID,
			&record.ChainID,
			&record.RunID,
			&record.Role,
			&record.SequenceInChain,
			&raw,
			&record.CreatedAt,
		); err != nil {
			return nil, err
		}
		content, err := decodeMessageContent(raw)
		if err != nil {
			return nil, err
		}
		record.Content = content
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if _, _, err := s.GetChain(ctx, chainID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *PostgresStore) AppendChainMessages(ctx context.Context, chainID, runID string, messages []types.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	return neon.WithTx(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var current int
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(sequence_in_chain), 0)
FROM `+tableChainMessages+`
WHERE chain_id = $1`, chainID).Scan(&current); err != nil {
			return err
		}
		now := time.Now().UTC()
		for i := range messages {
			contentJSON, marshalErr := marshalMessageContent(messages[i].Content)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO `+tableChainMessages+` (
	id,
	chain_id,
	run_id,
	role,
	sequence_in_chain,
	content_json,
	created_at
)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6::jsonb, $7)`,
				newDBID("cmsg"),
				chainID,
				runID,
				messages[i].Role,
				current+i+1,
				string(contentJSON),
				now,
			); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
UPDATE `+tableChains+`
SET message_count_cached = message_count_cached + $2,
	updated_at = $3
WHERE id = $1`, chainID, len(messages), now)
		return err
	})
}

func (s *PostgresStore) SetChainResumeTokenHash(ctx context.Context, chainID, resumeTokenHash string) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	_, err := s.db.Exec(ctx, `
UPDATE `+tableChains+`
SET resume_token_hash = $2
WHERE id = $1`, chainID, resumeTokenHash)
	return err
}

func (s *PostgresStore) GetChainResumeTokenHash(ctx context.Context, chainID string) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("postgres store is not configured")
	}
	var resumeTokenHash string
	err := s.db.QueryRow(ctx, `
SELECT COALESCE(resume_token_hash, '')
FROM `+tableChains+`
WHERE id = $1`, chainID).Scan(&resumeTokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return resumeTokenHash, err
}

func (s *PostgresStore) CreateRun(ctx context.Context, run *types.ChainRunRecord) error {
	if run == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	effectiveConfigJSON, err := marshalJSONValue(run.EffectiveConfig, "{}")
	if err != nil {
		return err
	}
	usageJSON, err := marshalJSONValue(run.Usage, "{}")
	if err != nil {
		return err
	}
	metadataJSON, err := marshalJSONValue(run.Metadata, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO `+tableRuns+` (
	id,
	org_id,
	chain_id,
	session_id,
	parent_run_id,
	rerun_of_run_id,
	idempotency_key,
	provider,
	model,
	status,
	stop_reason,
	effective_config_json,
	usage_json,
	metadata_json,
	tool_count,
	duration_ms,
	started_at,
	completed_at
)
VALUES (
	$1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14::jsonb, $15, $16, $17, $18
)
ON CONFLICT (id) DO UPDATE
SET org_id = EXCLUDED.org_id,
	chain_id = EXCLUDED.chain_id,
	session_id = EXCLUDED.session_id,
	parent_run_id = EXCLUDED.parent_run_id,
	rerun_of_run_id = EXCLUDED.rerun_of_run_id,
	idempotency_key = EXCLUDED.idempotency_key,
	provider = EXCLUDED.provider,
	model = EXCLUDED.model,
	status = EXCLUDED.status,
	stop_reason = EXCLUDED.stop_reason,
	effective_config_json = EXCLUDED.effective_config_json,
	usage_json = EXCLUDED.usage_json,
	metadata_json = EXCLUDED.metadata_json,
	tool_count = EXCLUDED.tool_count,
	duration_ms = EXCLUDED.duration_ms,
	started_at = EXCLUDED.started_at,
	completed_at = EXCLUDED.completed_at`,
		run.ID,
		run.OrgID,
		run.ChainID,
		run.SessionID,
		run.ParentRunID,
		run.RerunOfRunID,
		run.IdempotencyKey,
		run.Provider,
		run.Model,
		string(run.Status),
		run.StopReason,
		string(effectiveConfigJSON),
		string(usageJSON),
		string(metadataJSON),
		run.ToolCount,
		run.DurationMS,
		run.StartedAt.UTC(),
		nullTime(run.CompletedAt),
	)
	return err
}

func (s *PostgresStore) UpdateRun(ctx context.Context, run *types.ChainRunRecord) error {
	if run == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	effectiveConfigJSON, err := marshalJSONValue(run.EffectiveConfig, "{}")
	if err != nil {
		return err
	}
	usageJSON, err := marshalJSONValue(run.Usage, "{}")
	if err != nil {
		return err
	}
	metadataJSON, err := marshalJSONValue(run.Metadata, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
UPDATE `+tableRuns+`
SET org_id = $2,
	chain_id = $3,
	session_id = NULLIF($4, ''),
	parent_run_id = NULLIF($5, ''),
	rerun_of_run_id = NULLIF($6, ''),
	idempotency_key = NULLIF($7, ''),
	provider = $8,
	model = $9,
	status = $10,
	stop_reason = $11,
	effective_config_json = $12::jsonb,
	usage_json = $13::jsonb,
	metadata_json = $14::jsonb,
	tool_count = $15,
	duration_ms = $16,
	started_at = $17,
	completed_at = $18
WHERE id = $1`,
		run.ID,
		run.OrgID,
		run.ChainID,
		run.SessionID,
		run.ParentRunID,
		run.RerunOfRunID,
		run.IdempotencyKey,
		run.Provider,
		run.Model,
		string(run.Status),
		run.StopReason,
		string(effectiveConfigJSON),
		string(usageJSON),
		string(metadataJSON),
		run.ToolCount,
		run.DurationMS,
		run.StartedAt.UTC(),
		nullTime(run.CompletedAt),
	)
	return err
}

func (s *PostgresStore) GetRun(ctx context.Context, runID string) (*types.ChainRunRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	run, err := scanRunRow(s.db.QueryRow(ctx, `
SELECT
	id,
	org_id,
	chain_id,
	COALESCE(session_id, ''),
	COALESCE(parent_run_id, ''),
	COALESCE(rerun_of_run_id, ''),
	COALESCE(idempotency_key, ''),
	provider,
	model,
	status,
	COALESCE(stop_reason, ''),
	effective_config_json,
	usage_json,
	metadata_json,
	tool_count,
	duration_ms,
	started_at,
	completed_at
FROM `+tableRuns+`
WHERE id = $1`, runID))
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *PostgresStore) ListChainRuns(ctx context.Context, chainID string) ([]types.ChainRunRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT
	id,
	org_id,
	chain_id,
	COALESCE(session_id, ''),
	COALESCE(parent_run_id, ''),
	COALESCE(rerun_of_run_id, ''),
	COALESCE(idempotency_key, ''),
	provider,
	model,
	status,
	COALESCE(stop_reason, ''),
	effective_config_json,
	usage_json,
	metadata_json,
	tool_count,
	duration_ms,
	started_at,
	completed_at
FROM `+tableRuns+`
WHERE chain_id = $1
ORDER BY started_at ASC, id ASC`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]types.ChainRunRecord, 0)
	for rows.Next() {
		record, scanErr := scanRunFromRows(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) AppendRunItems(ctx context.Context, runID string, items []types.RunTimelineItem) error {
	if len(items) == 0 {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	return neon.WithTx(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var chainID string
		if err := tx.QueryRow(ctx, `SELECT chain_id FROM `+tableRuns+` WHERE id = $1`, runID).Scan(&chainID); err != nil {
			return err
		}
		for i := range items {
			contentJSON, err := marshalJSONValue(items[i].Content, "[]")
			if err != nil {
				return err
			}
			toolJSON, err := marshalJSONValue(items[i].Tool, "{}")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO `+tableRunItems+` (
	id,
	run_id,
	chain_id,
	kind,
	step_index,
	step_id,
	execution_id,
	sequence_in_run,
	sequence_in_chain,
	content_json,
	tool_json,
	asset_id,
	created_at
)
VALUES (
	$1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10::jsonb, $11::jsonb, NULLIF($12, ''), $13
)
ON CONFLICT (id) DO UPDATE
SET kind = EXCLUDED.kind,
	step_index = EXCLUDED.step_index,
	step_id = EXCLUDED.step_id,
	execution_id = EXCLUDED.execution_id,
	sequence_in_run = EXCLUDED.sequence_in_run,
	sequence_in_chain = EXCLUDED.sequence_in_chain,
	content_json = EXCLUDED.content_json,
	tool_json = EXCLUDED.tool_json,
	asset_id = EXCLUDED.asset_id,
	created_at = EXCLUDED.created_at`,
				items[i].ID,
				runID,
				chainID,
				items[i].Kind,
				items[i].StepIndex,
				items[i].StepID,
				items[i].ExecutionID,
				items[i].SequenceInRun,
				items[i].SequenceInChain,
				string(contentJSON),
				string(toolJSON),
				items[i].AssetID,
				items[i].CreatedAt.UTC(),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PostgresStore) GetRunTimeline(ctx context.Context, runID string) ([]types.RunTimelineItem, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT
	id,
	kind,
	step_index,
	COALESCE(step_id, ''),
	COALESCE(execution_id, ''),
	sequence_in_run,
	sequence_in_chain,
	content_json,
	tool_json,
	COALESCE(asset_id, ''),
	created_at
FROM `+tableRunItems+`
WHERE run_id = $1
ORDER BY sequence_in_run ASC, created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]types.RunTimelineItem, 0)
	for rows.Next() {
		item, scanErr := scanRunTimelineItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *PostgresStore) SaveEffectiveRequest(ctx context.Context, req *types.EffectiveRequestResponse) error {
	if req == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	raw, err := marshalJSONValue(req, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
UPDATE `+tableRuns+`
SET effective_request_json = $2::jsonb
WHERE id = $1`, req.RunID, string(raw))
	return err
}

func (s *PostgresStore) GetEffectiveRequest(ctx context.Context, runID string) (*types.EffectiveRequestResponse, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	var raw []byte
	if err := s.db.QueryRow(ctx, `
SELECT effective_request_json
FROM `+tableRuns+`
WHERE id = $1`, runID).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) || len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrNotFound
	}
	var resp types.EffectiveRequestResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (s *PostgresStore) SaveAttachment(ctx context.Context, attachment *AttachmentRecord) error {
	if attachment == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	scopeJSON, err := marshalJSONValue(attachment.CredentialScopeJSON, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO `+tableAttachments+` (
	id,
	org_id,
	chain_id,
	principal_id,
	principal_type,
	actor_id,
	attachment_role,
	mode,
	protocol,
	credential_scope_json,
	resume_token_hash,
	node_id,
	close_reason,
	started_at,
	ended_at
)
VALUES (
	$1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10::jsonb, $11, $12, NULLIF($13, ''), $14, $15
)
ON CONFLICT (id) DO UPDATE
SET org_id = EXCLUDED.org_id,
	chain_id = EXCLUDED.chain_id,
	principal_id = EXCLUDED.principal_id,
	principal_type = EXCLUDED.principal_type,
	actor_id = EXCLUDED.actor_id,
	attachment_role = EXCLUDED.attachment_role,
	mode = EXCLUDED.mode,
	protocol = EXCLUDED.protocol,
	credential_scope_json = EXCLUDED.credential_scope_json,
	resume_token_hash = EXCLUDED.resume_token_hash,
	node_id = EXCLUDED.node_id,
	close_reason = EXCLUDED.close_reason,
	started_at = EXCLUDED.started_at,
	ended_at = EXCLUDED.ended_at`,
		attachment.ID,
		attachment.OrgID,
		attachment.ChainID,
		attachment.PrincipalID,
		attachment.PrincipalType,
		attachment.ActorID,
		string(attachment.AttachmentRole),
		string(attachment.Mode),
		attachment.Protocol,
		string(scopeJSON),
		attachment.ResumeTokenHash,
		attachment.NodeID,
		attachment.CloseReason,
		attachment.StartedAt.UTC(),
		nullTime(attachment.EndedAt),
	)
	return err
}

func (s *PostgresStore) CloseAttachment(ctx context.Context, attachmentID, reason string, endedAt time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	_, err := s.db.Exec(ctx, `
UPDATE `+tableAttachments+`
SET close_reason = NULLIF($2, ''),
	ended_at = $3
WHERE id = $1`, attachmentID, reason, endedAt.UTC())
	return err
}

func (s *PostgresStore) SaveIdempotency(ctx context.Context, record *IdempotencyRecord) error {
	if record == nil {
		return nil
	}
	if s == nil || s.db == nil {
		return errors.New("postgres store is not configured")
	}
	resultJSON, err := marshalJSONValue(record.ResultRef, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO `+tableIdempotency+` (
	id,
	org_id,
	principal_id,
	chain_id,
	operation,
	idempotency_key,
	request_hash,
	result_ref_json,
	created_at,
	expires_at
)
VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10
)
ON CONFLICT (org_id, principal_id, chain_id, operation, idempotency_key) DO UPDATE
SET request_hash = EXCLUDED.request_hash,
	result_ref_json = EXCLUDED.result_ref_json,
	expires_at = EXCLUDED.expires_at`,
		record.ID,
		record.OrgID,
		record.PrincipalID,
		record.ChainID,
		record.Operation,
		record.IdempotencyKey,
		record.RequestHash,
		string(resultJSON),
		record.CreatedAt.UTC(),
		record.ExpiresAt.UTC(),
	)
	return err
}

func (s *PostgresStore) GetIdempotency(ctx context.Context, scope IdempotencyScope) (*IdempotencyRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not configured")
	}
	var record IdempotencyRecord
	var resultJSON []byte
	err := s.db.QueryRow(ctx, `
SELECT
	id,
	org_id,
	principal_id,
	chain_id,
	operation,
	idempotency_key,
	request_hash,
	result_ref_json,
	created_at,
	expires_at
FROM `+tableIdempotency+`
WHERE org_id = $1
	AND principal_id = $2
	AND chain_id = $3
	AND operation = $4
	AND idempotency_key = $5
	AND expires_at > now()
LIMIT 1`,
		scope.OrgID,
		scope.PrincipalID,
		scope.ChainID,
		scope.Operation,
		scope.IdempotencyKey,
	).Scan(
		&record.ID,
		&record.OrgID,
		&record.PrincipalID,
		&record.ChainID,
		&record.Operation,
		&record.IdempotencyKey,
		&record.RequestHash,
		&resultJSON,
		&record.CreatedAt,
		&record.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(resultJSON, &record.ResultRef); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanSessionRow(row pgx.Row) (*types.SessionRecord, error) {
	var (
		record       types.SessionRecord
		metadataJSON []byte
	)
	err := row.Scan(
		&record.ID,
		&record.OrgID,
		&record.ExternalSessionID,
		&record.CreatedByPrincipalID,
		&record.CreatedByPrincipalType,
		&record.ActorID,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.LatestChainID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanSessionFromRows(rows pgx.Rows) (*types.SessionRecord, error) {
	var (
		record       types.SessionRecord
		metadataJSON []byte
	)
	if err := rows.Scan(
		&record.ID,
		&record.OrgID,
		&record.ExternalSessionID,
		&record.CreatedByPrincipalID,
		&record.CreatedByPrincipalType,
		&record.ActorID,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.LatestChainID,
	); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanChainRow(row pgx.Row) (*types.ChainRecord, error) {
	var (
		record       types.ChainRecord
		status       string
		defaultsJSON []byte
		metadataJSON []byte
	)
	err := row.Scan(
		&record.ID,
		&record.OrgID,
		&record.SessionID,
		&record.ExternalSessionID,
		&record.CreatedByPrincipalID,
		&record.CreatedByPrincipalType,
		&record.ActorID,
		&status,
		&record.ChainVersion,
		&record.ParentChainID,
		&record.ForkedFromRunID,
		&record.MessageCountCached,
		&record.TokenEstimateCached,
		&defaultsJSON,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	record.Status = types.ChainStatus(status)
	if err := unmarshalJSONValue(defaultsJSON, &record.Defaults); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanChainFromRows(rows pgx.Rows) (*types.ChainRecord, error) {
	var (
		record       types.ChainRecord
		status       string
		defaultsJSON []byte
		metadataJSON []byte
	)
	if err := rows.Scan(
		&record.ID,
		&record.OrgID,
		&record.SessionID,
		&record.ExternalSessionID,
		&record.CreatedByPrincipalID,
		&record.CreatedByPrincipalType,
		&record.ActorID,
		&status,
		&record.ChainVersion,
		&record.ParentChainID,
		&record.ForkedFromRunID,
		&record.MessageCountCached,
		&record.TokenEstimateCached,
		&defaultsJSON,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return nil, err
	}
	record.Status = types.ChainStatus(status)
	if err := unmarshalJSONValue(defaultsJSON, &record.Defaults); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanRunRow(row pgx.Row) (*types.ChainRunRecord, error) {
	var (
		record              types.ChainRunRecord
		status              string
		effectiveConfigJSON []byte
		usageJSON           []byte
		metadataJSON        []byte
		completedAt         *time.Time
	)
	err := row.Scan(
		&record.ID,
		&record.OrgID,
		&record.ChainID,
		&record.SessionID,
		&record.ParentRunID,
		&record.RerunOfRunID,
		&record.IdempotencyKey,
		&record.Provider,
		&record.Model,
		&status,
		&record.StopReason,
		&effectiveConfigJSON,
		&usageJSON,
		&metadataJSON,
		&record.ToolCount,
		&record.DurationMS,
		&record.StartedAt,
		&completedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	record.Status = types.RunStatus(status)
	record.CompletedAt = completedAt
	if err := unmarshalJSONValue(effectiveConfigJSON, &record.EffectiveConfig); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(usageJSON, &record.Usage); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanRunFromRows(rows pgx.Rows) (*types.ChainRunRecord, error) {
	var (
		record              types.ChainRunRecord
		status              string
		effectiveConfigJSON []byte
		usageJSON           []byte
		metadataJSON        []byte
		completedAt         *time.Time
	)
	if err := rows.Scan(
		&record.ID,
		&record.OrgID,
		&record.ChainID,
		&record.SessionID,
		&record.ParentRunID,
		&record.RerunOfRunID,
		&record.IdempotencyKey,
		&record.Provider,
		&record.Model,
		&status,
		&record.StopReason,
		&effectiveConfigJSON,
		&usageJSON,
		&metadataJSON,
		&record.ToolCount,
		&record.DurationMS,
		&record.StartedAt,
		&completedAt,
	); err != nil {
		return nil, err
	}
	record.Status = types.RunStatus(status)
	record.CompletedAt = completedAt
	if err := unmarshalJSONValue(effectiveConfigJSON, &record.EffectiveConfig); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(usageJSON, &record.Usage); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(metadataJSON, &record.Metadata); err != nil {
		return nil, err
	}
	return &record, nil
}

func scanRunTimelineItem(rows pgx.Rows) (*types.RunTimelineItem, error) {
	var (
		item        types.RunTimelineItem
		contentJSON []byte
		toolJSON    []byte
	)
	if err := rows.Scan(
		&item.ID,
		&item.Kind,
		&item.StepIndex,
		&item.StepID,
		&item.ExecutionID,
		&item.SequenceInRun,
		&item.SequenceInChain,
		&contentJSON,
		&toolJSON,
		&item.AssetID,
		&item.CreatedAt,
	); err != nil {
		return nil, err
	}
	if err := unmarshalJSONValue(contentJSON, &item.Content); err != nil {
		return nil, err
	}
	if !isEmptyJSONObject(toolJSON) {
		item.Tool = &types.RunTimelineTool{}
		if err := unmarshalJSONValue(toolJSON, item.Tool); err != nil {
			return nil, err
		}
	}
	return &item, nil
}

func marshalMessageContent(content any) ([]byte, error) {
	if content == nil {
		return []byte(`""`), nil
	}
	return json.Marshal(content)
}

func decodeMessageContent(raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}
	if blocks, err := types.UnmarshalContentBlocks(trimmed); err == nil {
		return blocks, nil
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func marshalJSONValue(value any, emptyFallback string) ([]byte, error) {
	if value == nil {
		return []byte(emptyFallback), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if emptyFallback != "" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []byte(emptyFallback), nil
	}
	return raw, nil
}

func unmarshalJSONValue[T any](raw []byte, dst *T) error {
	if dst == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		var zero T
		*dst = zero
		return nil
	}
	return json.Unmarshal(trimmed, dst)
}

func isEmptyJSONObject(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "{}" || trimmed == "null"
}

func newDBID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return fmt.Sprintf("%s_%x", prefix, buf[:])
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
