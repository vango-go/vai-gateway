package openai

import (
	"encoding/json"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestBuildRequest_TranslatesMessagesToolsAndSchema(t *testing.T) {
	p := New("test-key")
	temperature := 0.4

	req := &types.MessageRequest{
		Model:       "gpt-4o",
		Temperature: &temperature,
		System: []types.ContentBlock{
			types.TextBlock{Type: "text", Text: "System line 1"},
			types.TextBlock{Type: "text", Text: "System line 2"},
		},
		Messages: []types.Message{
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "check weather"},
					types.ImageBlock{
						Type: "image",
						Source: types.ImageSource{
							Type:      "base64",
							MediaType: "image/png",
							Data:      "abc123",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "tool_1",
						Name:  "get_weather",
						Input: map[string]any{"location": "sf"},
					},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.ToolResultBlock{
						Type:      "tool_result",
						ToolUseID: "tool_1",
						Content:   []types.ContentBlock{types.TextBlock{Type: "text", Text: "72 and sunny"}},
					},
				},
			},
		},
		Tools: []types.Tool{
			{
				Type:        types.ToolTypeFunction,
				Name:        "get_weather",
				Description: "Weather lookup",
				InputSchema: &types.JSONSchema{Type: "object"},
			},
			{Type: types.ToolTypeWebSearch},
		},
		ToolChoice: &types.ToolChoice{Type: "tool", Name: "get_weather"},
		OutputFormat: &types.OutputFormat{
			Type: "json_schema",
			JSONSchema: &types.JSONSchema{
				Type: "object",
				Properties: map[string]types.JSONSchema{
					"answer": {Type: "string"},
				},
			},
		},
	}

	translated := p.buildRequest(req)

	if translated.MaxCompletionTokens == nil || *translated.MaxCompletionTokens != DefaultMaxTokens {
		t.Fatalf("max completion tokens = %v, want %d", translated.MaxCompletionTokens, DefaultMaxTokens)
	}
	if translated.MaxTokens != nil {
		t.Fatalf("max tokens should be nil when using max_completion_tokens, got %v", translated.MaxTokens)
	}
	if translated.ToolChoice == nil {
		t.Fatal("tool choice should be translated")
	}
	if translated.ResponseFormat == nil || translated.ResponseFormat.Type != "json_schema" {
		t.Fatalf("response format = %#v, want json_schema", translated.ResponseFormat)
	}
	if translated.ResponseFormat.JSONSchema == nil || !translated.ResponseFormat.JSONSchema.Strict {
		t.Fatal("json schema response format should be strict")
	}
	if len(translated.Tools) != 1 {
		t.Fatalf("translated tools len = %d, want 1 function tool", len(translated.Tools))
	}
	if translated.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", translated.Tools[0].Function.Name)
	}

	if len(translated.Messages) != 4 {
		t.Fatalf("translated messages len = %d, want 4", len(translated.Messages))
	}
	if translated.Messages[0].Role != "system" {
		t.Fatalf("system message role = %q, want system", translated.Messages[0].Role)
	}
	if translated.Messages[1].Role != "user" {
		t.Fatalf("user message role = %q, want user", translated.Messages[1].Role)
	}
	if translated.Messages[2].Role != "assistant" || len(translated.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %#v, want one call", translated.Messages[2].ToolCalls)
	}
	if translated.Messages[3].Role != "tool" || translated.Messages[3].ToolCallID != "tool_1" {
		t.Fatalf("tool result message = %#v, want tool role with tool_1", translated.Messages[3])
	}

	userParts, ok := translated.Messages[1].Content.([]contentPart)
	if !ok {
		t.Fatalf("user content type = %T, want []contentPart", translated.Messages[1].Content)
	}
	if len(userParts) != 2 {
		t.Fatalf("user content parts len = %d, want 2", len(userParts))
	}
	if userParts[1].ImageURL == nil || userParts[1].ImageURL.URL != "data:image/png;base64,abc123" {
		t.Fatalf("image url = %#v, want data URL", userParts[1].ImageURL)
	}
}

