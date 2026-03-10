package oai_resp

import (
	"encoding/json"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

func TestBuildRequest_ReasoningModelExtensionsAndToolTranslation(t *testing.T) {
	p := &Provider{}
	temperature := 0.8
	topP := 0.9

	req := &types.MessageRequest{
		Model:       "gpt-5-mini",
		MaxTokens:   8, // below API minimum
		Temperature: &temperature,
		TopP:        &topP,
		System:      "Follow policy",
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "call_1",
						Name:  "lookup",
						Input: map[string]any{"q": "x"},
					},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.ToolResultBlock{
						Type:      "tool_result",
						ToolUseID: "call_1",
						Content:   []types.ContentBlock{types.TextBlock{Type: "text", Text: "result"}},
					},
				},
			},
		},
		Tools: []types.Tool{
			{
				Type:        types.ToolTypeFunction,
				Name:        "lookup",
				Description: "Lookup data",
			},
			{
				Type:   types.ToolTypeComputerUse,
				Config: &types.ComputerUseConfig{DisplayWidth: 1440, DisplayHeight: 900},
			},
		},
		ToolChoice: &types.ToolChoice{Type: "tool", Name: "lookup"},
		OutputFormat: &types.OutputFormat{
			Type:       "json_schema",
			JSONSchema: &types.JSONSchema{Type: "object"},
		},
		Extensions: map[string]any{
			"oai_resp": map[string]any{
				"previous_response_id": "resp_prev",
				"reasoning": map[string]any{
					"effort":  "high",
					"summary": "auto",
				},
				"store": true,
			},
		},
		Metadata: map[string]any{"trace_id": "abc"},
	}

	translated := p.buildRequest(req)

	if translated.Temperature != nil || translated.TopP != nil {
		t.Fatal("temperature/top_p should be omitted for reasoning models")
	}
	if translated.MaxOutputTokens == nil || *translated.MaxOutputTokens != MinMaxTokens {
		t.Fatalf("max_output_tokens = %v, want %d", translated.MaxOutputTokens, MinMaxTokens)
	}
	if translated.Instructions != "Follow policy" {
		t.Fatalf("instructions = %q, want Follow policy", translated.Instructions)
	}
	if translated.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", translated.PreviousResponseID)
	}
	if translated.Reasoning == nil || translated.Reasoning.Effort != "high" || translated.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning config = %#v, want high/auto", translated.Reasoning)
	}
	if translated.Store == nil || !*translated.Store {
		t.Fatalf("store = %v, want true", translated.Store)
	}
	if translated.ToolChoice == nil {
		t.Fatal("tool choice should be translated")
	}
	if translated.Text == nil || translated.Text.Format == nil || translated.Text.Format.Type != "json_schema" {
		t.Fatalf("text format = %#v, want json_schema", translated.Text)
	}
	if len(translated.Tools) != 2 {
		t.Fatalf("tools len = %d, want 2", len(translated.Tools))
	}
	if translated.Tools[0].Type != "function" {
		t.Fatalf("first tool type = %q, want function", translated.Tools[0].Type)
	}
	if translated.Tools[1].Type != "computer_use_preview" {
		t.Fatalf("second tool type = %q, want computer_use_preview", translated.Tools[1].Type)
	}

	inputItems, ok := translated.Input.([]inputItem)
	if !ok {
		t.Fatalf("input type = %T, want []inputItem", translated.Input)
	}
	if len(inputItems) != 2 {
		t.Fatalf("input len = %d, want 2 (function_call + function_call_output)", len(inputItems))
	}
	if inputItems[0].Type != "function_call" || inputItems[1].Type != "function_call_output" {
		t.Fatalf("input items = %#v, want function call + output", inputItems)
	}
}

func TestBuildRequest_AddsSyntheticMessageForMultimodalToolResult(t *testing.T) {
	p := &Provider{}
	req := &types.MessageRequest{
		Model: "gpt-5-mini",
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
	inputItems, ok := translated.Input.([]inputItem)
	if !ok {
		t.Fatalf("input type = %T, want []inputItem", translated.Input)
	}
	if len(inputItems) != 3 {
		t.Fatalf("input len = %d, want 3", len(inputItems))
	}
	if inputItems[1].Type != "function_call_output" {
		t.Fatalf("inputItems[1].Type = %q, want function_call_output", inputItems[1].Type)
	}
	if inputItems[2].Type != "message" || inputItems[2].Role != "user" {
		t.Fatalf("inputItems[2] = %#v, want synthetic user message", inputItems[2])
	}
	contentParts, ok := inputItems[2].Content.([]contentPart)
	if !ok {
		t.Fatalf("synthetic content type = %T, want []contentPart", inputItems[2].Content)
	}
	if len(contentParts) != 2 {
		t.Fatalf("synthetic content len = %d, want 2", len(contentParts))
	}
	if contentParts[1].Type != "input_image" {
		t.Fatalf("synthetic image part = %#v", contentParts[1])
	}
}

func TestBuildRequest_EmptyToolResultStillSerializesOutputField(t *testing.T) {
	p := &Provider{}
	req := &types.MessageRequest{
		Model: "gpt-5-mini",
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "call_1",
						Name:  "lookup",
						Input: map[string]any{"q": "x"},
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
							types.TextBlock{Type: "text", Text: ""},
						},
					},
				},
			},
		},
	}

	translated := p.buildRequest(req)
	payload, err := json.Marshal(translated)
	if err != nil {
		t.Fatalf("marshal translated request: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode translated payload: %v", err)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("input len = %d, want 2", len(decoded.Input))
	}
	if _, ok := decoded.Input[1]["output"]; !ok {
		t.Fatalf("function_call_output payload = %#v, want explicit output field", decoded.Input[1])
	}
	if output, _ := decoded.Input[1]["output"].(string); output != "" {
		t.Fatalf("output = %q, want empty string", output)
	}
}

