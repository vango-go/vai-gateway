package gem

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func (p *Provider) translateGenerateContentResponse(model string, resp *genai.GenerateContentResponse, build *requestBuild) (*types.MessageResponse, error) {
	out := &types.MessageResponse{
		ID:    "",
		Type:  "message",
		Role:  "assistant",
		Model: p.prefixedModel(model),
	}
	if resp != nil {
		out.ID = strings.TrimSpace(resp.ResponseID)
	}

	content, stopReason, usage, metadata := translateCandidateResponse(resp, build)
	out.Content = content
	out.StopReason = stopReason
	out.Usage = usage
	if len(metadata) > 0 {
		out.Metadata = metadata
	}
	return out, nil
}

func translateCandidateResponse(resp *genai.GenerateContentResponse, build *requestBuild) ([]types.ContentBlock, types.StopReason, types.Usage, map[string]any) {
	blocks := make([]types.ContentBlock, 0, 8)
	metadata := map[string]any{}
	gemMeta := map[string]any{}
	warnings := append([]string(nil), build.warnings...)

	if resp == nil {
		if len(warnings) > 0 {
			gemMeta["warnings"] = warnings
			metadata["gem"] = gemMeta
		}
		return blocks, types.StopReasonEndTurn, types.Usage{}, metadata
	}

	usage := mapUsage(resp.UsageMetadata)
	candidate := firstCandidate(resp)
	var finishReason string
	if candidate != nil {
		finishReason = string(candidate.FinishReason)
		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				if part == nil {
					continue
				}
				switch {
				case part.FunctionCall != nil:
					blocks = append(blocks, functionCallPartToToolUse(part))
				case part.Text != "" && part.Thought:
					blocks = append(blocks, types.ThinkingBlock{Type: "thinking", Thinking: part.Text})
				case part.Text != "":
					blocks = append(blocks, types.TextBlock{Type: "text", Text: part.Text})
				case part.InlineData != nil:
					b, warn := inlinePartToContentBlock(part)
					if warn != "" {
						warnings = append(warnings, warn)
					}
					if b != nil {
						blocks = append(blocks, b)
					}
				case part.FileData != nil:
					b, warn := filePartToContentBlock(part)
					if warn != "" {
						warnings = append(warnings, warn)
					}
					if b != nil {
						blocks = append(blocks, b)
					}
				case part.ExecutableCode != nil:
					appendGemArray(gemMeta, "executable_code", map[string]any{"language": part.ExecutableCode.Language, "code": part.ExecutableCode.Code})
				case part.CodeExecutionResult != nil:
					appendGemArray(gemMeta, "code_execution_result", map[string]any{"outcome": part.CodeExecutionResult.Outcome, "output": part.CodeExecutionResult.Output})
				}
			}
		}
		if candidate.GroundingMetadata != nil {
			if gm, ok := toJSONMap(candidate.GroundingMetadata); ok {
				gemMeta["grounding"] = gm
			}
		}
	}

	if finishReason != "" {
		gemMeta["finish_reason"] = finishReason
	}

	if resp.UsageMetadata != nil {
		gemMeta["usage"] = map[string]any{
			"cached_content_token_count":  resp.UsageMetadata.CachedContentTokenCount,
			"thoughts_token_count":        resp.UsageMetadata.ThoughtsTokenCount,
			"tool_use_prompt_token_count": resp.UsageMetadata.ToolUsePromptTokenCount,
			"prompt_tokens_details":       convertToJSONAny(resp.UsageMetadata.PromptTokensDetails),
			"candidates_tokens_details":   convertToJSONAny(resp.UsageMetadata.CandidatesTokensDetails),
		}
	}

	if build.ext.CandidateCount > 1 && len(resp.Candidates) > 1 {
		gemMeta["candidates"] = convertToJSONAny(resp.Candidates[1:])
	}

	if len(warnings) > 0 {
		gemMeta["warnings"] = warnings
	}
	if len(gemMeta) > 0 {
		metadata["gem"] = gemMeta
	}

	return blocks, mapStopReason(candidate, blocks), usage, metadata
}

func mapUsage(u *genai.GenerateContentResponseUsageMetadata) types.Usage {
	if u == nil {
		return types.Usage{}
	}
	out := types.Usage{
		InputTokens:  int(u.PromptTokenCount),
		OutputTokens: int(u.CandidatesTokenCount),
		TotalTokens:  int(u.TotalTokenCount),
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

func mapStopReason(candidate *genai.Candidate, blocks []types.ContentBlock) types.StopReason {
	for _, block := range blocks {
		switch block.(type) {
		case types.ToolUseBlock, *types.ToolUseBlock:
			return types.StopReasonToolUse
		}
	}
	if candidate == nil {
		return types.StopReasonEndTurn
	}
	switch candidate.FinishReason {
	case genai.FinishReasonMaxTokens:
		return types.StopReasonMaxTokens
	default:
		return types.StopReasonEndTurn
	}
}

func firstCandidate(resp *genai.GenerateContentResponse) *genai.Candidate {
	if resp == nil || len(resp.Candidates) == 0 {
		return nil
	}
	return resp.Candidates[0]
}

func functionCallPartToToolUse(part *genai.Part) types.ToolUseBlock {
	call := part.FunctionCall
	id := strings.TrimSpace(call.ID)
	if id == "" {
		id = "gem_tool"
	}
	input := copyMap(call.Args)
	if input == nil {
		input = map[string]any{}
	}
	if len(part.ThoughtSignature) > 0 {
		input[thoughtSignatureKey] = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
	}
	return types.ToolUseBlock{Type: "tool_use", ID: id, Name: call.Name, Input: input}
}

func inlinePartToContentBlock(part *genai.Part) (types.ContentBlock, string) {
	mime := strings.ToLower(strings.TrimSpace(part.InlineData.MIMEType))
	data := base64.StdEncoding.EncodeToString(part.InlineData.Data)
	if strings.HasPrefix(mime, "image/") {
		return types.ImageBlock{Type: "image", Source: types.ImageSource{Type: "base64", MediaType: mime, Data: data}}, ""
	}
	if strings.HasPrefix(mime, "audio/") {
		return types.AudioBlock{Type: "audio", Source: types.AudioSource{Type: "base64", MediaType: mime, Data: data}}, ""
	}
	if strings.HasPrefix(mime, "video/") {
		return types.VideoBlock{Type: "video", Source: types.VideoSource{Type: "base64", MediaType: mime, Data: data}}, ""
	}
	return nil, fmt.Sprintf("unhandled inline output MIME type %q", mime)
}

func filePartToContentBlock(part *genai.Part) (types.ContentBlock, string) {
	mime := strings.ToLower(strings.TrimSpace(part.FileData.MIMEType))
	uri := strings.TrimSpace(part.FileData.FileURI)
	if strings.HasPrefix(mime, "image/") || mime == "" {
		return types.ImageBlock{Type: "image", Source: types.ImageSource{Type: "url", MediaType: mime, URL: uri}}, ""
	}
	return nil, fmt.Sprintf("unhandled file output MIME type %q", mime)
}

func toJSONMap(v any) (map[string]any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}

func convertToJSONAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

func appendGemArray(m map[string]any, key string, value any) {
	if m == nil {
		return
	}
	cur, _ := m[key].([]any)
	cur = append(cur, value)
	m[key] = cur
}
