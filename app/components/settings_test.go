package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/services"
	neon "github.com/vango-go/vango-neon"
	"github.com/vango-go/vango/pkg/vtest"
)

func TestParseUSDCentsValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{name: "whole dollars", in: "25", want: 2500},
		{name: "two decimals", in: "25.50", want: 2550},
		{name: "one decimal", in: "25.5", want: 2550},
		{name: "leading dollar", in: "$1.25", want: 125},
		{name: "commas", in: "1,234.56", want: 123456},
		{name: "fraction only", in: ".99", want: 99},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUSDCentsValue(tt.in)
			if err != nil {
				t.Fatalf("parseUSDCentsValue(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseUSDCentsValue(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseUSDCentsValueRejectsInvalid(t *testing.T) {
	for _, in := range []string{"", "-1.00", "12.345", "abc"} {
		if _, err := parseUSDCentsValue(in); err == nil {
			t.Fatalf("parseUSDCentsValue(%q) expected error", in)
		}
	}
}

func TestHomePageTransitionsFromLoadingToEmptyState(t *testing.T) {
	release := make(chan struct{})
	store := &testConversationStore{release: release}
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, HomePage, HomePageProps{Actor: testActor()})

	if html := h.HTML(m); !strings.Contains(html, "Opening your workspace...") {
		t.Fatalf("expected loading state, got HTML:\n%s", html)
	}

	close(release)
	h.AwaitResource(m, "conversations")

	html := h.HTML(m)
	if !strings.Contains(html, "Your workspace is ready") {
		t.Fatalf("expected empty-state title, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "Start a chat") {
		t.Fatalf("expected primary empty-state action, got HTML:\n%s", html)
	}
}

func TestHomePageShowsConversationLoadError(t *testing.T) {
	store := &testConversationStore{queryErr: errors.New("db offline")}
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, HomePage, HomePageProps{Actor: testActor()})

	h.AwaitResource(m, "conversations")
	html := h.HTML(m)
	if !strings.Contains(html, "Something failed") {
		t.Fatalf("expected error panel, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "db offline") {
		t.Fatalf("expected underlying load error, got HTML:\n%s", html)
	}
}

func TestDevelopersPageCreateAndRevokeKey(t *testing.T) {
	now := time.Date(2026, time.March, 7, 12, 0, 0, 0, time.UTC)
	store := newTestAPIKeyStore([]services.APIKeyRecord{
		{
			ID:          "gak_existing",
			Name:        "Existing key",
			TokenPrefix: "vai_sk_existing",
			CreatedAt:   now.Add(-time.Hour),
		},
	})
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, DevelopersPage, SettingsPageProps{Actor: testActor()})

	h.AwaitResource(m, "keys")
	if html := h.HTML(m); !strings.Contains(html, "Existing key") {
		t.Fatalf("expected initial API key list, got HTML:\n%s", html)
	}

	h.InputByTestID(m, "developer-key-name", "Production backend")
	h.SubmitByTestID(m, "developer-key-form")
	h.AwaitAction(m, "createKey")

	html := h.HTML(m)
	if !strings.Contains(html, "API key created") {
		t.Fatalf("expected created-key panel, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "Production backend") {
		t.Fatalf("expected new key name in HTML, got HTML:\n%s", html)
	}
	if got := len(store.snapshot()); got != 2 {
		t.Fatalf("expected 2 keys after create, got %d", got)
	}

	h.ClickByTestID(m, "developer-key-revoke-gak_existing")
	h.AwaitAction(m, "revokeKey")

	html = h.HTML(m)
	if !strings.Contains(html, "Status: revoked") {
		t.Fatalf("expected revoked status after revoke action, got HTML:\n%s", html)
	}

	keys := store.snapshot()
	foundRevoked := false
	for _, key := range keys {
		if key.ID == "gak_existing" && key.RevokedAt != nil {
			foundRevoked = true
			break
		}
	}
	if !foundRevoked {
		t.Fatalf("expected existing key to be revoked, got %#v", keys)
	}
}

func TestDevelopersPage_KeepsLastReadyKeysWhileRevokeRefetchIsPending(t *testing.T) {
	now := time.Date(2026, time.March, 7, 12, 0, 0, 0, time.UTC)
	store := newTestAPIKeyStore([]services.APIKeyRecord{
		{
			ID:          "gak_existing",
			Name:        "Existing key",
			TokenPrefix: "vai_sk_existing",
			CreatedAt:   now.Add(-time.Hour),
		},
	})
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, DevelopersPage, SettingsPageProps{Actor: testActor()})

	h.AwaitResource(m, "keys")
	release := store.blockNextQuery()

	h.ClickByTestID(m, "developer-key-revoke-gak_existing")
	h.AwaitAction(m, "revokeKey")

	html := h.HTML(m)
	if !strings.Contains(html, "Existing key") {
		t.Fatalf("expected last ready key list while refetch pending, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "Status: revoked") {
		t.Fatalf("expected optimistic revoked status while refetch pending, got HTML:\n%s", html)
	}

	close(release)
	h.AwaitResource(m, "keys")

	html = h.HTML(m)
	if !strings.Contains(html, "Status: revoked") {
		t.Fatalf("expected revoked status after refetch completes, got HTML:\n%s", html)
	}
}

func TestObservabilityPageLoadsWithoutBindingMismatch(t *testing.T) {
	now := time.Date(2026, time.March, 10, 16, 0, 0, 0, time.UTC)
	store := newTestObservabilityStore(
		[]services.APIKeyRecord{
			{
				ID:          "gak_existing",
				Name:        "Existing key",
				TokenPrefix: "vai_sk_existing",
				CreatedAt:   now.Add(-2 * time.Hour),
			},
		},
		[]services.GatewayRequestListEntry{
			{
				RequestID:         "req_123",
				GatewayAPIKeyID:   "gak_existing",
				GatewayAPIKeyName: "Existing key",
				GatewayAPIKeyPref: "vai_sk_existing",
				SessionID:         "sess_123",
				ChainID:           "chain_123",
				EndpointKind:      "runs_stream",
				Model:             "oai-resp/gpt-5-mini",
				KeySource:         services.KeySourcePlatformHosted,
				AccessCredential:  services.AccessCredentialGatewayAPIKey,
				StatusCode:        http.StatusOK,
				DurationMS:        842,
				StartedAt:         now.Add(-time.Minute),
				CompletedAt:       now,
			},
		},
	)
	installTestAppRuntime(t, &services.AppServices{
		DB:           store.db(),
		DefaultModel: "oai-resp/gpt-5-mini",
		Pricing:      services.DefaultPricingCatalog(),
	})

	h := vtest.New(t)
	m := vtest.Mount(h, ObservabilityPage, ObservabilityPageProps{Actor: testActor()})

	h.AwaitResource(m, "keys")
	h.AwaitResource(m, "data")

	html := h.HTML(m)
	if !strings.Contains(html, "Gateway observability") {
		t.Fatalf("expected observability title, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "req_123") {
		t.Fatalf("expected request log row, got HTML:\n%s", html)
	}
	if !strings.Contains(html, "Chain chain_123") {
		t.Fatalf("expected chain metadata in row, got HTML:\n%s", html)
	}
}

func installTestAppRuntime(t *testing.T, svc *services.AppServices) {
	t.Helper()
	if svc == nil {
		t.Fatal("services are required")
	}
	if svc.Pricing == nil {
		svc.Pricing = services.DefaultPricingCatalog()
	}
	if strings.TrimSpace(svc.DefaultModel) == "" {
		svc.DefaultModel = "oai-resp/gpt-5-mini"
	}
	appruntime.Set(&appruntime.Deps{
		Services: svc,
		Gateway:  http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: appruntime.Config{
			BaseURL:      "http://localhost:3000",
			DefaultModel: svc.DefaultModel,
			TopupOptions: []int64{1000, 2500, 5000},
		},
	})
}

func testActor() services.UserIdentity {
	return services.UserIdentity{
		UserID: "user_123",
		OrgID:  "org_123",
		Email:  "tester@example.com",
		Name:   "Test User",
	}
}

type testConversationStore struct {
	release  <-chan struct{}
	queryErr error
}

func (s *testConversationStore) db() *neon.TestDB {
	return &neon.TestDB{
		QueryFunc: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			if !strings.Contains(sql, "FROM vai_sessions") {
				return nil, fmt.Errorf("unexpected query: %s", strings.TrimSpace(sql))
			}
			if s.release != nil {
				select {
				case <-s.release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if s.queryErr != nil {
				return nil, s.queryErr
			}
			return newTestRows(), nil
		},
	}
}

type testAPIKeyStore struct {
	mu             sync.Mutex
	keys           []services.APIKeyRecord
	nextQueryBlock chan struct{}
}

func newTestAPIKeyStore(keys []services.APIKeyRecord) *testAPIKeyStore {
	cloned := append([]services.APIKeyRecord(nil), keys...)
	return &testAPIKeyStore{keys: cloned}
}

func (s *testAPIKeyStore) snapshot() []services.APIKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]services.APIKeyRecord(nil), s.keys...)
}

