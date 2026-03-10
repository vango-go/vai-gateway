package services

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stripe/stripe-go/v84"
	"github.com/vango-go/vai-lite/pkg/core/types"
	neon "github.com/vango-go/vango-neon"
	wos "github.com/vango-go/vango-workos"
)

type txRecorder struct {
	execCalls []execCall
	execTags  []pgconn.CommandTag
	execErr   error
}

type execCall struct {
	sql  string
	args []any
}

func (t *txRecorder) Begin(_ context.Context) (pgx.Tx, error) { panic("unexpected Begin") }

func (t *txRecorder) Commit(_ context.Context) error { return nil }

func (t *txRecorder) Rollback(_ context.Context) error { return nil }

func (t *txRecorder) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}

func (t *txRecorder) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}

func (t *txRecorder) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }

func (t *txRecorder) Prepare(_ context.Context, _ string, _ string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}

func (t *txRecorder) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execCalls = append(t.execCalls, execCall{sql: sql, args: append([]any(nil), args...)})
	if t.execErr != nil {
		return pgconn.CommandTag{}, t.execErr
	}
	if len(t.execTags) > 0 {
		tag := t.execTags[0]
		t.execTags = t.execTags[1:]
		return tag, nil
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (t *txRecorder) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (t *txRecorder) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	panic("unexpected QueryRow")
}

func (t *txRecorder) Conn() *pgx.Conn { return nil }

func newTxDB(tx pgx.Tx) *neon.TestDB {
	return &neon.TestDB{
		BeginTxFunc: func(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
			return tx, nil
		},
	}
}

func orgRow(orgID string, allowBYOK, hostedUsage bool) pgx.Row {
	return neon.NewRow(orgID, "Test Org", allowBYOK, hostedUsage, "oai-resp/gpt-5-mini")
}

func TestResolveExecutionHeaders_GatewayAPIKeyPrefersExternalBYOK(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return orgRow("org_123", true, true)
			},
		},
	}

	headers := http.Header{}
	headers.Set("X-Provider-Key-OpenAI", "sk-browser")

	out, source, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", headers, "", AccessCredentialGatewayAPIKey)
	if err != nil {
		t.Fatalf("ResolveExecutionHeaders() error = %v", err)
	}
	if source != KeySourceCustomerBYOKExternal {
		t.Fatalf("ResolveExecutionHeaders() source = %q, want %q", source, KeySourceCustomerBYOKExternal)
	}
	if got := out.Get("X-Provider-Key-OpenAI"); got != "sk-browser" {
		t.Fatalf("ResolveExecutionHeaders() header = %q", got)
	}
}

func TestPrepareGatewayObservation_LinksToPriorRequestInSession(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, sql string, args ...any) pgx.Row {
				if !strings.Contains(sql, "FROM vai_runs r") {
					t.Fatalf("unexpected query: %s", sql)
				}
				if got := args[4]; got != "sess_task_123" {
					t.Fatalf("session arg = %#v", got)
				}
				return neon.NewRow("req_parent_123", "chain_parent_123")
			},
		},
	}

	prepared, err := svc.PrepareGatewayObservation(context.Background(), GatewayObservationPrepareInput{
		RequestID:               "req_current_123",
		OrgID:                   "org_123",
		GatewayAPIKeyID:         "gkey_123",
		SessionID:               "sess_task_123",
		EndpointFamily:          "runs",
		InputContextFingerprint: "fingerprint_abc",
	})
	if err != nil {
		t.Fatalf("PrepareGatewayObservation() error = %v", err)
	}
	if prepared.ParentRequestID != "req_parent_123" {
		t.Fatalf("ParentRequestID = %q", prepared.ParentRequestID)
	}
	if prepared.ChainID != "chain_parent_123" {
		t.Fatalf("ChainID = %q", prepared.ChainID)
	}
	if prepared.SessionID != "sess_task_123" {
		t.Fatalf("SessionID = %q", prepared.SessionID)
	}
}

