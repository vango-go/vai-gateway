package components

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	chatTransportSSE       = "sse"
	chatTransportWebsocket = "websocket"
)

type chatRunPlan struct {
	SeedHistory []types.Message
	Input       []types.ContentBlock
	Forked      bool
}

type managedMessageMeta struct {
	KeySource services.KeySource
	CreatedAt string
}

func normalizeChatTransport(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case chatTransportWebsocket:
		return chatTransportWebsocket
	default:
		return chatTransportSSE
	}
}

func newDraftConversationID() string {
	return services.NewExternalSessionID("chat")
}

func managedChatMessageID(index int) string {
	return fmt.Sprintf("managed_%d", index)
}

func managedChatMessageIndex(id string) (int, bool) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "managed_") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(id, "managed_"))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func chatMessagesViewManaged(ctx context.Context, orgID string, detail *services.ManagedConversationDetail) ([]map[string]any, error) {
	if detail == nil {
		return nil, nil
	}
	metaByIndex := managedMessageMetadata(detail)
	assetCache := make(map[string]*services.ManagedAssetInfo)
	out := make([]map[string]any, 0, len(detail.History))
	for i, msg := range detail.History {
		attachments, err := managedMessageAttachments(ctx, orgID, msg, assetCache)
		if err != nil {
			return nil, err
		}
		meta := metaByIndex[i]
		entry := map[string]any{
			"id":          managedChatMessageID(i),
			"role":        msg.Role,
			"text":        managedMessageText(msg),
			"attachments": attachments,
		}
		if meta.KeySource != "" {
			entry["keySource"] = string(meta.KeySource)
		}
		if meta.CreatedAt != "" {
			entry["createdAt"] = meta.CreatedAt
		}
		out = append(out, entry)
	}
	return out, nil
}

func managedMessageMetadata(detail *services.ManagedConversationDetail) map[int]managedMessageMeta {
	result := make(map[int]managedMessageMeta)
	if detail == nil || len(detail.History) == 0 || len(detail.Runs) == 0 {
		return result
	}
	runs := append([]types.ChainRunRecord(nil), detail.Runs...)
	// Managed runs append at the tail of chain history. Walk backward so forked
	// chains with seeded history still annotate the latest run suffix correctly.
	sortRunsDescending(runs)
	historyIndex := len(detail.History) - 1
	for _, run := range runs {
		for historyIndex >= 0 && detail.History[historyIndex].Role != "assistant" {
			historyIndex--
		}
		if historyIndex < 0 {
			break
		}
		obs := managedObservabilityMeta(run.Metadata)
		assistantMeta := managedMessageMeta{
			KeySource: services.KeySource(strings.TrimSpace(obs["key_source"])),
		}
		if run.CompletedAt != nil {
			assistantMeta.CreatedAt = run.CompletedAt.UTC().Format(time.RFC3339)
		} else if !run.StartedAt.IsZero() {
			assistantMeta.CreatedAt = run.StartedAt.UTC().Format(time.RFC3339)
		}
		result[historyIndex] = assistantMeta
		historyIndex--
		for historyIndex >= 0 && detail.History[historyIndex].Role != "user" {
			historyIndex--
		}
		if historyIndex < 0 {
			break
		}
		userMeta := managedMessageMeta{
			KeySource: assistantMeta.KeySource,
		}
		if !run.StartedAt.IsZero() {
			userMeta.CreatedAt = run.StartedAt.UTC().Format(time.RFC3339)
		}
		result[historyIndex] = userMeta
		historyIndex--
	}
	return result
}

func managedObservabilityMeta(metadata map[string]any) map[string]string {
	out := map[string]string{}
	obs, _ := metadata["observability"].(map[string]any)
	for key, value := range obs {
		if s, ok := value.(string); ok {
			out[key] = strings.TrimSpace(s)
		}
	}
	return out
}

