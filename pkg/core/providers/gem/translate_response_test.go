package gem

import (
	"encoding/base64"
	"testing"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestTranslateGenerateContentResponse_ContentUsageAndMetadata(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")

	text := genai.NewPartFromText("hello")
	thinking := genai.NewPartFromText("internal thought")
	thinking.Thought = true

	tool := genai.NewPartFromFunctionCall("do_something", map[string]any{"arg": "value"})
	tool.FunctionCall.ID = "tool_1"
	tool.ThoughtSignature = []byte("opaque-sig")

	image := genai.NewPartFromBytes([]byte{1, 2, 3}, "image/png")
	audio := genai.NewPartFromBytes([]byte{4, 5, 6}, "audio/wav")

	resp := &genai.GenerateContentResponse{
		ResponseID: "resp_1",
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{text, thinking, tool, image, audio},
				},
				FinishReason:      genai.FinishReasonStop,
				GroundingMetadata: &genai.GroundingMetadata{},
			},
			{
				Content: &genai.Content{
					Parts: []*genai.Part{genai.NewPartFromText("alt")},
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        10,
			CandidatesTokenCount:    7,
			TotalTokenCount:         0, // force fallback
			ThoughtsTokenCount:      3,
			ToolUsePromptTokenCount: 2,
		},
	}

	out, err := provider.translateGenerateContentResponse("gemini-2.5-flash", resp, &requestBuild{
		ext:      gemExtensions{CandidateCount: 2},
		warnings: []string{"warn-1"},
	})
	if err != nil {
		t.Fatalf("translateGenerateContentResponse error: %v", err)
	}

	if out.Model != "gem-dev/gemini-2.5-flash" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.StopReason != types.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want tool_use", out.StopReason)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 7 || out.Usage.TotalTokens != 17 {
		t.Fatalf("unexpected usage: %+v", out.Usage)
	}
	if len(out.Content) != 5 {
		t.Fatalf("content len = %d, want 5", len(out.Content))
	}

	toolBlock, ok := out.Content[2].(types.ToolUseBlock)
	if !ok {
		t.Fatalf("expected tool_use block at index 2, got %T", out.Content[2])
	}
	if toolBlock.ID != "tool_1" || toolBlock.Name != "do_something" {
		t.Fatalf("unexpected tool block: %+v", toolBlock)
	}
	if toolBlock.Input[thoughtSignatureKey] == "" {
		t.Fatalf("expected encoded thought signature in tool input")
	}

	imgBlock, ok := out.Content[3].(types.ImageBlock)
	if !ok {
		t.Fatalf("expected image block at index 3, got %T", out.Content[3])
	}
	if imgBlock.Source.Type != "base64" || imgBlock.Source.MediaType != "image/png" {
		t.Fatalf("unexpected image block: %+v", imgBlock)
	}
	if _, err := base64.StdEncoding.DecodeString(imgBlock.Source.Data); err != nil {
		t.Fatalf("image output base64 decode failed: %v", err)
	}

	audioBlock, ok := out.Content[4].(types.AudioBlock)
	if !ok {
		t.Fatalf("expected audio block at index 4, got %T", out.Content[4])
	}
	if audioBlock.Source.Type != "base64" || audioBlock.Source.MediaType != "audio/wav" {
		t.Fatalf("unexpected audio block: %+v", audioBlock)
	}

	gemMeta, ok := out.Metadata["gem"].(map[string]any)
	if !ok {
		t.Fatalf("expected gem metadata object, got %#v", out.Metadata)
	}
	if gemMeta["finish_reason"] != string(genai.FinishReasonStop) {
		t.Fatalf("finish reason metadata = %#v", gemMeta["finish_reason"])
	}
	if _, ok := gemMeta["candidates"]; !ok {
		t.Fatalf("expected extra candidates metadata")
	}
	if _, ok := gemMeta["grounding"]; !ok {
		t.Fatalf("expected grounding metadata")
	}
}

func TestMapStopReason(t *testing.T) {
	t.Parallel()

	if got := mapStopReason(&genai.Candidate{FinishReason: genai.FinishReasonMaxTokens}, nil); got != types.StopReasonMaxTokens {
		t.Fatalf("max tokens mapping = %q", got)
	}
	if got := mapStopReason(nil, nil); got != types.StopReasonEndTurn {
		t.Fatalf("nil candidate mapping = %q", got)
	}
	if got := mapStopReason(&genai.Candidate{FinishReason: genai.FinishReasonStop}, []types.ContentBlock{
		types.ToolUseBlock{Type: "tool_use", ID: "x", Name: "f", Input: map[string]any{}},
	}); got != types.StopReasonToolUse {
		t.Fatalf("tool_use mapping = %q", got)
	}
}