func TestPrepareGatewayObservation_StartsNewChainWithoutSessionMatch(t *testing.T) {
	t.Parallel()

	var sawNoSessionLookup bool
	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, sql string, args ...any) pgx.Row {
				if strings.Contains(sql, "session_id IS NULL") && strings.Contains(sql, "24 hours") {
					sawNoSessionLookup = true
				}
				return &neon.ErrRow{Err: pgx.ErrNoRows}
			},
		},
	}

	prepared, err := svc.PrepareGatewayObservation(context.Background(), GatewayObservationPrepareInput{
		RequestID:               "req_current_456",
		OrgID:                   "org_123",
		GatewayAPIKeyID:         "gkey_123",
		EndpointFamily:          "messages",
		InputContextFingerprint: "fingerprint_def",
	})
	if err != nil {
		t.Fatalf("PrepareGatewayObservation() error = %v", err)
	}
	if !sawNoSessionLookup {
		t.Fatalf("expected no-session chain lookup query")
	}
	if prepared.ParentRequestID != "" {
		t.Fatalf("ParentRequestID = %q, want empty", prepared.ParentRequestID)
	}
	if !strings.HasPrefix(prepared.ChainID, "chain_") {
		t.Fatalf("ChainID = %q, want generated chain id", prepared.ChainID)
	}
}

func TestRecordGatewayObservation_PersistsRequestAndRunTrace(t *testing.T) {
	t.Parallel()

	tx := &txRecorder{}
	svc := &AppServices{
		DB: newTxDB(tx),
	}

	err := svc.RecordGatewayObservation(context.Background(), GatewayObservationRecordInput{
		RequestID:                "req_123",
		OrgID:                    "org_123",
		GatewayAPIKeyID:          "gkey_123",
		SessionID:                "sess_123",
		ChainID:                  "chain_123",
		ParentRequestID:          "req_parent_123",
		EndpointKind:             "runs_stream",
		EndpointFamily:           "runs",
		Method:                   "POST",
		Path:                     "/v1/runs:stream",
		Provider:                 "anthropic",
		Model:                    "anthropic/claude-sonnet-4",
		KeySource:                KeySourcePlatformHosted,
		AccessCredential:         AccessCredentialGatewayAPIKey,
		StatusCode:               200,
		DurationMS:               245,
		InputContextFingerprint:  "input_fp",
		OutputContextFingerprint: "output_fp",
		RequestSummary:           `{"messages_count":2}`,
		ResponseSummary:          `{"stop_reason":"end_turn"}`,
		RequestBody:              `{"request":{"model":"anthropic/claude-sonnet-4"}}`,
		ResponseBody:             `{"result":{"stop_reason":"end_turn"}}`,
		StartedAt:                time.Unix(100, 0).UTC(),
		CompletedAt:              time.Unix(101, 0).UTC(),
		RunTrace: &GatewayRunTraceRecord{
			RunConfig:     `{"max_turns":8}`,
			Usage:         `{"input_tokens":120,"output_tokens":32,"total_tokens":152}`,
			StopReason:    "end_turn",
			TurnCount:     1,
			ToolCallCount: 1,
			Steps: []GatewayRunStepRecord{
				{
					StepIndex:       0,
					DurationMS:      245,
					ResponseBody:    `{"id":"msg_123"}`,
					ResponseSummary: `{"tool_call_count":1}`,
					ToolCalls: []GatewayRunToolCallRecord{
						{
							ToolCallID: "tool_123",
							Name:       "vai_web_search",
							InputJSON:  `{"query":"latest launch"}`,
							ResultBody: `{"content":[{"type":"text","text":"ok"}]}`,
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RecordGatewayObservation() error = %v", err)
	}
	if len(tx.execCalls) != 4 {
		t.Fatalf("len(execCalls) = %d, want 4", len(tx.execCalls))
	}
	if !strings.Contains(tx.execCalls[0].sql, "INSERT INTO gateway_request_logs") {
		t.Fatalf("first exec sql = %q", tx.execCalls[0].sql)
	}
	if !strings.Contains(tx.execCalls[1].sql, "INSERT INTO gateway_run_traces") {
		t.Fatalf("second exec sql = %q", tx.execCalls[1].sql)
	}
	if !strings.Contains(tx.execCalls[2].sql, "INSERT INTO gateway_run_steps") {
		t.Fatalf("third exec sql = %q", tx.execCalls[2].sql)
	}
	if !strings.Contains(tx.execCalls[3].sql, "INSERT INTO gateway_run_tool_calls") {
		t.Fatalf("fourth exec sql = %q", tx.execCalls[3].sql)
	}
}

func TestResolveExecutionHeaders_GatewayAPIKeyUsesPlatformHostedWithoutProviderHeaders(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return orgRow("org_123", false, true)
			},
		},
		platformKeys: map[string]string{
			"openai": "sk-platform-openai",
		},
	}

	out, source, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", nil, "", AccessCredentialGatewayAPIKey)
	if err != nil {
		t.Fatalf("ResolveExecutionHeaders() error = %v", err)
	}
	if source != KeySourcePlatformHosted {
		t.Fatalf("ResolveExecutionHeaders() source = %q, want %q", source, KeySourcePlatformHosted)
	}
	if out == nil {
		t.Fatal("ResolveExecutionHeaders() returned nil headers")
	}
	if got := out.Get("X-Provider-Key-OpenAI"); got != "sk-platform-openai" {
		t.Fatalf("platform header = %q", got)
	}
}

func TestResolveExecutionHeaders_PlatformHostedRequiresConfiguredPlatformKeys(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return orgRow("org_123", false, true)
			},
		},
	}

	_, _, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", nil, KeySourcePlatformHosted, AccessCredentialSessionAuth)
	if err == nil || !strings.Contains(err.Error(), "not configured with upstream provider keys") {
		t.Fatalf("ResolveExecutionHeaders() error = %v", err)
	}
}