func managedMessageText(msg types.Message) string {
	switch value := msg.Content.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		var parts []string
		for _, block := range msg.ContentBlocks() {
			switch typed := block.(type) {
			case types.TextBlock:
				if strings.TrimSpace(typed.Text) != "" {
					parts = append(parts, strings.TrimSpace(typed.Text))
				}
			case *types.TextBlock:
				if typed != nil && strings.TrimSpace(typed.Text) != "" {
					parts = append(parts, strings.TrimSpace(typed.Text))
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
}

func managedMessageAttachments(ctx context.Context, orgID string, msg types.Message, cache map[string]*services.ManagedAssetInfo) ([]map[string]any, error) {
	out := make([]map[string]any, 0, 2)
	for _, block := range msg.ContentBlocks() {
		attachment, err := managedAttachmentPayload(ctx, orgID, block, cache)
		if err != nil {
			return nil, err
		}
		if attachment != nil {
			out = append(out, attachment)
		}
	}
	return out, nil
}

func managedAttachmentPayload(ctx context.Context, orgID string, block types.ContentBlock, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	switch typed := block.(type) {
	case types.ImageBlock:
		return attachmentFromImage(ctx, orgID, typed, cache)
	case *types.ImageBlock:
		if typed == nil {
			return nil, nil
		}
		return attachmentFromImage(ctx, orgID, *typed, cache)
	case types.DocumentBlock:
		return attachmentFromDocument(ctx, orgID, typed, cache)
	case *types.DocumentBlock:
		if typed == nil {
			return nil, nil
		}
		return attachmentFromDocument(ctx, orgID, *typed, cache)
	case types.VideoBlock:
		return attachmentFromVideo(ctx, orgID, typed, cache)
	case *types.VideoBlock:
		if typed == nil {
			return nil, nil
		}
		return attachmentFromVideo(ctx, orgID, *typed, cache)
	case types.AudioBlock:
		return attachmentFromAudio(ctx, orgID, typed, cache)
	case *types.AudioBlock:
		if typed == nil {
			return nil, nil
		}
		return attachmentFromAudio(ctx, orgID, *typed, cache)
	default:
		return nil, nil
	}
}

func attachmentFromImage(ctx context.Context, orgID string, block types.ImageBlock, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	return managedAttachment(ctx, orgID, block.Source.AssetID, block.Source.URL, block.Source.MediaType, "image", cache)
}

func attachmentFromDocument(ctx context.Context, orgID string, block types.DocumentBlock, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	name := strings.TrimSpace(block.Filename)
	if name == "" {
		name = "document"
	}
	return managedAttachment(ctx, orgID, block.Source.AssetID, block.Source.URL, block.Source.MediaType, name, cache)
}

func attachmentFromVideo(ctx context.Context, orgID string, block types.VideoBlock, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	return managedAttachment(ctx, orgID, block.Source.AssetID, "", block.Source.MediaType, "video", cache)
}

func attachmentFromAudio(ctx context.Context, orgID string, block types.AudioBlock, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	return managedAttachment(ctx, orgID, block.Source.AssetID, "", block.Source.MediaType, "audio", cache)
}

func managedAttachment(ctx context.Context, orgID, assetID, directURL, mediaType, fallbackName string, cache map[string]*services.ManagedAssetInfo) (map[string]any, error) {
	assetID = strings.TrimSpace(assetID)
	directURL = strings.TrimSpace(directURL)
	if assetID == "" && directURL == "" {
		return nil, nil
	}
	filename := strings.TrimSpace(fallbackName)
	if filename == "" {
		filename = "attachment"
	}
	sizeBytes := int64(0)
	url := directURL
	if assetID != "" {
		info, ok := cache[assetID]
		if !ok {
			loaded, err := appruntime.Get().Services.ManagedAsset(ctx, orgID, assetID)
			if err != nil {
				return nil, err
			}
			cache[assetID] = loaded
			info = loaded
		}
		if info != nil {
			if strings.TrimSpace(info.URL) != "" {
				url = strings.TrimSpace(info.URL)
			}
			if strings.TrimSpace(info.MediaType) != "" {
				mediaType = strings.TrimSpace(info.MediaType)
			}
			sizeBytes = info.SizeBytes
		}
	}
	return map[string]any{
		"id":          firstNonEmpty(assetID, directURL),
		"filename":    filename,
		"contentType": strings.TrimSpace(mediaType),
		"sizeBytes":   sizeBytes,
		"url":         url,
	}, nil
}

func buildManagedRunPlan(detail *services.ManagedConversationDetail, message string, attachmentIDs []string, regenerate bool, editMessageID string) (chatRunPlan, error) {
	if detail == nil {
		return chatRunPlan{}, fmt.Errorf("conversation detail is required")
	}
	if regenerate {
		index := lastUserHistoryIndex(detail.History)
		if index < 0 {
			return chatRunPlan{}, fmt.Errorf("conversation has no user message to regenerate from")
		}
		user := detail.History[index]
		return chatRunPlan{
			SeedHistory: cloneManagedHistory(detail.History[:index]),
			Input:       cloneContentBlocks(user.ContentBlocks()),
			Forked:      true,
		}, nil
	}
	if editMessageID != "" {
		index, ok := managedChatMessageIndex(editMessageID)
		if !ok || index < 0 || index >= len(detail.History) {
			return chatRunPlan{}, fmt.Errorf("message is not editable")
		}
		edited := detail.History[index]
		if edited.Role != "user" {
			return chatRunPlan{}, fmt.Errorf("only user messages can be edited")
		}
		return chatRunPlan{
			SeedHistory: cloneManagedHistory(detail.History[:index]),
			Input:       replaceEditedUserBlocks(edited, message, attachmentIDs),
			Forked:      true,
		}, nil
	}
	return chatRunPlan{
		SeedHistory: cloneManagedHistory(detail.History),
		Input:       buildUserInputBlocks(message, attachmentIDs),
		Forked:      false,
	}, nil
}

func buildUserInputBlocks(message string, attachmentIDs []string) []types.ContentBlock {
	blocks := make([]types.ContentBlock, 0, len(attachmentIDs)+1)
	if strings.TrimSpace(message) != "" {
		blocks = append(blocks, types.TextBlock{Type: "text", Text: strings.TrimSpace(message)})
	}
	for _, assetID := range attachmentIDs {
		assetID = strings.TrimSpace(assetID)
		if assetID == "" {
			continue
		}
		blocks = append(blocks, types.ImageBlock{
			Type: "image",
			Source: types.ImageSource{
				Type:    types.AssetSourceType,
				AssetID: assetID,
			},
		})
	}
	return blocks
}

func replaceEditedUserBlocks(original types.Message, message string, attachmentIDs []string) []types.ContentBlock {
	blocks := make([]types.ContentBlock, 0, len(original.ContentBlocks())+len(attachmentIDs)+1)
	textAdded := false
	trimmed := strings.TrimSpace(message)
	for _, block := range original.ContentBlocks() {
		switch block.(type) {
		case types.TextBlock:
			if !textAdded && trimmed != "" {
				blocks = append(blocks, types.TextBlock{Type: "text", Text: trimmed})
				textAdded = true
			}
		case *types.TextBlock:
			if !textAdded && trimmed != "" {
				blocks = append(blocks, types.TextBlock{Type: "text", Text: trimmed})
				textAdded = true
			}
		default:
			blocks = append(blocks, block)
		}
	}
	if !textAdded && trimmed != "" {
		blocks = append([]types.ContentBlock{types.TextBlock{Type: "text", Text: trimmed}}, blocks...)
	}
	for _, assetID := range attachmentIDs {
		assetID = strings.TrimSpace(assetID)
		if assetID == "" {
			continue
		}
		blocks = append(blocks, types.ImageBlock{
			Type: "image",
			Source: types.ImageSource{
				Type:    types.AssetSourceType,
				AssetID: assetID,
			},
		})
	}
	return blocks
}

func lastUserHistoryIndex(history []types.Message) int {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return i
		}
	}
	return -1
}

func cloneManagedHistory(history []types.Message) []types.Message {
	if len(history) == 0 {
		return nil
	}
	raw, err := json.Marshal(history)
	if err != nil {
		return append([]types.Message(nil), history...)
	}
	var out []types.Message
	if err := json.Unmarshal(raw, &out); err != nil {
		return append([]types.Message(nil), history...)
	}
	return out
}

func cloneContentBlocks(blocks []types.ContentBlock) []types.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return append([]types.ContentBlock(nil), blocks...)
	}
	decoded, err := types.UnmarshalContentBlocks(raw)
	if err != nil {
		return append([]types.ContentBlock(nil), blocks...)
	}
	return decoded
}

