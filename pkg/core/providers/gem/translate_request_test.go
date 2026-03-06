package gem

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestBuildGenerateContentRequest_MultimodalToolsAndThoughtSignature(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")
	sig := base64.StdEncoding.EncodeToString([]byte("opaque-sig"))

	req := &types.MessageRequest{
		Model: "gemini-2.5-flash",
		System: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "system-1"},
			types.TextBlock{Type: "text", Text: "system-2"},
		},
		Messages: []types.Message{
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "hello"},
					types.ImageBlock{
						Type: "image",
						Source: types.ImageSource{
							Type:      "base64",
							MediaType: "image/png",
							Data:      base64.StdEncoding.EncodeToString([]byte{1, 2, 3}),
						},
					},
					types.ImageBlock{
						Type: "image",
						Source: types.ImageSource{
							Type:      "url",
							MediaType: "image/jpeg",
							URL:       "https://example.com/cat.jpg",
						},
					},
					types.AudioBlock{
						Type: "audio",
						Source: types.AudioSource{
							Type:      "base64",
							MediaType: "audio/wav",
							Data:      base64.StdEncoding.EncodeToString([]byte{4, 5, 6}),
						},
					},
					types.VideoBlock{
						Type: "video",
						Source: types.VideoSource{
							Type:      "base64",
							MediaType: "video/mp4",
							Data:      base64.StdEncoding.EncodeToString([]byte{7, 8, 9}),
						},
					},
					types.DocumentBlock{
						Type: "document",
						Source: types.DocumentSource{
							Type:      "base64",
							MediaType: "application/pdf",
							Data:      base64.StdEncoding.EncodeToString([]byte{10, 11, 12}),
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type: "tool_use",
						ID:   "call_1",
						Name: "do_something",
						Input: map[string]any{
							"arg":               "value",
							thoughtSignatureKey: sig,
						},
					},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.ToolResultBlock{
						Type:      "tool_result",
						ToolUseID: "call_1",
						Content: []types.ContentBlock{
							types.TextBlock{Type: "text", Text: "tool output"},
						},
					},
				},
			},
		},
		Tools: []types.Tool{
			types.NewFunctionTool("do_something", "desc", &types.JSONSchema{
				Type:       "object",
				Properties: map[string]types.JSONSchema{"arg": {Type: "string"}},
			}),
			types.NewWebSearchTool(nil),
			types.NewCodeExecutionTool(nil),
		},
		ToolChoice: &types.ToolChoice{Type: "tool", Name: "do_something"},
		Extensions: map[string]any{
			"gem": map[string]any{
				"candidate_count": 2,
			},
		},
	}

	build, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions())
	if err != nil {
		t.Fatalf("buildGenerateContentRequest error: %v", err)
	}

	if build.config.SystemInstruction == nil || len(build.config.SystemInstruction.Parts) != 1 {
		t.Fatalf("expected system instruction text part")
	}
	if got := build.config.SystemInstruction.Parts[0].Text; got != "system-1\nsystem-2" {
		t.Fatalf("system instruction text = %q", got)
	}
	if build.config.CandidateCount != 2 {
		t.Fatalf("candidate count = %d, want 2", build.config.CandidateCount)
	}

	if len(build.config.Tools) < 3 {
		t.Fatalf("expected function + web search + code execution tools, got %d", len(build.config.Tools))
	}
	if build.config.ToolConfig == nil || build.config.ToolConfig.FunctionCallingConfig == nil {
		t.Fatalf("expected function calling config")
	}
	if got := build.config.ToolConfig.FunctionCallingConfig.Mode; got != genai.FunctionCallingConfigModeValidated {
		t.Fatalf("tool mode = %q, want VALIDATED", got)
	}
	if got := build.config.ToolConfig.FunctionCallingConfig.AllowedFunctionNames; len(got) != 1 || got[0] != "do_something" {
		t.Fatalf("allowed function names = %#v", got)
	}

	if len(build.contents) != 3 {
		t.Fatalf("contents len = %d, want 3", len(build.contents))
	}
	if len(build.contents[0].Parts) != 6 {
		t.Fatalf("first message parts len = %d, want 6", len(build.contents[0].Parts))
	}

	toolCallPart := build.contents[1].Parts[0]
	if toolCallPart.FunctionCall == nil {
		t.Fatalf("assistant message should contain function call")
	}
	if _, ok := toolCallPart.FunctionCall.Args[thoughtSignatureKey]; ok {
		t.Fatalf("thought signature leaked into function args")
	}
	if got := string(toolCallPart.ThoughtSignature); got != "opaque-sig" {
		t.Fatalf("thought signature bytes = %q, want %q", got, "opaque-sig")
	}

	toolRespPart := build.contents[2].Parts[0]
	if toolRespPart.FunctionResponse == nil {
		t.Fatalf("tool result should map to function response")
	}
	if toolRespPart.FunctionResponse.ID != "call_1" {
		t.Fatalf("function response id = %q", toolRespPart.FunctionResponse.ID)
	}
}

