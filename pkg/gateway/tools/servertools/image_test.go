package servertools

import (
	"context"
	"strings"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestBuildImageRefRegistryAndInjectImageRefText(t *testing.T) {
	t.Parallel()

	image := types.ImageBlock{
		Type: "image",
		Source: types.ImageSource{
			Type:      "url",
			URL:       "https://example.com/cat.png",
			MediaType: "image/png",
		},
	}
	history := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: "look"},
				image,
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				types.ToolResultBlock{
					Type:      "tool_result",
					ToolUseID: "call_1",
					Content:   []types.ContentBlock{image},
				},
			},
		},
	}

	reg := BuildImageRefRegistry(history)
	if _, ok := reg.Lookup("img-01"); !ok {
		t.Fatal("expected img-01 in registry")
	}
	injected := InjectImageRefText(history, reg)
	firstBlocks := injected[0].ContentBlocks()
	if len(firstBlocks) != 3 {
		t.Fatalf("first blocks=%d, want 3", len(firstBlocks))
	}
	if tb, ok := firstBlocks[1].(types.TextBlock); !ok || tb.Text != "img-01" {
		t.Fatalf("first blocks[1]=%#v", firstBlocks[1])
	}
	toolResult, ok := injected[1].ContentBlocks()[0].(types.ToolResultBlock)
	if !ok {
		t.Fatalf("tool result block=%T", injected[1].ContentBlocks()[0])
	}
	if tb, ok := toolResult.Content[0].(types.TextBlock); !ok || tb.Text != "img-01" {
		t.Fatalf("tool result injected content=%#v", toolResult.Content)
	}
}

func TestBuildPlannerMessages_NonVisionUsesPlaceholderText(t *testing.T) {
	t.Parallel()

	image := types.ImageBlock{
		Type: "image",
		Source: types.ImageSource{
			Type:      "url",
			URL:       "https://example.com/cat.png",
			MediaType: "image/png",
		},
	}
	history := []types.Message{{
		Role: "user",
		Content: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "look"},
			image,
		},
	}}

	plannerMessages, _ := BuildPlannerMessages(history, false)
	blocks := plannerMessages[0].ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("planner blocks=%d, want 3", len(blocks))
	}
	if tb, ok := blocks[1].(types.TextBlock); !ok || tb.Text != "img-01" {
		t.Fatalf("blocks[1]=%#v, want img-01 text block", blocks[1])
	}
	if tb, ok := blocks[2].(types.TextBlock); !ok || tb.Text != "[image omitted for non-vision model]" {
		t.Fatalf("blocks[2]=%#v, want placeholder text", blocks[2])
	}
	for _, block := range blocks {
		if _, ok := block.(types.ImageBlock); ok {
			t.Fatalf("non-vision planner blocks should not include an image: %#v", blocks)
		}
	}
}

func TestImageExecutorExecute_ValidatesInput(t *testing.T) {
	t.Parallel()

	ex := NewImageExecutor(ImageConfig{Provider: ProviderGemDev, Model: DefaultImageModel}, "sk-test", nil)
	ex.generate = func(ctx context.Context, req imageGenerateRequest) ([]types.ContentBlock, *types.Error) {
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: req.Prompt}}, nil
	}

	if _, err := ex.Execute(context.Background(), map[string]any{"prompt": ""}); err == nil || err.Param != "prompt" {
		t.Fatalf("expected prompt validation error, got %#v", err)
	}
	if _, err := ex.Execute(context.Background(), map[string]any{"prompt": "ok", "size": "3K"}); err == nil || err.Param != "size" {
		t.Fatalf("expected size validation error, got %#v", err)
	}
	if _, err := ex.Execute(context.Background(), map[string]any{"prompt": "ok", "images": []any{map[string]any{"id": "img-01"}, map[string]any{"id": "img-01"}}}); err == nil || !strings.Contains(err.Message, "duplicate image id") {
		t.Fatalf("expected duplicate image id error, got %#v", err)
	}
}

func TestFinalizeImageToolResultContent_AssignsGeneratedIDs(t *testing.T) {
	t.Parallel()

	content := []types.ContentBlock{
		types.TextBlock{Type: "text", Text: `{"tool":"vai_image","provider":"gem-dev","model":"gemini-3.1-flash-image-preview","prompt":"hi","referenced_image_ids":["img-01"],"generated_image_ids":[]}`},
		types.ImageBlock{
			Type: "image",
			Source: types.ImageSource{
				Type:      "base64",
				MediaType: "image/png",
				Data:      "aGVsbG8=",
			},
		},
	}
	reg := BuildImageRefRegistry([]types.Message{{
		Role: "user",
		Content: []types.ContentBlock{
			types.ImageBlock{
				Type: "image",
				Source: types.ImageSource{
					Type:      "url",
					URL:       "https://example.com/original.png",
					MediaType: "image/png",
				},
			},
		},
	}})

	finalized := FinalizeImageToolResultContent(content, reg)
	tb, ok := finalized[0].(types.TextBlock)
	if !ok {
		t.Fatalf("metadata block=%T", finalized[0])
	}
	if !strings.Contains(tb.Text, `"generated_image_ids":["img-02"]`) {
		t.Fatalf("metadata=%s", tb.Text)
	}
}