func sortRunsDescending(runs []types.ChainRunRecord) {
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			if runs[j].StartedAt.After(runs[i].StartedAt) {
				runs[i], runs[j] = runs[j], runs[i]
			}
		}
	}
}

func chatCredentialSignature(headers http.Header, keySource services.KeySource) string {
	if len(headers) == 0 {
		return string(keySource)
	}
	type kv struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	items := make([]kv, 0, len(headers)+1)
	items = append(items, kv{Key: "__key_source__", Value: string(keySource)})
	for key, values := range headers {
		key = strings.TrimSpace(strings.ToLower(key))
		if key == "" {
			continue
		}
		value := ""
		for _, candidate := range values {
			if strings.TrimSpace(candidate) != "" {
				value = strings.TrimSpace(candidate)
				break
			}
		}
		if value == "" {
			continue
		}
		items = append(items, kv{Key: key, Value: value})
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return string(keySource)
	}
	return string(raw)
}

func chatRunMetadata(keySource services.KeySource, transport string) map[string]any {
	return map[string]any{
		"observability": map[string]any{
			"request_origin":     "platform_demo_chat",
			"endpoint_kind":      chatEndpointKind(transport),
			"key_source":         string(keySource),
			"access_credential":  string(services.AccessCredentialSessionAuth),
			"selected_transport": normalizeChatTransport(transport),
		},
	}
}

func chatChainMetadata(keySource services.KeySource, transport, externalSessionID string) map[string]any {
	metadata := chatRunMetadata(keySource, transport)
	metadata["external_session_id"] = strings.TrimSpace(externalSessionID)
	return metadata
}

func chatEndpointKind(transport string) string {
	if normalizeChatTransport(transport) == chatTransportWebsocket {
		return "chains_ws"
	}
	return "chains_runs_stream"
}
