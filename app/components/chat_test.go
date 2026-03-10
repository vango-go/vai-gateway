package components

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vango"
	neon "github.com/vango-go/vango-neon"
	"github.com/vango-go/vango/pkg/vtest"
)

func TestChatPageIncludesReadyBalanceInIslandProps(t *testing.T) {
	store := newTestChatPageStore(1250, nil)
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, ChatPage, ChatPageProps{
		Actor:          testActor(),
		ConversationID: "conv_123",
	})

	awaitChatPageResources(h, m)

	rendered := html.UnescapeString(h.HTML(m))
	if !strings.Contains(rendered, `"currentBalanceStatus":"ready"`) {
		t.Fatalf("expected ready balance status in island props, got HTML:\n%s", rendered)
	}
	if !strings.Contains(rendered, `"currentBalanceCents":1250`) {
		t.Fatalf("expected balance cents in island props, got HTML:\n%s", rendered)
	}
}

func TestChatPageMarksBalanceUnavailableWithoutZeroFallback(t *testing.T) {
	store := newTestChatPageStore(0, errors.New("wallet unavailable"))
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, ChatPage, ChatPageProps{
		Actor:          testActor(),
		ConversationID: "conv_123",
	})

	awaitChatPageResources(h, m)

	rendered := html.UnescapeString(h.HTML(m))
	if strings.Contains(rendered, "Something failed") {
		t.Fatalf("expected chat page to stay interactive when balance load fails, got HTML:\n%s", rendered)
	}
	if !strings.Contains(rendered, `"currentBalanceStatus":"unavailable"`) {
		t.Fatalf("expected unavailable balance status in island props, got HTML:\n%s", rendered)
	}
	if strings.Contains(rendered, `"currentBalanceCents":0`) {
		t.Fatalf("expected unavailable balance state without synthetic zero balance, got HTML:\n%s", rendered)
	}
}

func TestChatResolveConversationDataKeepsSameConversationSnapshotDuringLoading(t *testing.T) {
	last := &chatConversationData{
		Detail: &services.ManagedConversationDetail{
			Conversation: services.Conversation{ID: "conv_123"},
		},
		Messages: []map[string]any{{"id": "msg_1"}},
	}

	got, loading, err := chatResolveConversationData("conv_123", vango.Loading, nil, nil, last)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loading {
		t.Fatal("expected stale conversation snapshot to keep page ready during loading")
	}
	if got != last {
		t.Fatalf("conversation snapshot mismatch: got %+v want %+v", got, last)
	}
}

func TestChatResolveConversationDataRejectsDifferentConversationSnapshot(t *testing.T) {
	last := &chatConversationData{
		Detail: &services.ManagedConversationDetail{
			Conversation: services.Conversation{ID: "conv_old"},
		},
	}

	got, loading, err := chatResolveConversationData("conv_new", vango.Loading, nil, nil, last)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no stale snapshot for different conversation, got %+v", got)
	}
	if !loading {
		t.Fatal("expected loading state when only a different conversation snapshot exists")
	}
}

func TestChatResolveConversationsDataKeepsLastReadyListDuringLoading(t *testing.T) {
	last := []services.Conversation{{ID: "conv_123", Title: "Existing chat"}}

	got, loading, err := chatResolveConversationsData(vango.Loading, nil, nil, last)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loading {
		t.Fatal("expected stale conversations list to keep sidebar ready during loading")
	}
	if len(got) != 1 || got[0].ID != "conv_123" {
		t.Fatalf("unexpected conversations snapshot: %+v", got)
	}
}

func TestChatResolveBalanceDataKeepsLastReadyBalanceDuringRefetch(t *testing.T) {
	last := chatBalanceState{
		Status:              chatBalanceStatusReady,
		CurrentBalanceCents: 4200,
	}

	got := chatResolveBalanceData(vango.Loading, chatBalanceState{}, last)
	if got != last {
		t.Fatalf("balance snapshot mismatch: got %+v want %+v", got, last)
	}
}

