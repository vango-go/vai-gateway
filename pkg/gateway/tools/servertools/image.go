package servertools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core"
	coregem "github.com/vango-go/vai-lite/pkg/core/providers/gem"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

var supportedImageAspectRatios = map[string]struct{}{
	"1:1":  {},
	"1:4":  {},
	"1:8":  {},
	"2:3":  {},
	"3:2":  {},
	"3:4":  {},
	"4:1":  {},
	"4:3":  {},
	"4:5":  {},
	"5:4":  {},
	"8:1":  {},
	"9:16": {},
	"16:9": {},
	"21:9": {},
}

type ImageExecutor struct {
	config   ImageConfig
	apiKey   string
	http     *http.Client
	generate func(ctx context.Context, req imageGenerateRequest) ([]types.ContentBlock, *types.Error)
}

type imageGenerateRequest struct {
	Provider    string
	Model       string
	Prompt      string
	Images      []imageInputRef
	AspectRatio string
	Size        string
}

type imageInputRef struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

type imageResultMetadata struct {
	Tool               string   `json:"tool"`
	Provider           string   `json:"provider"`
	Model              string   `json:"model"`
	Prompt             string   `json:"prompt"`
	ReferencedImageIDs []string `json:"referenced_image_ids,omitempty"`
	GeneratedImageIDs  []string `json:"generated_image_ids,omitempty"`
}

func NewImageExecutor(config ImageConfig, apiKey string, httpClient *http.Client) *ImageExecutor {
	ex := &ImageExecutor{config: config, apiKey: strings.TrimSpace(apiKey), http: httpClient}
	ex.generate = ex.generateWithGemini
	return ex
}

func (e *ImageExecutor) Name() string { return ToolImage }

func (e *ImageExecutor) Definition() types.Tool {
	additionalProps := false
	return types.Tool{
		Type:        types.ToolTypeFunction,
		Name:        ToolImage,
		Description: "Generate or edit images with Gemini image generation. Use a prompt alone for new images, or pass prior image IDs for edits and variations.",
		InputSchema: &types.JSONSchema{
			Type: "object",
			Properties: map[string]types.JSONSchema{
				"prompt": {Type: "string", Description: "Image generation or editing prompt."},
				"images": {
					Type: "array",
					Items: &types.JSONSchema{
						Type: "object",
						Properties: map[string]types.JSONSchema{
							"id":          {Type: "string", Description: "Synthetic image ID from the conversation history, such as img-01."},
							"description": {Type: "string", Description: "Optional guidance for how to use this image."},
						},
						Required:             []string{"id"},
						AdditionalProperties: &additionalProps,
					},
				},
				"aspect_ratio": {Type: "string", Description: "Optional output aspect ratio, such as 1:1, 4:3, 16:9, or 21:9."},
				"size":         {Type: "string", Description: "Optional output size: 1K, 2K, or 4K."},
			},
			Required:             []string{"prompt"},
			AdditionalProperties: &additionalProps,
		},
	}
}

func (e *ImageExecutor) Execute(ctx context.Context, input map[string]any) ([]types.ContentBlock, *types.Error) {
	req, err := decodeImageToolInput(input)
	if err != nil {
		return nil, err
	}
	if e.generate == nil {
		return nil, &types.Error{Type: string(core.ErrAPI), Message: "image tool is not configured", Code: "tool_provider_error"}
	}
	return e.generate(ctx, imageGenerateRequest{
		Provider:    e.config.Provider,
		Model:       e.config.Model,
		Prompt:      req.Prompt,
		Images:      req.Images,
		AspectRatio: req.AspectRatio,
		Size:        req.Size,
	})
}

func (e *ImageExecutor) generateWithGemini(ctx context.Context, req imageGenerateRequest) ([]types.ContentBlock, *types.Error) {
	var provider *coregem.Provider
	switch req.Provider {
	case ProviderGemDev:
		provider = coregem.NewDeveloper(e.apiKey, coregem.WithHTTPClient(e.http))
	case ProviderGemVert:
		provider = coregem.NewVertex(e.apiKey, coregem.WithHTTPClient(e.http))
	default:
		return nil, &types.Error{Type: string(core.ErrInvalidRequest), Message: "unsupported image provider", Param: "provider", Code: "unsupported_tool_provider"}
	}

	blocks := make([]types.ContentBlock, 0, 1+len(req.Images))
	blocks = append(blocks, types.TextBlock{Type: "text", Text: req.Prompt})
	reg := ImageRefRegistryFromContext(ctx)
	if len(req.Images) > 0 && reg == nil {
		return nil, &types.Error{
			Type:    string(core.ErrInvalidRequest),
			Message: "image id references require /v1/runs, /v1/runs:stream, or /v1/live; /v1/server-tools:execute is stateless",
			Param:   "images[0].id",
			Code:    "run_validation_failed",
		}
	}
	for i := range req.Images {
		img, ok := reg.Lookup(req.Images[i].ID)
		if !ok {
			return nil, &types.Error{
				Type:    string(core.ErrInvalidRequest),
				Message: fmt.Sprintf("unknown image id %q", req.Images[i].ID),
				Param:   fmt.Sprintf("images[%d].id", i),
				Code:    "run_validation_failed",
			}
		}
		if note := strings.TrimSpace(req.Images[i].Description); note != "" {
			blocks = append(blocks, types.TextBlock{Type: "text", Text: note})
		}
		blocks = append(blocks, img)
	}

	size := req.Size
	if size == "" {
		size = "1K"
	}
	msgReq := &types.MessageRequest{
		Model: req.Model,
		Messages: []types.Message{{
			Role:    "user",
			Content: blocks,
		}},
		Output: &types.OutputConfig{
			Modalities: []string{"text", "image"},
			Image:      &types.ImageConfig{Size: size},
		},
	}
	if req.AspectRatio != "" {
		msgReq.Extensions = map[string]any{
			"gem": map[string]any{
				"image_config": map[string]any{
					"aspect_ratio": req.AspectRatio,
				},
			},
		}
	}

	resp, err := provider.CreateMessage(ctx, msgReq)
	if err != nil {
		if typed, ok := err.(*core.Error); ok {
			return nil, &types.Error{
				Type:          string(typed.Type),
				Message:       typed.Message,
				Param:         typed.Param,
				Code:          typed.Code,
				ProviderError: typed.ProviderError,
				RetryAfter:    typed.RetryAfter,
			}
		}
		return nil, &types.Error{Type: string(core.ErrAPI), Message: err.Error(), Code: "tool_provider_error"}
	}

	meta := imageResultMetadata{
		Tool:               ToolImage,
		Provider:           req.Provider,
		Model:              req.Model,
		Prompt:             req.Prompt,
		ReferencedImageIDs: imageRefIDs(req.Images),
		GeneratedImageIDs:  []string{},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, &types.Error{Type: string(core.ErrAPI), Message: "failed to encode image tool metadata", Code: "tool_provider_error"}
	}

	content := make([]types.ContentBlock, 0, len(resp.Content)+1)
	content = append(content, types.TextBlock{Type: "text", Text: string(metaJSON)})
	content = append(content, resp.Content...)
	return content, nil
}