func (s *testAPIKeyStore) blockNextQuery() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.nextQueryBlock = ch
	return ch
}

func (s *testAPIKeyStore) db() *neon.TestDB {
	return &neon.TestDB{
		QueryFunc: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			if !strings.Contains(sql, "FROM gateway_api_keys") {
				return nil, fmt.Errorf("unexpected query: %s", strings.TrimSpace(sql))
			}

			s.mu.Lock()
			wait := s.nextQueryBlock
			s.nextQueryBlock = nil
			snapshot := append([]services.APIKeyRecord(nil), s.keys...)
			s.mu.Unlock()
			if wait != nil {
				select {
				case <-wait:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			sort.Slice(snapshot, func(i, j int) bool {
				return snapshot[i].CreatedAt.After(snapshot[j].CreatedAt)
			})

			rows := make([][]any, 0, len(snapshot))
			for _, key := range snapshot {
				rows = append(rows, []any{
					key.ID,
					key.Name,
					key.TokenPrefix,
					key.CreatedAt,
					key.LastUsedAt,
					key.RevokedAt,
				})
			}
			return newTestRows(rows...), nil
		},
		ExecFunc: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			switch {
			case strings.Contains(sql, "INSERT INTO gateway_api_keys"):
				now := time.Now().UTC()
				record := services.APIKeyRecord{
					ID:          args[0].(string),
					Name:        args[2].(string),
					TokenPrefix: args[4].(string),
					CreatedAt:   now,
				}
				s.mu.Lock()
				s.keys = append(s.keys, record)
				s.mu.Unlock()
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			case strings.Contains(sql, "UPDATE gateway_api_keys"):
				id := args[0].(string)
				now := time.Now().UTC()
				s.mu.Lock()
				defer s.mu.Unlock()
				for i := range s.keys {
					if s.keys[i].ID == id && s.keys[i].RevokedAt == nil {
						s.keys[i].RevokedAt = &now
						return pgconn.NewCommandTag("UPDATE 1"), nil
					}
				}
				return pgconn.NewCommandTag("UPDATE 0"), nil
			default:
				return pgconn.CommandTag{}, fmt.Errorf("unexpected exec: %s", strings.TrimSpace(sql))
			}
		},
	}
}

