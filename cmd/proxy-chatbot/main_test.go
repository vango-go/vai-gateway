package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
	vai "github.com/vango-go/vai-lite/sdk"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func TestParseChatConfig_DefaultsAndEnv(t *testing.T) {
	t.Parallel()

	cfg, err := parseChatConfig(nil, envMap(map[string]string{
		"OPENAI_API_KEY":      "sk-openai-test",
		"VAI_GATEWAY_API_KEY": "vai_sk_test",
		"TAVILY_API_KEY":      "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig error: %v", err)
	}

	if cfg.BaseURL != defaultBaseURL {
		t.Fatalf("BaseURL=%q, want %q", cfg.BaseURL, defaultBaseURL)
	}
	if cfg.Model != defaultModel {
		t.Fatalf("Model=%q, want %q", cfg.Model, defaultModel)
	}
	if cfg.MaxTokens != defaultMaxTokens {
		t.Fatalf("MaxTokens=%d, want %d", cfg.MaxTokens, defaultMaxTokens)
	}
	if cfg.Timeout != defaultTimeout {
		t.Fatalf("Timeout=%v, want %v", cfg.Timeout, defaultTimeout)
	}
	if cfg.GatewayAPIKey != "vai_sk_test" {
		t.Fatalf("GatewayAPIKey=%q, want %q", cfg.GatewayAPIKey, "vai_sk_test")
	}
	if cfg.ProviderKeys["openai"] != "sk-openai-test" {
		t.Fatalf("ProviderKeys[openai]=%q, want %q", cfg.ProviderKeys["openai"], "sk-openai-test")
	}
	if cfg.TavilyAPIKey != "tvly-test" {
		t.Fatalf("TavilyAPIKey=%q, want %q", cfg.TavilyAPIKey, "tvly-test")
	}
}

func TestParseChatConfig_MissingOpenAIKey(t *testing.T) {
	t.Parallel()

	_, err := parseChatConfig(nil, envMap(map[string]string{
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err == nil {
		t.Fatalf("expected error when OPENAI_API_KEY is missing")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") || !strings.Contains(err.Error(), "oai-resp") {
		t.Fatalf("error=%q, expected OPENAI_API_KEY mention", err.Error())
	}
}

func TestParseChatConfig_GatewayAPIKeyOptional(t *testing.T) {
	t.Parallel()

	cfg, err := parseChatConfig(nil, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig error: %v", err)
	}
	if cfg.GatewayAPIKey != "" {
		t.Fatalf("GatewayAPIKey=%q, want empty", cfg.GatewayAPIKey)
	}

	cfg, err = parseChatConfig([]string{"--gateway-api-key", "vai_sk_explicit"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig with explicit gateway key error: %v", err)
	}
	if cfg.GatewayAPIKey != "vai_sk_explicit" {
		t.Fatalf("GatewayAPIKey=%q, want %q", cfg.GatewayAPIKey, "vai_sk_explicit")
	}
}

func TestParseChatConfig_ModelAndBaseURLValidation(t *testing.T) {
	t.Parallel()

	_, err := parseChatConfig([]string{"--base-url", "not-a-url"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err == nil || !strings.Contains(err.Error(), "base-url") {
		t.Fatalf("expected base-url validation error, got %v", err)
	}

	_, err = parseChatConfig([]string{"--model", "gpt-5-mini"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err == nil || !strings.Contains(err.Error(), "invalid model") {
		t.Fatalf("expected invalid model error, got %v", err)
	}
}

func TestValidateChatConfig_TimeoutAndMaxTokens(t *testing.T) {
	t.Parallel()

	base := chatConfig{
		BaseURL:      "http://127.0.0.1:8080",
		Model:        "oai-resp/gpt-5-mini",
		TavilyAPIKey: "tvly-test",
		ProviderKeys: map[string]string{
			"openai": "sk-openai-test",
		},
	}

	cfg := base
	cfg.MaxTokens = 0
	cfg.Timeout = 90 * time.Second
	if err := validateChatConfig(cfg); err == nil || !strings.Contains(err.Error(), "max-tokens") {
		t.Fatalf("expected max-tokens error, got %v", err)
	}

	cfg = base
	cfg.MaxTokens = 512
	cfg.Timeout = 0
	if err := validateChatConfig(cfg); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestParseChatConfig_AnthropicModelRequiresAnthropicKey(t *testing.T) {
	t.Parallel()

	_, err := parseChatConfig([]string{"--model", "anthropic/claude-haiku-4-5"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY validation error, got %v", err)
	}

	cfg, err := parseChatConfig([]string{"--model", "anthropic/claude-haiku-4-5"}, envMap(map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"OPENAI_API_KEY":    "sk-openai-test",
		"TAVILY_API_KEY":    "tvly-test",
	}))
	if err != nil {
		t.Fatalf("expected anthropic model parse to succeed, got %v", err)
	}
	if cfg.ProviderKeys["anthropic"] != "sk-ant-test" {
		t.Fatalf("ProviderKeys[anthropic]=%q, want %q", cfg.ProviderKeys["anthropic"], "sk-ant-test")
	}
}

func TestCollectProviderKeys_GeminiFallbackToGoogle(t *testing.T) {
	t.Parallel()

	keys := collectProviderKeys(envMap(map[string]string{
		"GOOGLE_API_KEY": "google-key",
	}))
	if keys["gemini"] != "google-key" {
		t.Fatalf("gemini key fallback=%q, want %q", keys["gemini"], "google-key")
	}

	keys = collectProviderKeys(envMap(map[string]string{
		"GEMINI_API_KEY": "gemini-key",
		"GOOGLE_API_KEY": "google-key",
	}))
	if keys["gemini"] != "gemini-key" {
		t.Fatalf("gemini key precedence=%q, want %q", keys["gemini"], "gemini-key")
	}
}

func TestParseChatConfig_MissingTavilyKey(t *testing.T) {
	t.Parallel()

	_, err := parseChatConfig(nil, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
	}))
	if err == nil {
		t.Fatalf("expected error when TAVILY_API_KEY is missing")
	}
	if !strings.Contains(err.Error(), "TAVILY_API_KEY") {
		t.Fatalf("error=%q, expected TAVILY_API_KEY mention", err.Error())
	}
}

func TestBuildClientOptions_RegistersProviderKeys(t *testing.T) {
	t.Parallel()

	opts := buildClientOptions(chatConfig{
		BaseURL:       "http://127.0.0.1:8080",
		GatewayAPIKey: "vai_sk_test",
		ProviderKeys: map[string]string{
			"openai":    "sk-openai-test",
			"anthropic": "sk-ant-test",
			"gemini":    "sk-gemini-test",
		},
	})
	client := vai.NewClient(opts...)

	if got := client.Engine().GetAPIKey("openai"); got != "sk-openai-test" {
		t.Fatalf("openai key=%q", got)
	}
	if got := client.Engine().GetAPIKey("anthropic"); got != "sk-ant-test" {
		t.Fatalf("anthropic key=%q", got)
	}
	if got := client.Engine().GetAPIKey("gemini"); got != "sk-gemini-test" {
		t.Fatalf("gemini key=%q", got)
	}
}

func TestBuildWebTools_DefaultNamesAndHandlers(t *testing.T) {
	t.Parallel()

	tools := buildWebTools(chatConfig{
		TavilyAPIKey: "tvly-test",
	})
	if len(tools) != 2 {
		t.Fatalf("len(tools)=%d, want 2", len(tools))
	}
	if tools[0].Name != "vai_web_search" {
		t.Fatalf("tools[0].Name=%q, want %q", tools[0].Name, "vai_web_search")
	}
	if tools[1].Name != "vai_web_fetch" {
		t.Fatalf("tools[1].Name=%q, want %q", tools[1].Name, "vai_web_fetch")
	}
	if tools[0].Handler == nil {
		t.Fatalf("tools[0].Handler is nil")
	}
	if tools[1].Handler == nil {
		t.Fatalf("tools[1].Handler is nil")
	}
}

func TestHandleSlashCommand_ShowCurrentModel(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{currentModel: "oai-resp/gpt-5-mini"}
	cfg := chatConfig{}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/model", state, cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if !strings.Contains(out.String(), "current model: oai-resp/gpt-5-mini") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr output: %q", errOut.String())
	}
}

func TestHandleSlashCommand_ModelSwitchValid(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{currentModel: "oai-resp/gpt-5-mini"}
	cfg := chatConfig{
		ProviderKeys: map[string]string{
			"anthropic": "sk-ant-test",
			"openai":    "sk-openai-test",
		},
	}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/model:anthropic/claude-haiku-4-5", state, cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if state.currentModel != "anthropic/claude-haiku-4-5" {
		t.Fatalf("currentModel=%q", state.currentModel)
	}
	if !strings.Contains(out.String(), "model switched: oai-resp/gpt-5-mini -> anthropic/claude-haiku-4-5") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr output: %q", errOut.String())
	}
}

func TestHandleSlashCommand_InvalidModelFormatRejected(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{currentModel: "oai-resp/gpt-5-mini"}
	cfg := chatConfig{
		ProviderKeys: map[string]string{"openai": "sk-openai-test"},
	}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/model:gpt-5-mini", state, cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if state.currentModel != "oai-resp/gpt-5-mini" {
		t.Fatalf("currentModel changed unexpectedly: %q", state.currentModel)
	}
	if !strings.Contains(errOut.String(), "model switch error: invalid model") {
		t.Fatalf("unexpected stderr: %q", errOut.String())
	}
}

func TestHandleSlashCommand_EmptySetterRejected(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{currentModel: "oai-resp/gpt-5-mini"}
	cfg := chatConfig{
		ProviderKeys: map[string]string{"openai": "sk-openai-test"},
	}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/model:", state, cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if state.currentModel != "oai-resp/gpt-5-mini" {
		t.Fatalf("currentModel changed unexpectedly: %q", state.currentModel)
	}
	if !strings.Contains(errOut.String(), "model switch error: model must not be empty") {
		t.Fatalf("unexpected stderr: %q", errOut.String())
	}
}

func TestHandleSlashCommand_MissingProviderKeyKeepsModel(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{currentModel: "oai-resp/gpt-5-mini"}
	cfg := chatConfig{
		ProviderKeys: map[string]string{"openai": "sk-openai-test"},
	}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/model:anthropic/claude-haiku-4-5", state, cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if state.currentModel != "oai-resp/gpt-5-mini" {
		t.Fatalf("currentModel changed unexpectedly: %q", state.currentModel)
	}
	if !strings.Contains(errOut.String(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY hint, got: %q", errOut.String())
	}
}

func TestHandleSlashCommand_ModelSwitchPreservesHistory(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{
		currentModel: "oai-resp/gpt-5-mini",
		history: []vai.Message{
			{Role: "user", Content: vai.Text("hello")},
			{Role: "assistant", Content: vai.Text("hi")},
		},
	}
	cfg := chatConfig{
		ProviderKeys: map[string]string{
			"anthropic": "sk-ant-test",
			"openai":    "sk-openai-test",
		},
	}
	var out bytes.Buffer

	beforeLen := len(state.history)
	handled, err := handleSlashCommand("/model:anthropic/claude-haiku-4-5", state, cfg, &out, io.Discard)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true")
	}
	if len(state.history) != beforeLen {
		t.Fatalf("history length changed: got %d want %d", len(state.history), beforeLen)
	}
}

func TestParseChatConfig_CanonicalizesModelAndSystemPrompt(t *testing.T) {
	t.Parallel()

	cfg, err := parseChatConfig([]string{"--model", " OpenAI/gpt-5-mini ", "--system", "  be concise  "}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig error: %v", err)
	}
	if cfg.Model != "openai/gpt-5-mini" {
		t.Fatalf("Model=%q, want %q", cfg.Model, "openai/gpt-5-mini")
	}
	if cfg.SystemPrompt != "be concise" {
		t.Fatalf("SystemPrompt=%q, want %q", cfg.SystemPrompt, "be concise")
	}
}

func TestRunChatbot_CanonicalizesConfigForRuntimeState(t *testing.T) {
	t.Parallel()

	cfg := chatConfig{
		BaseURL:      "http://127.0.0.1:8080",
		Model:        " OpenAI/gpt-5-mini ",
		MaxTokens:    128,
		Timeout:      2 * time.Second,
		TavilyAPIKey: "tvly-test",
		ProviderKeys: map[string]string{
			"openai": "sk-openai-test",
		},
	}

	var out bytes.Buffer
	err := runChatbot(context.Background(), cfg, strings.NewReader(""), &out, io.Discard)
	if err != nil {
		t.Fatalf("runChatbot error: %v", err)
	}
	if !strings.Contains(out.String(), "using openai/gpt-5-mini") {
		t.Fatalf("banner missing canonical model, output=%q", out.String())
	}
}

func TestBuildStreamCallbacks_PrintsToolStreamLinesAndText(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(2, "", "")
	callbacks.OnToolInputDelta(2, "", "", "{\n\"a\":1}")
	callbacks.OnToolUseStop(2, "", "")
	callbacks.OnToolCallStart("call_1", "vai_web_search", nil)
	callbacks.OnTextDelta("hello")

	got := out.String()
	if !strings.Contains(got, "[tool-stream:start] idx=2 name=- id=-") {
		t.Fatalf("missing tool-stream start line, got=%q", got)
	}
	if !strings.Contains(got, "[tool-stream:delta] idx=2 name=- id=- {\\n\"a\":1}") {
		t.Fatalf("missing/sanitized tool-stream delta line, got=%q", got)
	}
	if !strings.Contains(got, "[tool-stream:stop] idx=2 name=- id=-") {
		t.Fatalf("missing tool-stream stop line, got=%q", got)
	}
	if !strings.Contains(got, "[tool] vai_web_search") {
		t.Fatalf("missing execution tool line, got=%q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("missing streamed text, got=%q", got)
	}
	if !state.sawText {
		t.Fatal("state.sawText=false, want true")
	}
	if state.text.String() != "hello" {
		t.Fatalf("state.text=%q, want %q", state.text.String(), "hello")
	}
}

func TestResolveAssistantDisplay_NoDuplicateWhenTextStreamed(t *testing.T) {
	t.Parallel()

	result := &vai.RunResult{
		Response: &vai.Response{
			MessageResponse: &types.MessageResponse{
				Type:    "message",
				Role:    "assistant",
				Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "fallback"}},
			},
		},
	}

	text, shouldPrint := resolveAssistantDisplay(true, "streamed", "process", result)
	if text != "streamed" {
		t.Fatalf("text=%q, want %q", text, "streamed")
	}
	if shouldPrint {
		t.Fatal("shouldPrint=true, want false")
	}
}