func TestBuildRequest_AddsSyntheticUserMessageForMultimodalToolResult(t *testing.T) {
	p := New("test-key")

	req := &types.MessageRequest{
		Model: "gpt-4o",
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
									Data:      "aGVsbG8=",
								},
							},
						},
					},
				},
			},
		},
	}

	translated := p.buildRequest(req)
	if len(translated.Messages) != 3 {
		t.Fatalf("translated messages len = %d, want 3", len(translated.Messages))
	}
	if translated.Messages[1].Role != "tool" || translated.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("tool result message = %#v", translated.Messages[1])
	}
	if translated.Messages[2].Role != "user" {
		t.Fatalf("synthetic multimodal message role = %q, want user", translated.Messages[2].Role)
	}
	parts, ok := translated.Messages[2].Content.([]contentPart)
	if !ok {
		t.Fatalf("synthetic content type = %T, want []contentPart", translated.Messages[2].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("synthetic content parts len = %d, want 2", len(parts))
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("synthetic image part = %#v", parts[1])
	}
}

func TestParseResponse_MapsTextToolUseStopReasonAndUsage(t *testing.T) {
	p := New("test-key")

	body := []byte(`{
		"id":"chatcmpl_1",
		"model":"gpt-4o-mini",
		"choices":[
			{
				"index":0,
				"finish_reason":"tool_calls",
				"message":{
					"role":"assistant",
					"content":"Calling weather tool",
					"tool_calls":[
						{
							"id":"call_1",
							"type":"function",
							"function":{"name":"get_weather","arguments":"{\"location\":\"sf\"}"}
						}
					]
				}
			}
		],
		"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}
	}`)

	resp, err := p.parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse() error = %v", err)
	}

	if resp.Model != "openai/gpt-4o-mini" {
		t.Fatalf("model = %q, want openai/gpt-4o-mini", resp.Model)
	}
	if resp.StopReason != types.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, types.StopReasonToolUse)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 || resp.Usage.TotalTokens != 20 {
		t.Fatalf("usage = %+v, want input=12 output=8 total=20", resp.Usage)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content len = %d, want text + tool_use", len(resp.Content))
	}

	tb, ok := resp.Content[0].(types.TextBlock)
	if !ok || tb.Text != "Calling weather tool" {
		t.Fatalf("first content = %#v, want text block", resp.Content[0])
	}
	tu, ok := resp.Content[1].(types.ToolUseBlock)
	if !ok {
		t.Fatalf("second content type = %T, want ToolUseBlock", resp.Content[1])
	}
	if tu.ID != "call_1" || tu.Name != "get_weather" {
		t.Fatalf("tool use = %#v, want call_1/get_weather", tu)
	}
	location, _ := tu.Input["location"].(string)
	if location != "sf" {
		t.Fatalf("tool input location = %q, want sf", location)
	}
}

func TestParseResponse_NoChoices(t *testing.T) {
	p := New("test-key")

	body, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl_empty",
		"model":   "gpt-4o-mini",
		"choices": []any{},
		"usage": map[string]int{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	})

	_, err := p.parseResponse(body)
	if err == nil {
		t.Fatal("expected error for response with no choices")
	}
}

func TestBuildRequest_UsesMaxTokensFieldWhenConfigured(t *testing.T) {
	p := New("test-key", WithMaxTokensField(MaxTokensFieldMaxTokens))

	translated := p.buildRequest(&types.MessageRequest{
		Model:     "gpt-4o-mini",
		MaxTokens: 1234,
		Messages: []types.Message{
			{Role: "user", Content: "hello"},
		},
	})

	if translated.MaxTokens == nil || *translated.MaxTokens != 1234 {
		t.Fatalf("max tokens = %v, want 1234", translated.MaxTokens)
	}
	if translated.MaxCompletionTokens != nil {
		t.Fatalf("max_completion_tokens should be nil, got %v", translated.MaxCompletionTokens)
	}
}

func TestParseResponse_UsesConfiguredModelPrefix(t *testing.T) {
	p := New("test-key", WithResponseModelPrefix("groq"))

	body := []byte(`{
		"id":"chatcmpl_1",
		"model":"llama-3.3-70b-versatile",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)

	resp, err := p.parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse() error = %v", err)
	}
	if resp.Model != "groq/llama-3.3-70b-versatile" {
		t.Fatalf("model = %q, want groq/llama-3.3-70b-versatile", resp.Model)
	}
}