type testObservabilityStore struct {
	keys []services.APIKeyRecord
	logs []services.GatewayRequestListEntry
}

func newTestObservabilityStore(keys []services.APIKeyRecord, logs []services.GatewayRequestListEntry) *testObservabilityStore {
	return &testObservabilityStore{
		keys: append([]services.APIKeyRecord(nil), keys...),
		logs: append([]services.GatewayRequestListEntry(nil), logs...),
	}
}

func (s *testObservabilityStore) db() *neon.TestDB {
	return &neon.TestDB{
		QueryFunc: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case strings.Contains(sql, "FROM vai_sessions"):
				return newTestRows(), nil
			case strings.Contains(sql, "FROM vai_chains"):
				return newTestRows(), nil
			case strings.Contains(sql, "FROM vai_runs"):
				return newTestRows(), nil
			case strings.Contains(sql, "FROM gateway_request_logs l"):
				rows := make([][]any, 0, len(s.logs))
				for _, item := range s.logs {
					rows = append(rows, []any{
						item.RequestID,
						item.GatewayAPIKeyID,
						item.GatewayAPIKeyName,
						item.GatewayAPIKeyPref,
						item.SessionID,
						item.ChainID,
						item.ParentRequestID,
						item.EndpointKind,
						item.Model,
						item.KeySource,
						item.AccessCredential,
						item.StatusCode,
						item.DurationMS,
						item.ErrorSummary,
						item.StartedAt,
						item.CompletedAt,
					})
				}
				return newTestRows(rows...), nil
			case strings.Contains(sql, "FROM gateway_api_keys"):
				rows := make([][]any, 0, len(s.keys))
				for _, key := range s.keys {
					rows = append(rows, []any{
						key.ID,
						key.Name,
						key.TokenPrefix,
						key.CreatedAt,
						key.LastUsedAt,
						key.RevokedAt,
					})
				}
				return newTestRows(rows...), nil
			default:
				return nil, fmt.Errorf("unexpected query: %s", strings.TrimSpace(sql))
			}
		},
		QueryRowFunc: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return &neon.ErrRow{Err: pgx.ErrNoRows}
		},
	}
}