func TestResolveAssistantDisplay_FallbackFromResultThenProcess(t *testing.T) {
	t.Parallel()

	result := &vai.RunResult{
		Response: &vai.Response{
			MessageResponse: &types.MessageResponse{
				Type:    "message",
				Role:    "assistant",
				Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "from result"}},
			},
		},
	}
	text, shouldPrint := resolveAssistantDisplay(false, "", "from process", result)
	if text != "from result" || !shouldPrint {
		t.Fatalf("got (text=%q, shouldPrint=%v), want (%q,true)", text, shouldPrint, "from result")
	}

	text, shouldPrint = resolveAssistantDisplay(false, "", "from process", nil)
	if text != "from process" || !shouldPrint {
		t.Fatalf("got (text=%q, shouldPrint=%v), want (%q,true)", text, shouldPrint, "from process")
	}
}

func TestSyncHistoryFromRunResult_UsesResultMessages(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{
		history: []vai.Message{
			{Role: "user", Content: vai.Text("old")},
		},
	}
	result := &vai.RunResult{
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{
				nil,
				types.TextBlock{Text: "new-user"},
			}},
			{Role: "assistant", Content: vai.Text("new-assistant")},
		},
	}

	syncHistoryFromRunResult(state, result, "ignored")

	if len(state.history) != 2 {
		t.Fatalf("history len=%d, want 2", len(state.history))
	}
	if state.history[0].TextContent() != "new-user" || state.history[1].TextContent() != "new-assistant" {
		t.Fatalf("history content unexpected: %#v", state.history)
	}
	firstBlocks := state.history[0].ContentBlocks()
	if len(firstBlocks) != 1 {
		t.Fatalf("sanitized first message blocks len=%d, want 1", len(firstBlocks))
	}
	if tb, ok := firstBlocks[0].(types.TextBlock); !ok || tb.Type != "text" || tb.Text != "new-user" {
		t.Fatalf("sanitized first block=%#v, want TextBlock{Type:text,Text:new-user}", firstBlocks[0])
	}

	result.Messages[0] = types.Message{Role: "assistant", Content: vai.Text("mutated")}
	if state.history[0].TextContent() != "new-user" {
		t.Fatalf("history aliasing detected, got=%q", state.history[0].TextContent())
	}
}