func TestBuildGenerateContentRequest_ToolResultIncludesMultimodalParts(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")
	req := &types.MessageRequest{
		Model: "gemini-2.5-flash",
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "call_1",
						Name:  "edit_image",
						Input: map[string]any{"prompt": "make it dramatic"},
					},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.ToolResultBlock{
						Type:      "tool_result",
						ToolUseID: "call_1",
						Content: []types.ContentBlock{
							types.TextBlock{Type: "text", Text: `{"generated_image_ids":["img-02"]}`},
							types.ImageBlock{
								Type: "image",
								Source: types.ImageSource{
									Type:      "base64",
									MediaType: "image/png",
									Data:      base64.StdEncoding.EncodeToString([]byte("hello")),
								},
							},
						},
					},
				},
			},
		},
	}

	build, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions())
	if err != nil {
		t.Fatalf("buildGenerateContentRequest error: %v", err)
	}
	if len(build.contents) != 2 {
		t.Fatalf("contents len = %d, want 2", len(build.contents))
	}
	if len(build.contents[1].Parts) != 3 {
		t.Fatalf("tool result parts len = %d, want 3", len(build.contents[1].Parts))
	}
	if build.contents[1].Parts[0].FunctionResponse == nil {
		t.Fatalf("first part should be function response")
	}
	if build.contents[1].Parts[1].Text != `{"generated_image_ids":["img-02"]}` {
		t.Fatalf("second part text = %q", build.contents[1].Parts[1].Text)
	}
	if build.contents[1].Parts[2].InlineData == nil || build.contents[1].Parts[2].InlineData.MIMEType != "image/png" {
		t.Fatalf("third part = %#v, want inline image data", build.contents[1].Parts[2])
	}
}

func TestBuildGenerateContentRequest_Validation(t *testing.T) {
	t.Parallel()

	provider := NewDeveloper("test-key")

	t.Run("invalid_tool_name", func(t *testing.T) {
		req := &types.MessageRequest{
			Model: "gemini-2.5-flash",
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			},
			Tools: []types.Tool{
				types.NewFunctionTool("1invalid", "desc", nil),
			},
		}
		if _, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions()); err == nil {
			t.Fatalf("expected invalid tool name error")
		}
	})

	t.Run("video_output_rejected", func(t *testing.T) {
		req := &types.MessageRequest{
			Model: "gemini-2.5-flash",
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			},
			Output: &types.OutputConfig{Modalities: []string{"video"}},
		}
		if _, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions()); err == nil || !strings.Contains(err.Error(), "video output is not implemented") {
			t.Fatalf("expected video output rejection, got %v", err)
		}
	})

	t.Run("inline_limit", func(t *testing.T) {
		req := &types.MessageRequest{
			Model: "gemini-2.5-flash",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						types.ImageBlock{
							Type: "image",
							Source: types.ImageSource{
								Type:      "base64",
								MediaType: "image/png",
								Data:      base64.StdEncoding.EncodeToString([]byte("1234")),
							},
						},
					},
				},
			},
			Extensions: map[string]any{
				"gem": map[string]any{
					"inline_media_max_bytes": 2,
				},
			},
		}
		if _, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions()); err == nil || !strings.Contains(err.Error(), "inline media exceeds max decoded bytes") {
			t.Fatalf("expected inline size error, got %v", err)
		}
	})

	t.Run("known_extension_wrong_type", func(t *testing.T) {
		req := &types.MessageRequest{
			Model: "gemini-2.5-flash",
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			},
			Extensions: map[string]any{
				"gem": map[string]any{
					"thinking_budget": "bad",
				},
			},
		}
		if _, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions()); err == nil || !strings.Contains(err.Error(), "thinking_budget") {
			t.Fatalf("expected extension type error, got %v", err)
		}
	})

	t.Run("unknown_mime_warning", func(t *testing.T) {
		req := &types.MessageRequest{
			Model: "gemini-2.5-flash",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						types.ImageBlock{
							Type: "image",
							Source: types.ImageSource{
								Type:      "base64",
								MediaType: "image/unknown",
								Data:      base64.StdEncoding.EncodeToString([]byte("x")),
							},
						},
					},
				},
			},
		}
		build, err := provider.buildGenerateContentRequest(req, defaultBuildRequestOptions())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		raw, _ := json.Marshal(build.warnings)
		if !strings.Contains(string(raw), "unsupported image MIME type") {
			t.Fatalf("expected unknown mime warning, got %s", string(raw))
		}
	})
}

