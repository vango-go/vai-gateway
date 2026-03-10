package chains

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func cloneMessages(in []types.Message) []types.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.Message, len(in))
	for i := range in {
		out[i] = types.Message{
			Role:    in[i].Role,
			Content: normalizedMessageContent(in[i].Content),
		}
	}
	return out
}

func normalizedMessageContent(content any) any {
	switch typed := content.(type) {
	case nil:
		return nil
	case string:
		return typed
	case []types.ContentBlock:
		return cloneContentBlocks(typed)
	case types.ContentBlock:
		return cloneContentBlocks([]types.ContentBlock{typed})
	default:
		raw, err := json.Marshal(content)
		if err == nil {
			if blocks, blockErr := types.UnmarshalContentBlocks(raw); blockErr == nil {
				return blocks
			}
			var text string
			if textErr := json.Unmarshal(raw, &text); textErr == nil {
				return text
			}
		}
		return cloneJSON(content)
	}
}

func cloneContentBlocksFromAny(value any) []types.ContentBlock {
	switch typed := normalizedMessageContent(value).(type) {
	case []types.ContentBlock:
		return typed
	case string:
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: typed}}
	default:
		return nil
	}
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

func normalizeToolResultContent(content []types.ContentBlock) []types.ContentBlock {
	cloned := cloneContentBlocks(content)
	if len(cloned) > 0 {
		return cloned
	}
	return []types.ContentBlock{types.TextBlock{Type: "text", Text: ""}}
}

func cloneChainDefaults(defaults types.ChainDefaults) types.ChainDefaults {
	cloned := cloneJSON(defaults)
	cloned.System = normalizedMessageContent(defaults.System)
	return cloned
}

func cloneJSON[T any](value T) T {
	var zero T
	encoded, err := json.Marshal(value)
	if err != nil {
		return zero
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		return zero
	}
	return out
}

func buildChainMessageRecords(chainID, runID string, messages []types.Message, offset int, createdAt time.Time) []types.ChainMessageRecord {
	if len(messages) == 0 {
		return nil
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	records := make([]types.ChainMessageRecord, 0, len(messages))
	for i := range messages {
		records = append(records, types.ChainMessageRecord{
			ID:              fmt.Sprintf("%s_msg_%d", chainID, offset+i+1),
			ChainID:         chainID,
			RunID:           runID,
			Role:            messages[i].Role,
			SequenceInChain: offset + i + 1,
			Content:         normalizedMessageContent(messages[i].Content),
			CreatedAt:       createdAt,
		})
	}
	return records
}