func TestBuildRequest_DoesNotSerializeOutputOnRegularMessages(t *testing.T) {
	p := &Provider{}
	req := &types.MessageRequest{
		Model: "gpt-5-mini",
		Messages: []types.Message{
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "hello"},
				},
			},
		},
	}

	translated := p.buildRequest(req)
	payload, err := json.Marshal(translated)
	if err != nil {
		t.Fatalf("marshal translated request: %v", err)
	}

	var decoded struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode translated payload: %v", err)
	}
	if len(decoded.Input) != 1 {
		t.Fatalf("input len = %d, want 1", len(decoded.Input))
	}
	if _, ok := decoded.Input[0]["output"]; ok {
		t.Fatalf("message payload = %#v, did not expect output field", decoded.Input[0])
	}
}

func TestParseResponse_MapsOutputItemsAndUsageMetadata(t *testing.T) {
	p := &Provider{}
	body := []byte(`{
		"id":"resp_1",
		"model":"gpt-5-mini",
		"status":"completed",
		"output_text":"fallback text",
		"output":[
			{
				"type":"reasoning",
				"summary":[{"type":"summary_text","text":"thought summary"}]
			},
			{
				"type":"function_call",
				"id":"item_1",
				"call_id":"call_1",
				"name":"lookup",
				"arguments":"{\"q\":\"abc\"}"
			},
			{
				"type":"message",
				"content":[{"type":"output_text","text":"final answer"}]
			}
		],
		"usage":{
			"input_tokens":10,
			"output_tokens":7,
			"total_tokens":17,
			"input_tokens_details":{"cached_tokens":4}
		}
	}`)

	resp, err := p.parseResponse(body, nil)
	if err != nil {
		t.Fatalf("parseResponse() error = %v", err)
	}

	if resp.Model != "oai-resp/gpt-5-mini" {
		t.Fatalf("model = %q, want oai-resp/gpt-5-mini", resp.Model)
	}
	if resp.StopReason != types.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, types.StopReasonToolUse)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 7 || resp.Usage.TotalTokens != 17 {
		t.Fatalf("usage = %+v, want input=10 output=7 total=17", resp.Usage)
	}
	if resp.Usage.CacheReadTokens == nil || *resp.Usage.CacheReadTokens != 4 {
		t.Fatalf("cache_read_tokens = %v, want 4", resp.Usage.CacheReadTokens)
	}

	var gotToolUse, gotText, gotThinking bool
	for _, block := range resp.Content {
		switch b := block.(type) {
		case types.ToolUseBlock:
			gotToolUse = gotToolUse || (b.ID == "call_1" && b.Name == "lookup")
		case types.TextBlock:
			if b.Text == "final answer" || b.Text == "fallback text" {
				gotText = true
			}
		case types.ThinkingBlock:
			if b.Thinking == "thought summary" {
				gotThinking = true
			}
		}
	}

	if !gotToolUse {
		t.Fatal("expected translated tool_use block")
	}
	if !gotText {
		t.Fatal("expected text output block")
	}
	if !gotThinking {
		t.Fatal("expected thinking block")
	}

	oaiExt, ok := resp.Metadata["oai_resp"].(map[string]any)
	if !ok {
		t.Fatalf("metadata oai_resp = %#v, want map", resp.Metadata["oai_resp"])
	}
	if responseID, _ := oaiExt["response_id"].(string); responseID != "resp_1" {
		t.Fatalf("metadata response_id = %q, want resp_1", responseID)
	}
}

func TestParseResponse_AcceptsTextContentTypeText(t *testing.T) {
	p := &Provider{}
	body := []byte(`{
		"id":"resp_123",
		"model":"gpt-5-mini",
		"status":"completed",
		"output":[
			{
				"type":"message",
				"content":[{"type":"text","text":"pong"}]
			}
		],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
	}`)

	resp, err := p.parseResponse(body, nil)
	if err != nil {
		t.Fatalf("parseResponse() error = %v", err)
	}
	if got := resp.TextContent(); got != "pong" {
		t.Fatalf("TextContent()=%q, want %q", got, "pong")
	}
}

func TestValidateRequest_RejectsStopSequences(t *testing.T) {
	p := &Provider{}
	err := p.validateRequest(&types.MessageRequest{
		Model:         "gpt-5-mini",
		StopSequences: []string{"DONE"},
	})
	if err == nil {
		t.Fatal("expected stop sequence validation error")
	}
}

func TestValidateRequest_RejectsAudioSTTBlocks(t *testing.T) {
	p := &Provider{}
	err := p.validateRequest(&types.MessageRequest{
		Model: "gpt-5-mini",
		Messages: []types.Message{
			{
				Role: "user",
				Content: []types.ContentBlock{
					types.AudioSTTBlock{
						Type: "audio_stt",
						Source: types.AudioSource{
							Type:      "base64",
							MediaType: "audio/wav",
							Data:      "AAAA",
						},
					},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected audio_stt validation error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if oe.Type != ErrInvalidRequest {
		t.Fatalf("error type = %q, want %q", oe.Type, ErrInvalidRequest)
	}
	if oe.Param != "messages" {
		t.Fatalf("param = %q, want messages", oe.Param)
	}
}

func TestEnsurePropertiesInSchema_AlwaysIncludesProperties(t *testing.T) {
	schema := &types.JSONSchema{Type: "object"}
	raw := ensurePropertiesInSchema(schema)

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if _, ok := decoded["properties"]; !ok {
		t.Fatalf("decoded schema missing properties: %#v", decoded)
	}
}