func FinalizeImageToolResultContent(content []types.ContentBlock, reg *ImageRefRegistry) []types.ContentBlock {
	if len(content) == 0 {
		return content
	}
	generated := make([]string, 0, 1)
	for i := range content {
		switch b := content[i].(type) {
		case types.ImageBlock:
			generated = append(generated, reg.Register(b))
		case *types.ImageBlock:
			if b != nil {
				generated = append(generated, reg.Register(*b))
			}
		}
	}
	tb, ok := content[0].(types.TextBlock)
	if !ok {
		return content
	}
	var meta imageResultMetadata
	if err := json.Unmarshal([]byte(tb.Text), &meta); err != nil || meta.Tool != ToolImage {
		return content
	}
	meta.GeneratedImageIDs = generated
	encoded, err := json.Marshal(meta)
	if err != nil {
		return content
	}
	out := append([]types.ContentBlock(nil), content...)
	out[0] = types.TextBlock{Type: "text", Text: string(encoded)}
	return out
}

func PromoteImageToolResultContent(content []types.ContentBlock) []types.ContentBlock {
	if len(content) == 0 {
		return nil
	}
	out := make([]types.ContentBlock, 0, len(content))
	for i := range content {
		switch b := content[i].(type) {
		case types.ImageBlock:
			out = append(out, b)
		case *types.ImageBlock:
			if b != nil {
				out = append(out, *b)
			}
		}
	}
	return out
}

func decodeImageToolInput(input map[string]any) (imageGenerateRequest, *types.Error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "input must be an object", Param: "input", Code: "run_validation_failed"}
	}
	var decoded struct {
		Prompt      string          `json:"prompt"`
		Images      []imageInputRef `json:"images"`
		AspectRatio string          `json:"aspect_ratio"`
		Size        string          `json:"size"`
	}
	dec := json.NewDecoder(strings.NewReader(string(encoded)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: fmt.Sprintf("invalid image tool input: %v", err), Param: "input", Code: "run_validation_failed"}
	}
	decoded.Prompt = strings.TrimSpace(decoded.Prompt)
	if decoded.Prompt == "" {
		return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "prompt is required", Param: "prompt", Code: "run_validation_failed"}
	}
	seen := make(map[string]struct{}, len(decoded.Images))
	for i := range decoded.Images {
		decoded.Images[i].ID = strings.TrimSpace(decoded.Images[i].ID)
		if decoded.Images[i].ID == "" {
			return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "image id is required", Param: fmt.Sprintf("images[%d].id", i), Code: "run_validation_failed"}
		}
		if _, ok := seen[decoded.Images[i].ID]; ok {
			return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "duplicate image id", Param: fmt.Sprintf("images[%d].id", i), Code: "run_validation_failed"}
		}
		seen[decoded.Images[i].ID] = struct{}{}
		decoded.Images[i].Description = strings.TrimSpace(decoded.Images[i].Description)
	}
	decoded.AspectRatio = strings.TrimSpace(decoded.AspectRatio)
	if decoded.AspectRatio != "" {
		if _, ok := supportedImageAspectRatios[decoded.AspectRatio]; !ok {
			return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "unsupported aspect_ratio", Param: "aspect_ratio", Code: "run_validation_failed"}
		}
	}
	decoded.Size = strings.ToUpper(strings.TrimSpace(decoded.Size))
	switch decoded.Size {
	case "", "1K", "2K", "4K":
	default:
		return imageGenerateRequest{}, &types.Error{Type: string(core.ErrInvalidRequest), Message: "unsupported size", Param: "size", Code: "run_validation_failed"}
	}
	return imageGenerateRequest{
		Prompt:      decoded.Prompt,
		Images:      decoded.Images,
		AspectRatio: decoded.AspectRatio,
		Size:        decoded.Size,
	}, nil
}

func imageRefIDs(images []imageInputRef) []string {
	if len(images) == 0 {
		return nil
	}
	out := make([]string, 0, len(images))
	for i := range images {
		out = append(out, images[i].ID)
	}
	return out
}