func TestChatResolveBalanceDataFallsBackToLoadingWithoutReadySnapshot(t *testing.T) {
	got := chatResolveBalanceData(vango.Loading, chatBalanceState{}, chatBalanceState{})
	if got.Status != chatBalanceStatusLoading {
		t.Fatalf("expected loading status, got %+v", got)
	}
	if got.CurrentBalanceCents != 0 {
		t.Fatalf("expected zero cents while loading without snapshot, got %+v", got)
	}
}

func TestBuildManagedRunPlan_RegenerateForksFromPriorHistory(t *testing.T) {
	detail := &services.ManagedConversationDetail{
		History: []types.Message{
			{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			{Role: "user", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: "what is the weather"},
				types.ImageBlock{Type: "image", Source: types.ImageSource{Type: types.AssetSourceType, AssetID: "asset_weather"}},
			}},
			{Role: "assistant", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "sunny"}}},
		},
	}

	plan, err := buildManagedRunPlan(detail, "", nil, true, "")
	if err != nil {
		t.Fatalf("buildManagedRunPlan() error = %v", err)
	}
	if !plan.Forked {
		t.Fatal("expected regenerate plan to fork")
	}
	if got := len(plan.SeedHistory); got != 2 {
		t.Fatalf("len(seed_history)=%d, want 2", got)
	}
	if got := len(plan.Input); got != 2 {
		t.Fatalf("len(input)=%d, want 2", got)
	}
	text, ok := plan.Input[0].(types.TextBlock)
	if !ok || text.Text != "what is the weather" {
		t.Fatalf("regenerate input[0]=%#v", plan.Input[0])
	}
	image, ok := plan.Input[1].(types.ImageBlock)
	if !ok || image.Source.AssetID != "asset_weather" {
		t.Fatalf("regenerate input[1]=%#v", plan.Input[1])
	}
}

func TestBuildManagedRunPlan_EditReusesNonTextBlocksAndAddsNewAttachments(t *testing.T) {
	detail := &services.ManagedConversationDetail{
		History: []types.Message{
			{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			{Role: "user", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: "draft"},
				types.ImageBlock{Type: "image", Source: types.ImageSource{Type: types.AssetSourceType, AssetID: "asset_existing"}},
			}},
		},
	}

	plan, err := buildManagedRunPlan(detail, "edited copy", []string{"asset_new"}, false, managedChatMessageID(2))
	if err != nil {
		t.Fatalf("buildManagedRunPlan() error = %v", err)
	}
	if !plan.Forked {
		t.Fatal("expected edit plan to fork")
	}
	if got := len(plan.SeedHistory); got != 2 {
		t.Fatalf("len(seed_history)=%d, want 2", got)
	}
	if got := len(plan.Input); got != 3 {
		t.Fatalf("len(input)=%d, want 3", got)
	}
	text, ok := plan.Input[0].(types.TextBlock)
	if !ok || text.Text != "edited copy" {
		t.Fatalf("edit input[0]=%#v", plan.Input[0])
	}
	existing, ok := plan.Input[1].(types.ImageBlock)
	if !ok || existing.Source.AssetID != "asset_existing" {
		t.Fatalf("edit input[1]=%#v", plan.Input[1])
	}
	added, ok := plan.Input[2].(types.ImageBlock)
	if !ok || added.Source.AssetID != "asset_new" {
		t.Fatalf("edit input[2]=%#v", plan.Input[2])
	}
}

func awaitChatPageResources(h *vtest.Harness, m *vtest.Mounted) {
	h.AwaitResource(m, "staticData")
	h.AwaitResource(m, "conversations")
	h.AwaitResource(m, "conversationData")
	h.AwaitResource(m, "balanceData")
}

type testChatPageStore struct {
	balance    int64
	balanceErr error
	now        time.Time
}

func newTestChatPageStore(balance int64, balanceErr error) *testChatPageStore {
	return &testChatPageStore{
		balance:    balance,
		balanceErr: balanceErr,
		now:        time.Date(2026, time.March, 7, 12, 0, 0, 0, time.UTC),
	}
}

