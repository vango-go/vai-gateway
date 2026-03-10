package components

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/chatruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
)

type chatIslandClientMessage struct {
	Type          string            `json:"type"`
	RequestID     string            `json:"requestId,omitempty"`
	Message       string            `json:"message,omitempty"`
	Model         string            `json:"model,omitempty"`
	KeySource     string            `json:"keySource,omitempty"`
	Transport     string            `json:"transport,omitempty"`
	AttachmentIDs []string          `json:"attachmentIds,omitempty"`
	Regenerate    bool              `json:"regenerate,omitempty"`
	EditMessageID string            `json:"editMessageId,omitempty"`
	Filename      string            `json:"filename,omitempty"`
	ContentType   string            `json:"contentType,omitempty"`
	SizeBytes     int64             `json:"sizeBytes,omitempty"`
	IntentToken   string            `json:"intentToken,omitempty"`
	BrowserKeys   map[string]string `json:"browserKeys,omitempty"`
}

type chatUploadIntentCommand struct {
	HID         string
	RequestID   string
	Filename    string
	ContentType string
	SizeBytes   int64
}

type chatUploadClaimCommand struct {
	HID         string
	RequestID   string
	Filename    string
	ContentType string
	SizeBytes   int64
	IntentToken string
}

type chatSubmitCommand struct {
	HID           string
	RequestID     string
	Message       string
	Model         string
	KeySource     services.KeySource
	Transport     string
	AttachmentIDs []string
	Regenerate    bool
	EditMessageID string
	BrowserKeys   map[string]string
}

type chatServerMessage struct {
	Type       string                 `json:"type"`
	RequestID  string                 `json:"requestId,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Intent     any                    `json:"intent,omitempty"`
	Attachment *chatAttachmentPayload `json:"attachment,omitempty"`
	Delta      string                 `json:"delta,omitempty"`
	Tool       any                    `json:"tool,omitempty"`
	Assistant  *chatAssistantPayload  `json:"assistant,omitempty"`
}

type chatAttachmentPayload struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	URL         string `json:"url,omitempty"`
}

type chatAssistantPayload struct {
	Text      string `json:"text"`
	KeySource string `json:"keySource"`
	CreatedAt string `json:"createdAt"`
	ToolTrace any    `json:"toolTrace,omitempty"`
}

type chatIslandDispatcher interface {
	Dispatch(func())
}

func chatIslandBoundary(props map[string]any, handler func(vango.IslandMessage)) *vango.VNode {
	args := []any{
		Class("chat-island"),
		JSIsland("vai-chat", props),
	}
	if handler != nil {
		args = append(args, OnIslandMessage(handler))
	}
	return Div(args...)
}

func dispatchChatIsland(ctx chatIslandDispatcher, hid string, payload chatServerMessage) {
	if ctx == nil || hid == "" {
		return
	}
	ctx.Dispatch(func() {
		SendToIslandHID(hid, payload)
	})
}

func buildBrowserBYOKHeaders(keys map[string]string) http.Header {
	headers := make(http.Header)
	for provider, value := range keys {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		header := services.ProviderHeader(provider)
		if header == "" {
			continue
		}
		headers.Set(header, value)
	}
	return headers
}

func buildChatAttachmentPayload(ctx context.Context, attachment *services.Attachment) *chatAttachmentPayload {
	if attachment == nil {
		return nil
	}
	url := ""
	if store := appruntime.Get().Services.BlobStore; store != nil {
		signed, err := store.PresignGet(ctx, attachment.BlobRef.Key, 30*time.Minute)
		if err == nil {
			url = signed
		}
	}
	return &chatAttachmentPayload{
		ID:          attachment.ID,
		Filename:    attachment.Filename,
		ContentType: attachment.ContentType,
		SizeBytes:   attachment.SizeBytes,
		URL:         url,
	}
}

func buildChatAssistantPayload(result *types.RunResult, keySource services.KeySource) *chatAssistantPayload {
	if result == nil || result.Response == nil {
		return nil
	}
	return &chatAssistantPayload{
		Text:      result.Response.TextContent(),
		KeySource: string(keySource),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ToolTrace: result.Steps,
	}
}

func relayChatStreamEvent(ctx chatIslandDispatcher, hid, requestID string, event chatruntime.SSEEvent) {
	if ctx == nil || hid == "" || requestID == "" {
		return
	}

	switch event.Event {
	case "tool_call_start", "tool_result", "step_complete":
		var payload any
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			return
		}
		dispatchChatIsland(ctx, hid, chatServerMessage{
			Type:      "chat_tool",
			RequestID: requestID,
			Tool:      payload,
		})
	case "stream_event":
		decoded, err := types.UnmarshalRunStreamEvent([]byte(event.Data))
		if err != nil {
			return
		}
		wrapper, ok := decoded.(types.RunStreamEventWrapper)
		if !ok {
			return
		}
		delta, ok := wrapper.Event.(types.ContentBlockDeltaEvent)
		if !ok {
			return
		}
		textDelta, ok := delta.Delta.(types.TextDelta)
		if !ok || textDelta.Text == "" {
			return
		}
		dispatchChatIsland(ctx, hid, chatServerMessage{
			Type:      "chat_delta",
			RequestID: requestID,
			Delta:     textDelta.Text,
		})
	}
}