func TestBuildGenerateContentRequest_StreamArgsPolicy(t *testing.T) {
	t.Parallel()

	baseReq := func() *types.MessageRequest {
		return &types.MessageRequest{
			Model: "gemini-3-flash-preview",
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "hi"}}},
			},
			Tools: []types.Tool{
				types.NewFunctionTool("get_weather", "Get weather", &types.JSONSchema{
					Type:       "object",
					Properties: map[string]types.JSONSchema{"location": {Type: "string"}},
				}),
			},
		}
	}

	t.Run("vertex unary does not set streaming args", func(t *testing.T) {
		provider := NewVertex("test-key")
		build, err := provider.buildGenerateContentRequest(baseReq(), defaultBuildRequestOptions())
		if err != nil {
			t.Fatalf("build error: %v", err)
		}
		fc := build.config.ToolConfig.FunctionCallingConfig
		if fc.StreamFunctionCallArguments != nil {
			t.Fatalf("expected nil StreamFunctionCallArguments for unary")
		}
		if build.streamArgsEnabled {
			t.Fatalf("expected streamArgsEnabled=false for unary")
		}
		if build.streamArgsAuto || build.streamArgsExplicit {
			t.Fatalf("expected unary stream arg policy markers to be false")
		}
	})

	t.Run("vertex stream auto defaults true", func(t *testing.T) {
		provider := NewVertex("test-key")
		build, err := provider.buildGenerateContentRequest(baseReq(), buildRequestOptions{isStream: true})
		if err != nil {
			t.Fatalf("build error: %v", err)
		}
		fc := build.config.ToolConfig.FunctionCallingConfig
		if fc.StreamFunctionCallArguments == nil || !*fc.StreamFunctionCallArguments {
			t.Fatalf("expected StreamFunctionCallArguments=true")
		}
		if !build.streamArgsEnabled || !build.streamArgsAuto || build.streamArgsExplicit {
			t.Fatalf("unexpected stream args policy flags: enabled=%t auto=%t explicit=%t", build.streamArgsEnabled, build.streamArgsAuto, build.streamArgsExplicit)
		}
	})

	t.Run("vertex stream explicit false", func(t *testing.T) {
		provider := NewVertex("test-key")
		req := baseReq()
		req.Extensions = map[string]any{"gem": map[string]any{"stream_function_call_arguments": false}}
		build, err := provider.buildGenerateContentRequest(req, buildRequestOptions{isStream: true})
		if err != nil {
			t.Fatalf("build error: %v", err)
		}
		fc := build.config.ToolConfig.FunctionCallingConfig
		if fc.StreamFunctionCallArguments == nil || *fc.StreamFunctionCallArguments {
			t.Fatalf("expected StreamFunctionCallArguments=false")
		}
		if build.streamArgsAuto || !build.streamArgsExplicit {
			t.Fatalf("expected explicit policy flags for extension override")
		}
	})

	t.Run("vertex stream auto override false", func(t *testing.T) {
		provider := NewVertex("test-key")
		disabled := false
		build, err := provider.buildGenerateContentRequest(baseReq(), buildRequestOptions{
			isStream:               true,
			autoStreamArgsOverride: &disabled,
		})
		if err != nil {
			t.Fatalf("build error: %v", err)
		}
		fc := build.config.ToolConfig.FunctionCallingConfig
		if fc.StreamFunctionCallArguments == nil || *fc.StreamFunctionCallArguments {
			t.Fatalf("expected StreamFunctionCallArguments=false from auto override")
		}
		if build.streamArgsExplicit || !build.streamArgsAuto {
			t.Fatalf("expected auto (non-explicit) policy flags")
		}
	})
}
