package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	chainrt "github.com/vango-go/vai-lite/pkg/gateway/chains"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type ManagedConversationDetail struct {
	Conversation       Conversation
	Session            *types.SessionRecord
	Chain              *types.ChainRecord
	History            []types.Message
	Runs               []types.ChainRunRecord
	LatestRun          *types.ChainRunRecord
	PreferredTransport string
}

type ManagedAssetInfo struct {
	ID          string
	MediaType   string
	SizeBytes   int64
	URL         string
}

func (s *AppServices) ListManagedConversations(ctx context.Context, orgID string) ([]Conversation, error) {
	store, err := s.managedChainStore()
	if err != nil {
		return nil, err
	}
	sessions, err := store.ListSessions(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]Conversation, 0, len(sessions))
	for i := range sessions {
		session := sessions[i]
		if strings.TrimSpace(session.ExternalSessionID) == "" {
			continue
		}
		detail, err := s.ManagedConversation(ctx, orgID, session.ExternalSessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, detail.Conversation)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (s *AppServices) ManagedConversation(ctx context.Context, orgID, externalSessionID string) (*ManagedConversationDetail, error) {
	store, err := s.managedChainStore()
	if err != nil {
		return nil, err
	}
	org, err := s.Org(ctx, orgID)
	if err != nil {
		return nil, err
	}
	externalSessionID = strings.TrimSpace(externalSessionID)
	if externalSessionID == "" {
		return nil, errors.New("external session id is required")
	}

	session, err := store.GetSessionByExternal(ctx, orgID, externalSessionID)
	if err != nil && !errors.Is(err, chainrt.ErrNotFound) {
		return nil, err
	}
	if session == nil {
		return &ManagedConversationDetail{
			Conversation: Conversation{
				ID:        externalSessionID,
				Title:     "New chat",
				Model:     org.DefaultModel,
				KeySource: KeySourcePlatformHosted,
				UpdatedAt: time.Now().UTC(),
			},
			History: nil,
		}, nil
	}

	chains, err := store.ListSessionChains(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	chain := latestConversationChain(session, chains)
	if chain == nil {
		return &ManagedConversationDetail{
			Conversation: Conversation{
				ID:        externalSessionID,
				Title:     "New chat",
				Model:     org.DefaultModel,
				KeySource: KeySourcePlatformHosted,
				UpdatedAt: session.UpdatedAt,
			},
			Session: session,
		}, nil
	}
	chainRecord, history, err := store.GetChain(ctx, chain.ID)
	if err != nil {
		return nil, err
	}
	runs, err := store.ListChainRuns(ctx, chain.ID)
	if err != nil {
		return nil, err
	}
	var latestRun *types.ChainRunRecord
	if len(runs) > 0 {
		sort.Slice(runs, func(i, j int) bool {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		})
		latestRun = &runs[0]
	}
	model := chainRecord.Defaults.Model
	if latestRun != nil && strings.TrimSpace(latestRun.Model) != "" {
		model = latestRun.Model
	}
	if strings.TrimSpace(model) == "" {
		model = org.DefaultModel
	}
	keySource := KeySourcePlatformHosted
	preferredTransport := "sse"
	if latestRun != nil {
		obs := metadataMap(latestRun.Metadata, "observability")
		if value := KeySource(metadataString(obs, "key_source")); strings.TrimSpace(string(value)) != "" {
			keySource = value
		}
		switch strings.TrimSpace(metadataString(obs, "transport")) {
		case "websocket":
			preferredTransport = "websocket"
		case "http":
			preferredTransport = "http"
		}
	}
	updatedAt := chainRecord.UpdatedAt
	if latestRun != nil && latestRun.CompletedAt != nil && latestRun.CompletedAt.After(updatedAt) {
		updatedAt = latestRun.CompletedAt.UTC()
	}
	return &ManagedConversationDetail{
		Conversation: Conversation{
			ID:        externalSessionID,
			Title:     conversationTitleFromHistory(history, externalSessionID),
			Model:     model,
			KeySource: keySource,
			UpdatedAt: updatedAt,
		},
		Session:            session,
		Chain:              chainRecord,
		History:            history,
		Runs:               append([]types.ChainRunRecord(nil), runs...),
		LatestRun:          latestRun,
		PreferredTransport: preferredTransport,
	}, nil
}

func (s *AppServices) ManagedAsset(ctx context.Context, orgID, assetID string) (*ManagedAssetInfo, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("managed asset store is not configured")
	}
	var (
		objectKey string
		mediaType string
		sizeBytes int64
	)
	err := s.DB.QueryRow(ctx, `
SELECT object_key, media_type, size_bytes
FROM vai_assets
WHERE org_id = $1 AND id = $2`,
		orgID, assetID,
	).Scan(&objectKey, &mediaType, &sizeBytes)
	if err != nil {
		return nil, err
	}
	url := ""
	if s.BlobStore != nil && strings.TrimSpace(objectKey) != "" {
		signed, signErr := s.BlobStore.PresignGet(ctx, objectKey, 30*time.Minute)
		if signErr != nil {
			return nil, signErr
		}
		url = signed
	}
	return &ManagedAssetInfo{
		ID:        assetID,
		MediaType: mediaType,
		SizeBytes: sizeBytes,
		URL:       url,
	}, nil
}

func latestConversationChain(session *types.SessionRecord, chains []types.ChainRecord) *types.ChainRecord {
	if session != nil && strings.TrimSpace(session.LatestChainID) != "" {
		for i := range chains {
			if chains[i].ID == session.LatestChainID {
				return &chains[i]
			}
		}
	}
	if len(chains) == 0 {
		return nil
	}
	sort.Slice(chains, func(i, j int) bool {
		return chains[i].UpdatedAt.After(chains[j].UpdatedAt)
	})
	return &chains[0]
}

func conversationTitleFromHistory(history []types.Message, fallback string) string {
	for _, msg := range history {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(messageTextContent(msg))
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > 48 {
			return string(runes[:48]) + "…"
		}
		return text
	}
	if strings.TrimSpace(fallback) == "" {
		return "New chat"
	}
	return fallback
}

func messageTextContent(msg types.Message) string {
	switch value := msg.Content.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		var parts []string
		for _, block := range msg.ContentBlocks() {
			if text, ok := block.(types.TextBlock); ok {
				parts = append(parts, strings.TrimSpace(text.Text))
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
}

func (s *AppServices) DebugManagedConversation(ctx context.Context, orgID, externalSessionID string) string {
	detail, err := s.ManagedConversation(ctx, orgID, externalSessionID)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("conversation=%s chain=%s messages=%d", detail.Conversation.ID, chainID(detail.Chain), len(detail.History))
}

func chainID(record *types.ChainRecord) string {
	if record == nil {
		return ""
	}
	return record.ID
}