func (s *testChatPageStore) db() *neon.TestDB {
	return &neon.TestDB{
		QueryRowFunc: func(_ context.Context, sql string, _ ...any) pgx.Row {
			switch {
			case strings.Contains(sql, "FROM app_orgs"):
				return newChatTestRow("org_123", "Test Org", true, true, "oai-resp/gpt-5-mini")
			case strings.Contains(sql, "FROM vai_sessions") && strings.Contains(sql, "external_session_id = $2"):
				return newChatTestRow(
					"sess_123",
					"org_123",
					"conv_123",
					"user_123",
					"app_user",
					"",
					`{}`,
					s.now,
					s.now,
					"",
				)
			case strings.Contains(sql, "FROM vai_chains") && strings.Contains(sql, "WHERE id = $1"):
				return newChatTestRow(
					"chain_123",
					"org_123",
					"sess_123",
					"conv_123",
					"user_123",
					"app_user",
					"",
					"idle",
					1,
					"",
					"",
					1,
					0,
					`{"model":"oai-resp/gpt-5-mini"}`,
					`{}`,
					s.now,
					s.now,
				)
			case strings.Contains(sql, "FROM wallet_ledger"):
				if s.balanceErr != nil {
					return &neon.ErrRow{Err: s.balanceErr}
				}
				return newChatTestRow(s.balance)
			default:
				return &neon.ErrRow{Err: fmt.Errorf("unexpected query row: %s", strings.TrimSpace(sql))}
			}
		},
		QueryFunc: func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
			switch {
			case strings.Contains(sql, "FROM vai_sessions") && strings.Contains(sql, "ORDER BY updated_at DESC"):
				return newTestRows([]any{
					"sess_123",
					"org_123",
					"conv_123",
					"user_123",
					"app_user",
					"",
					`{}`,
					s.now,
					s.now,
					"chain_123",
				}), nil
			case strings.Contains(sql, "FROM vai_chains") && strings.Contains(sql, "WHERE session_id = $1"):
				return newTestRows([]any{
					"chain_123",
					"org_123",
					"sess_123",
					"conv_123",
					"user_123",
					"app_user",
					"",
					"idle",
					1,
					"",
					"",
					1,
					0,
					`{"model":"oai-resp/gpt-5-mini"}`,
					`{}`,
					s.now,
					s.now,
				}), nil
			case strings.Contains(sql, "FROM vai_chain_messages") && strings.Contains(sql, "WHERE chain_id = $1"):
				return newTestRows([]any{
					"user",
					`[{"type":"text","text":"Hello"}]`,
				}), nil
			case strings.Contains(sql, "FROM vai_runs") && strings.Contains(sql, "WHERE chain_id = $1"):
				return newTestRows([]any{
					"run_123",
					"org_123",
					"chain_123",
					"sess_123",
					"",
					"",
					"",
					"openai",
					"oai-resp/gpt-5-mini",
					"completed",
					"end_turn",
					`{"model":"oai-resp/gpt-5-mini"}`,
					`{"input_tokens":1,"output_tokens":1,"total_tokens":2}`,
					`{"observability":{"key_source":"platform_hosted","transport":"sse"}}`,
					1,
					100,
					s.now,
					s.now,
				}), nil
			case strings.Contains(sql, "FROM provider_secrets"):
				return newTestRows(), nil
			default:
				return nil, fmt.Errorf("unexpected query: %s", strings.TrimSpace(sql))
			}
		},
	}
}

type chatTestRow struct {
	values []any
	err    error
}

func newChatTestRow(values ...any) pgx.Row {
	return &chatTestRow{values: append([]any(nil), values...)}
}

func (r *chatTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		r.err = fmt.Errorf("scan dest count %d != row count %d", len(dest), len(r.values))
		return r.err
	}
	for i := range r.values {
		if err := assignTestScanValue(dest[i], r.values[i]); err != nil {
			r.err = err
			return err
		}
	}
	return nil
}