type testRows struct {
	data [][]any
	idx  int
	err  error
}

func newTestRows(rows ...[]any) *testRows {
	return &testRows{
		data: rows,
		idx:  -1,
	}
}

func (r *testRows) Close() {}

func (r *testRows) Err() error { return r.err }

func (r *testRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

func (r *testRows) Conn() *pgx.Conn { return nil }

func (r *testRows) RawValues() [][]byte { return nil }

func (r *testRows) FieldDescriptions() []pgconn.FieldDescription { return nil }

func (r *testRows) Next() bool {
	r.idx++
	return r.idx >= 0 && r.idx < len(r.data)
}

func (r *testRows) Scan(dest ...any) error {
	if r.idx < 0 || r.idx >= len(r.data) {
		return pgx.ErrNoRows
	}
	row := r.data[r.idx]
	if len(dest) != len(row) {
		r.err = fmt.Errorf("scan dest count %d != row count %d", len(dest), len(row))
		return r.err
	}
	for i := range row {
		if err := assignTestScanValue(dest[i], row[i]); err != nil {
			r.err = err
			return err
		}
	}
	return nil
}

func (r *testRows) Values() ([]any, error) {
	if r.idx < 0 || r.idx >= len(r.data) {
		return nil, pgx.ErrNoRows
	}
	return append([]any(nil), r.data[r.idx]...), nil
}

func assignTestScanValue(dest any, src any) error {
	dstValue := reflect.ValueOf(dest)
	if !dstValue.IsValid() || dstValue.Kind() != reflect.Pointer || dstValue.IsNil() {
		return fmt.Errorf("scan target must be a non-nil pointer, got %T", dest)
	}
	return assignTestValue(dstValue.Elem(), src)
}

func assignTestValue(dst reflect.Value, src any) error {
	if !dst.CanSet() {
		return fmt.Errorf("scan target %s is not settable", dst.Type())
	}
	if src == nil {
		dst.Set(reflect.Zero(dst.Type()))
		return nil
	}

	srcValue := reflect.ValueOf(src)
	for srcValue.Kind() == reflect.Interface {
		if srcValue.IsNil() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		srcValue = srcValue.Elem()
	}

	if dst.Kind() == reflect.Pointer {
		if srcValue.Kind() == reflect.Pointer {
			if srcValue.IsNil() {
				dst.Set(reflect.Zero(dst.Type()))
				return nil
			}
			srcValue = srcValue.Elem()
		}
		elemType := dst.Type().Elem()
		if srcValue.Type().AssignableTo(elemType) {
			next := reflect.New(elemType)
			next.Elem().Set(srcValue)
			dst.Set(next)
			return nil
		}
		if srcValue.Type().ConvertibleTo(elemType) {
			next := reflect.New(elemType)
			next.Elem().Set(srcValue.Convert(elemType))
			dst.Set(next)
			return nil
		}
		return fmt.Errorf("cannot assign %s to %s", srcValue.Type(), dst.Type())
	}

	if srcValue.Kind() == reflect.Pointer {
		if srcValue.IsNil() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		srcValue = srcValue.Elem()
	}
	if srcValue.Type().AssignableTo(dst.Type()) {
		dst.Set(srcValue)
		return nil
	}
	if srcValue.Type().ConvertibleTo(dst.Type()) {
		dst.Set(srcValue.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %s to %s", srcValue.Type(), dst.Type())
}
