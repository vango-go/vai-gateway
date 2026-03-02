package gemini_oauth

import (
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestBuildRequest_PreservesThoughtSignaturesAcrossRepeatedCalls(t *testing.T) {
	provider := &Provider{}

	req := &types.MessageRequest{
		Model: "gemini-oauth/gemini-3-pro-preview",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: "Call get_weather repeatedly"},
			}},
			{Role: "assistant", Content: []types.ContentBlock{
				types.ToolUseBlock{
					Type: "tool_use",
					ID:   "call_get_weather",
					Name: "get_weather",
					Input: map[string]any{
						"location":            "san francisco",
						"__thought_signature": "sig-1",
					},
				},
			}},
			{Role: "user", Content: []types.ContentBlock{
				types.ToolResultBlock{
					Type:      "tool_result",
					ToolUseID: "call_get_weather",
					Content: []types.ContentBlock{
						types.TextBlock{Type: "text", Text: "sunny"},
					},
				},
			}},
		},
	}

	validate := func(gReq *geminiRequest) {
		t.Helper()
		var toolParts []geminiPart
		for _, content := range gReq.Contents {
			for _, part := range content.Parts {
				if part.FunctionCall != nil {
					toolParts = append(toolParts, part)
				}
			}
		}
		if len(toolParts) != 1 {
			t.Fatalf("function call parts = %d, want 1", len(toolParts))
		}
		if toolParts[0].ThoughtSignature != "sig-1" {
			t.Fatalf("thought signature = %q, want %q", toolParts[0].ThoughtSignature, "sig-1")
		}
		if toolParts[0].FunctionCall.Args["location"] != "san francisco" {
			t.Fatalf("function args location = %v, want %q", toolParts[0].FunctionCall.Args["location"], "san francisco")
		}
		if _, ok := toolParts[0].FunctionCall.Args["__thought_signature"]; ok {
			t.Fatalf("__thought_signature leaked into function args: %#v", toolParts[0].FunctionCall.Args)
		}
	}

	first := provider.buildRequest(req)
	validate(first)

	second := provider.buildRequest(req)
	validate(second)

	assistantBlocks := req.Messages[1].ContentBlocks()
	if len(assistantBlocks) != 1 {
		t.Fatalf("assistant blocks = %d, want 1", len(assistantBlocks))
	}
	toolUse, ok := assistantBlocks[0].(types.ToolUseBlock)
	if !ok {
		t.Fatalf("assistant block type = %T, want ToolUseBlock", assistantBlocks[0])
	}
	if got, ok := toolUse.Input["__thought_signature"].(string); !ok || got != "sig-1" {
		t.Fatalf("history thought signature = %v, want %q", toolUse.Input["__thought_signature"], "sig-1")
	}
}

func TestBuildRequest_StreamFunctionCallArgumentsExtension(t *testing.T) {
	provider := &Provider{}

	baseReq := &types.MessageRequest{
		Model:    "gemini-oauth/gemini-2.5-pro",
		Stream:   true,
		Messages: []types.Message{{Role: "user", Content: "hi"}},
		Tools: []types.Tool{
			{
				Type:        types.ToolTypeFunction,
				Name:        "talk_to_user",
				Description: "Speak to user",
				InputSchema: &types.JSONSchema{Type: "object"},
			},
		},
	}

	t.Run("snake_case_extension_enables_streaming_flag", func(t *testing.T) {
		req := *baseReq
		req.Extensions = map[string]any{
			"gemini": map[string]any{
				"stream_function_call_arguments": true,
			},
		}

		got := provider.buildRequest(&req)
		if got.ToolConfig == nil || got.ToolConfig.FunctionCallingConfig == nil {
			t.Fatalf("tool config missing: %#v", got.ToolConfig)
		}
		if got.ToolConfig.FunctionCallingConfig.Mode != "AUTO" {
			t.Fatalf("mode = %q, want AUTO", got.ToolConfig.FunctionCallingConfig.Mode)
		}
		if !got.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments {
			t.Fatal("stream_function_call_arguments should be enabled")
		}
	})

	t.Run("camel_case_alias_enables_streaming_flag", func(t *testing.T) {
		req := *baseReq
		req.Extensions = map[string]any{
			"gemini": map[string]any{
				"streamFunctionCallArguments": true,
			},
		}

		got := provider.buildRequest(&req)
		if got.ToolConfig == nil || got.ToolConfig.FunctionCallingConfig == nil {
			t.Fatalf("tool config missing: %#v", got.ToolConfig)
		}
		if !got.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments {
			t.Fatal("stream_function_call_arguments should be enabled")
		}
	})

	t.Run("non_stream_request_does_not_enable_flag", func(t *testing.T) {
		req := *baseReq
		req.Stream = false
		req.Extensions = map[string]any{
			"gemini": map[string]any{
				"stream_function_call_arguments": true,
			},
		}

		got := provider.buildRequest(&req)
		if got.ToolConfig != nil && got.ToolConfig.FunctionCallingConfig != nil &&
			got.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments {
			t.Fatalf("stream_function_call_arguments unexpectedly enabled: %#v", got.ToolConfig.FunctionCallingConfig)
		}
	})
}

func TestBuildRequest_SkipsEmptyTextPartsAndMessages(t *testing.T) {
	provider := &Provider{}

	req := &types.MessageRequest{
		Model:  "gemini-oauth/gemini-3-flash-preview",
		System: "",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: "hello"},
			}},
			{Role: "assistant", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: ""},
			}},
			{Role: "user", Content: []types.ContentBlock{
				types.TextBlock{Type: "text", Text: ""},
				types.TextBlock{Type: "text", Text: "world"},
			}},
		},
	}

	got := provider.buildRequest(req)
	if got.SystemInstruction != nil {
		t.Fatalf("system instruction should be omitted when empty: %#v", got.SystemInstruction)
	}
	if len(got.Contents) != 2 {
		t.Fatalf("contents=%d, want 2", len(got.Contents))
	}
	if got.Contents[0].Role != "user" || len(got.Contents[0].Parts) != 1 || got.Contents[0].Parts[0].Text != "hello" {
		t.Fatalf("unexpected first content: %#v", got.Contents[0])
	}
	if got.Contents[1].Role != "user" || len(got.Contents[1].Parts) != 1 || got.Contents[1].Parts[0].Text != "world" {
		t.Fatalf("unexpected second content: %#v", got.Contents[1])
	}

	for i, c := range got.Contents {
		for j, p := range c.Parts {
			if p.Text == "" && p.InlineData == nil && p.FileData == nil && p.FunctionCall == nil && p.FunctionResponse == nil {
				t.Fatalf("empty part at content[%d].parts[%d]: %#v", i, j, p)
			}
		}
	}
}