func TestResolveExecutionHeaders_UsesWorkspaceVaultKeysForSessionMode(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return orgRow("org_123", true, true)
			},
			QueryFunc: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
				return neon.NewRows([]string{"provider", "object_name"}).AddRow("openai", "vault/openai").Build(), nil
			},
		},
		vaultRead: func(_ context.Context, name string) (string, error) {
			if name != "vault/openai" {
				t.Fatalf("vaultRead name = %q", name)
			}
			return "sk-vault", nil
		},
	}

	out, source, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", nil, KeySourceCustomerBYOKVault, AccessCredentialSessionAuth)
	if err != nil {
		t.Fatalf("ResolveExecutionHeaders() error = %v", err)
	}
	if source != KeySourceCustomerBYOKVault {
		t.Fatalf("ResolveExecutionHeaders() source = %q, want %q", source, KeySourceCustomerBYOKVault)
	}
	if got := out.Get("X-Provider-Key-OpenAI"); got != "sk-vault" {
		t.Fatalf("vault header = %q", got)
	}
}

func TestReservePlatformHostedUsageRejectsDepletedBalance(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return neon.NewRow(int64(0))
			},
		},
		Pricing: DefaultPricingCatalog(),
	}

	err := svc.ReservePlatformHostedUsage(context.Background(), "org_123", "oai-resp/gpt-5-mini")
	if err == nil || !strings.Contains(err.Error(), "depleted") {
		t.Fatalf("ReservePlatformHostedUsage() error = %v, want depleted balance", err)
	}
}