func TestSanitizeContentBlocksForInput_DropsUnknownEmptyTypeAndNil(t *testing.T) {
	t.Parallel()

	blocks := sanitizeContentBlocksForInput([]vai.ContentBlock{
		nil,
		types.UnknownContentBlock{Type: ""},
		types.TextBlock{Type: "", Text: "ok"},
	})
	if len(blocks) != 1 {
		t.Fatalf("len(blocks)=%d, want 1", len(blocks))
	}
	tb, ok := blocks[0].(types.TextBlock)
	if !ok {
		t.Fatalf("blocks[0]=%T, want TextBlock", blocks[0])
	}
	if tb.Type != "text" || tb.Text != "ok" {
		t.Fatalf("TextBlock=%#v, want Type=text Text=ok", tb)
	}
}

func TestSyncHistoryFromRunResult_FallbackUsesFullResponseContent(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{
		history: []vai.Message{
			{Role: "user", Content: vai.Text("u1")},
		},
	}
	result := &vai.RunResult{
		Response: &vai.Response{
			MessageResponse: &types.MessageResponse{
				Type: "message",
				Role: "assistant",
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "call_1",
						Name:  "talk_to_user",
						Input: map[string]any{"message": "hi"},
					},
				},
			},
		},
	}

	syncHistoryFromRunResult(state, result, "fallback text")

	if len(state.history) != 2 {
		t.Fatalf("history len=%d, want 2", len(state.history))
	}
	if state.history[1].Role != "assistant" {
		t.Fatalf("history[1].Role=%q, want assistant", state.history[1].Role)
	}
	blocks := state.history[1].ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("history[1].ContentBlocks len=%d, want 1", len(blocks))
	}
	if _, ok := blocks[0].(types.ToolUseBlock); !ok {
		t.Fatalf("history[1].ContentBlocks[0]=%T, want ToolUseBlock", blocks[0])
	}
}

func TestSyncHistoryFromRunResult_FallbackUsesAssistantText(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{
		history: []vai.Message{
			{Role: "user", Content: vai.Text("u1")},
		},
	}
	result := &vai.RunResult{
		Response: &vai.Response{
			MessageResponse: &types.MessageResponse{
				Type:    "message",
				Role:    "assistant",
				Content: nil,
			},
		},
	}

	syncHistoryFromRunResult(state, result, "assistant fallback")
	if len(state.history) != 2 {
		t.Fatalf("history len=%d, want 2", len(state.history))
	}
	if got := state.history[1].TextContent(); got != "assistant fallback" {
		t.Fatalf("assistant text=%q, want %q", got, "assistant fallback")
	}
}
