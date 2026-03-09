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
		Detail: &services.ConversationDetail{
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
		Detail: &services.ConversationDetail{
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
			case strings.Contains(sql, "FROM conversations") && strings.Contains(sql, "WHERE id = $1 AND org_id = $2"):
				return newChatTestRow("conv_123", "Balance check", "oai-resp/gpt-5-mini", "platform_hosted", s.now)
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
			case strings.Contains(sql, "FROM conversations") && strings.Contains(sql, "ORDER BY updated_at DESC"):
				return newTestRows([]any{
					"conv_123",
					"Balance check",
					"oai-resp/gpt-5-mini",
					services.KeySourcePlatformHosted,
					s.now,
				}), nil
			case strings.Contains(sql, "FROM conversation_messages"):
				return newTestRows([]any{
					"msg_123",
					"user",
					"Hello",
					services.KeySourcePlatformHosted,
					"",
					"",
					s.now,
				}), nil
			case strings.Contains(sql, "FROM attachments"):
				return newTestRows(), nil
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