func TestRecordUsageBrowserBYOKDoesNotDebitWallet(t *testing.T) {
	t.Parallel()

	tx := &txRecorder{}
	svc := &AppServices{
		DB:      newTxDB(tx),
		Pricing: DefaultPricingCatalog(),
	}

	err := svc.RecordUsage(context.Background(), "org_123", "conv_123", "msg_123", "chat_stream", "unknown/model", KeySourceCustomerBYOKBrowser, AccessCredentialSessionAuth, types.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}, map[string]any{
		"via": "test",
	})
	if err != nil {
		t.Fatalf("RecordUsage() error = %v", err)
	}
	if len(tx.execCalls) != 1 {
		t.Fatalf("len(execCalls) = %d, want 1", len(tx.execCalls))
	}
	if !strings.Contains(tx.execCalls[0].sql, "INSERT INTO usage_events") {
		t.Fatalf("first exec sql = %q", tx.execCalls[0].sql)
	}
	if got := tx.execCalls[0].args[9]; got != false {
		t.Fatalf("billable arg = %#v, want false", got)
	}
	if got := tx.execCalls[0].args[6]; got != string(AccessCredentialSessionAuth) {
		t.Fatalf("access credential arg = %#v", got)
	}
}

func TestRecordUsagePlatformHostedDebitsWallet(t *testing.T) {
	t.Parallel()

	tx := &txRecorder{}
	svc := &AppServices{
		DB:      newTxDB(tx),
		Pricing: DefaultPricingCatalog(),
	}

	estimate, err := svc.Pricing.Estimate("oai-resp/gpt-5-mini", types.Usage{InputTokens: 1200, OutputTokens: 200, TotalTokens: 1400}, PricingContext{})
	if err != nil {
		t.Fatalf("Estimate() error = %v", err)
	}

	err = svc.RecordUsage(context.Background(), "org_123", "conv_123", "msg_123", "chat_stream", "oai-resp/gpt-5-mini", KeySourcePlatformHosted, AccessCredentialGatewayAPIKey, types.Usage{InputTokens: 1200, OutputTokens: 200, TotalTokens: 1400}, map[string]any{
		"via": "test",
	})
	if err != nil {
		t.Fatalf("RecordUsage() error = %v", err)
	}
	if len(tx.execCalls) != 2 {
		t.Fatalf("len(execCalls) = %d, want 2", len(tx.execCalls))
	}
	if got := tx.execCalls[0].args[9]; got != true {
		t.Fatalf("billable arg = %#v, want true", got)
	}
	if got := tx.execCalls[0].args[6]; got != string(AccessCredentialGatewayAPIKey) {
		t.Fatalf("access credential arg = %#v", got)
	}
	if !strings.Contains(tx.execCalls[1].sql, "INSERT INTO wallet_ledger") {
		t.Fatalf("second exec sql = %q", tx.execCalls[1].sql)
	}
	if got := tx.execCalls[1].args[2]; got != -estimate.BilledCents {
		t.Fatalf("wallet debit = %#v, want %d", got, -estimate.BilledCents)
	}
}

func TestApplyStripeCheckoutCompletedIdempotent(t *testing.T) {
	t.Parallel()

	tx := &txRecorder{
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 0"),
		},
	}
	svc := &AppServices{DB: newTxDB(tx)}

	err := svc.ApplyStripeCheckoutCompleted(context.Background(), "evt_123", "cs_123", "topup_123", "org_123", 2500)
	if err != nil {
		t.Fatalf("ApplyStripeCheckoutCompleted() error = %v", err)
	}
	if len(tx.execCalls) != 1 {
		t.Fatalf("len(execCalls) = %d, want 1", len(tx.execCalls))
	}
	if !strings.Contains(tx.execCalls[0].sql, "processed_stripe_events") {
		t.Fatalf("first exec sql = %q", tx.execCalls[0].sql)
	}
}

func TestCreateTopupCheckoutRejectsInvalidAmount(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		StripeClient: &stripe.Client{},
	}

	_, err := svc.CreateTopupCheckout(context.Background(), UserIdentity{OrgID: "org_123", UserID: "user_123"}, 99)
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("CreateTopupCheckout() error = %v", err)
	}
}

func TestRecordUsagePlatformHostedRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB:      &neon.TestDB{},
		Pricing: DefaultPricingCatalog(),
	}

	err := svc.RecordUsage(context.Background(), "org_123", "", "", "chat_stream", "unknown/model", KeySourcePlatformHosted, AccessCredentialSessionAuth, types.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}, nil)
	if err == nil {
		t.Fatal("RecordUsage() expected error for unknown hosted model")
	}
}

func TestResolveExecutionHeaders_BrowserBYOKHonorsWorkspaceSetting(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return orgRow("org_123", false, false)
			},
		},
	}

	headers := http.Header{}
	headers.Set("X-Provider-Key-Anthropic", "sk-ant")

	_, _, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", headers, KeySourceCustomerBYOKBrowser, AccessCredentialSessionAuth)
	if err == nil {
		t.Fatal("ResolveExecutionHeaders() expected browser BYOK policy error")
	}
}

func TestSanitizeIDReturnsFallbackForEmpty(t *testing.T) {
	t.Parallel()

	if got := sanitizeID("   "); got != "unknown" {
		t.Fatalf("sanitizeID() = %q, want unknown", got)
	}
}

func TestHashTokenChangesWithInput(t *testing.T) {
	t.Parallel()

	if hashToken("a") == hashToken("b") {
		t.Fatal("hashToken() should vary by input")
	}
}

func TestProviderHeaderUnknownProviderEmpty(t *testing.T) {
	t.Parallel()

	if got := providerHeader("not-real"); got != "" {
		t.Fatalf("providerHeader() = %q, want empty", got)
	}
}

func TestMessageTitleShortensLongBodies(t *testing.T) {
	t.Parallel()

	title := messageTitle(strings.Repeat("x", 100))
	if len(title) > 72 {
		t.Fatalf("messageTitle() len = %d, want <= 72", len(title))
	}
}

func TestValidateTopupAmount(t *testing.T) {
	t.Parallel()

	if err := validateTopupAmount(MinTopupAmountCents); err != nil {
		t.Fatalf("validateTopupAmount(min) error = %v", err)
	}
	if err := validateTopupAmount(MinTopupAmountCents - 1); err == nil {
		t.Fatal("validateTopupAmount() expected low amount error")
	}
	if err := validateTopupAmount(MaxTopupAmountCents + 1); err == nil {
		t.Fatal("validateTopupAmount() expected high amount error")
	}
}

func TestNewDefaultsTopupOptionsAndModel(t *testing.T) {
	t.Parallel()

	svc := New(&neon.TestDB{}, "http://localhost:8080", "", "", "", nil, nil, nil, nil)
	if svc.DefaultModel == "" {
		t.Fatal("DefaultModel should not be empty")
	}
	if len(svc.TopupOptions) == 0 {
		t.Fatal("TopupOptions should default")
	}
}

func TestResolveExecutionHeadersPropagatesOrgLookupError(t *testing.T) {
	t.Parallel()

	svc := &AppServices{
		DB: &neon.TestDB{
			QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
				return &neon.ErrRow{Err: errors.New("boom")}
			},
		},
	}

	_, _, err := svc.ResolveExecutionHeaders(context.Background(), "org_123", nil, "", AccessCredentialGatewayAPIKey)
	if err == nil {
		t.Fatal("ResolveExecutionHeaders() expected error")
	}
}

func TestActorFromIdentityFallsBackToDerivedOrg(t *testing.T) {
	t.Parallel()

	actor, err := ActorFromIdentity(&wos.Identity{
		UserID: "user_123",
		Email:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("ActorFromIdentity() error = %v", err)
	}
	if actor.OrgID != "org_user_123" {
		t.Fatalf("ActorFromIdentity() org = %q, want %q", actor.OrgID, "org_user_123")
	}
	if actor.Name != "test@example.com" {
		t.Fatalf("ActorFromIdentity() name = %q, want email fallback", actor.Name)
	}
}
