package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
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

func TestBuildChatTools_DefaultNamesAndHandlers(t *testing.T) {
	t.Parallel()

	tools := buildChatTools(chatConfig{
		TavilyAPIKey: "tvly-test",
	})
	if len(tools) != 3 {
		t.Fatalf("len(tools)=%d, want 3", len(tools))
	}
	if tools[0].Name != "vai_web_search" {
		t.Fatalf("tools[0].Name=%q, want %q", tools[0].Name, "vai_web_search")
	}
	if tools[1].Name != "vai_web_fetch" {
		t.Fatalf("tools[1].Name=%q, want %q", tools[1].Name, "vai_web_fetch")
	}
	if tools[2].Name != "talk_to_user" {
		t.Fatalf("tools[2].Name=%q, want %q", tools[2].Name, "talk_to_user")
	}
	if tools[0].Handler == nil {
		t.Fatalf("tools[0].Handler is nil")
	}
	if tools[1].Handler == nil {
		t.Fatalf("tools[1].Handler is nil")
	}
	if tools[2].Handler == nil {
		t.Fatalf("tools[2].Handler is nil")
	}
}

func TestComposeSystemPrompt_AppendsTalkInstruction(t *testing.T) {
	t.Parallel()

	withoutBase := composeSystemPrompt("")
	if !strings.Contains(withoutBase, `talk_to_user`) {
		t.Fatalf("missing talk_to_user instruction in prompt: %q", withoutBase)
	}
	if !strings.Contains(withoutBase, `{"content":"..."}`) {
		t.Fatalf("missing canonical content shape in prompt: %q", withoutBase)
	}

	withBase := composeSystemPrompt("Be concise.")
	if !strings.Contains(withBase, "Be concise.") {
		t.Fatalf("missing user prompt prefix: %q", withBase)
	}
	if !strings.Contains(withBase, talkToUserSystemInstruction) {
		t.Fatalf("missing enforced talk instruction suffix: %q", withBase)
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

func TestBuildStreamCallbacks_PrintsSingleLineToolStreamAndText(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(2, "", "vai_web_search")
	callbacks.OnToolInputDelta(2, "", "", `{"query":"recent`)
	callbacks.OnToolInputDelta(2, "", "", ` news"}`)
	callbacks.OnToolUseStop(2, "", "vai_web_search")
	callbacks.OnToolCallStart("call_1", "vai_web_search", nil)
	callbacks.OnTextDelta("hello")

	got := out.String()
	if strings.Contains(got, "[tool-stream:") {
		t.Fatalf("unexpected legacy tool-stream marker output, got=%q", got)
	}
	if !strings.Contains(got, "\nTool-vai_web_search: {\"query\":\"recent news\"}\n") {
		t.Fatalf("missing single-line tool stream output, got=%q", got)
	}
	if !strings.Contains(got, "[tool] vai_web_search\n") {
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

func TestBuildStreamCallbacks_ToolStreamNewlineContinuationHasNoPrefix(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(1, "", "vai_web_search")
	callbacks.OnToolInputDelta(1, "", "", "{\"query\":\"line1\nline2\"}")
	callbacks.OnToolUseStop(1, "", "vai_web_search")

	got := out.String()
	if !strings.Contains(got, "\nTool-vai_web_search: {\"query\":\"line1\nline2\"}\n") {
		t.Fatalf("unexpected newline continuation rendering, got=%q", got)
	}
	if strings.Contains(got, "\nTool-vai_web_search: line2") {
		t.Fatalf("continuation line was incorrectly prefixed, got=%q", got)
	}
}

func TestBuildStreamCallbacks_ToolStreamInterleavedIndicesStaySeparated(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(1, "", "tool_a")
	callbacks.OnToolUseStart(2, "", "tool_b")
	callbacks.OnToolInputDelta(1, "", "", `{"a":`)
	callbacks.OnToolInputDelta(2, "", "", `{"b":`)
	callbacks.OnToolInputDelta(1, "", "", `1}`)
	callbacks.OnToolUseStop(1, "", "tool_a")
	callbacks.OnToolUseStop(2, "", "tool_b")

	got := out.String()
	if strings.Count(got, "Tool-tool_a:") != 2 {
		t.Fatalf("expected tool_a to open two segments due to interleaving, got=%q", got)
	}
	if strings.Count(got, "Tool-tool_b:") != 1 {
		t.Fatalf("expected one tool_b segment, got=%q", got)
	}
	firstA := strings.Index(got, "Tool-tool_a:")
	firstB := strings.Index(got, "Tool-tool_b:")
	lastA := strings.LastIndex(got, "Tool-tool_a:")
	if !(firstA >= 0 && firstB > firstA && lastA > firstB) {
		t.Fatalf("tool stream interleaving order invalid, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TextDeltaStartsAfterToolLineTermination(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(3, "", "tool_x")
	callbacks.OnToolInputDelta(3, "", "", "abc")
	callbacks.OnTextDelta("hello")

	got := out.String()
	if !strings.Contains(got, "\nTool-tool_x: abc\nhello") {
		t.Fatalf("assistant text did not start on a new line after tool stream, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TalkToUser_StreamsExtractedSpeechOnly(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(0, "call_1", "talk_to_user")
	callbacks.OnToolInputDelta(0, "call_1", "talk_to_user", `{"content":"hel`)
	callbacks.OnToolInputDelta(0, "call_1", "talk_to_user", `lo"}`)
	callbacks.OnToolUseStop(0, "call_1", "talk_to_user")

	got := out.String()
	if strings.Contains(got, "Tool-talk_to_user:") {
		t.Fatalf("unexpected raw talk tool line prefix, got=%q", got)
	}
	if strings.Contains(got, `{"content":"`) {
		t.Fatalf("unexpected raw json output for talk_to_user, got=%q", got)
	}
	if !strings.Contains(got, "hello\n") {
		t.Fatalf("expected streamed spoken text, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TalkToUser_NewlineContinuationHasNoPrefix(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(0, "", "talk_to_user")
	callbacks.OnToolInputDelta(0, "", "talk_to_user", `{"content":"line1\nline2"}`)
	callbacks.OnToolUseStop(0, "", "talk_to_user")

	got := out.String()
	if !strings.Contains(got, "line1\nline2\n") {
		t.Fatalf("expected newline continuation without prefix, got=%q", got)
	}
	if strings.Contains(got, "Tool-talk_to_user") {
		t.Fatalf("unexpected talk tool prefix on continuation, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TalkToUser_HandlesEscapesAcrossChunks(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(5, "", "talk_to_user")
	callbacks.OnToolInputDelta(5, "", "talk_to_user", `{"content":"he\`)
	callbacks.OnToolInputDelta(5, "", "talk_to_user", `nllo \u00`)
	callbacks.OnToolInputDelta(5, "", "talk_to_user", `41 \"ok\""}`)
	callbacks.OnToolUseStop(5, "", "talk_to_user")

	got := out.String()
	if !strings.Contains(got, "he\nllo A \"ok\"\n") {
		t.Fatalf("expected decoded escaped speech, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TalkToUser_LocksFirstDetectedCandidateKey(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(9, "call_lock", "talk_to_user")
	callbacks.OnToolInputDelta(9, "call_lock", "talk_to_user", `{"message":"hel`)
	callbacks.OnToolInputDelta(9, "call_lock", "talk_to_user", `lo","content":"should-not-emit"}`)
	callbacks.OnToolUseStop(9, "call_lock", "talk_to_user")

	got := out.String()
	if !strings.Contains(got, "hello\n") {
		t.Fatalf("expected spoken output from first detected key, got=%q", got)
	}
	if strings.Contains(got, "should-not-emit") {
		t.Fatalf("unexpected output from later candidate key after lock, got=%q", got)
	}
}

func TestBuildStreamCallbacks_TalkToUser_HidesExecutionMarker(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state)

	callbacks.OnToolUseStart(8, "call_talk", "talk_to_user")
	callbacks.OnToolInputDelta(8, "call_talk", "talk_to_user", `{"content":"hi"}`)
	callbacks.OnToolUseStop(8, "call_talk", "talk_to_user")
	callbacks.OnToolCallStart("call_talk", "talk_to_user", map[string]any{"content": "hi"})
	callbacks.OnToolCallStart("call_search", "vai_web_search", nil)

	got := out.String()
	if strings.Contains(got, "[tool] talk_to_user") {
		t.Fatalf("unexpected talk_to_user execution marker, got=%q", got)
	}
	if !strings.Contains(got, "[tool] vai_web_search") {
		t.Fatalf("expected non-talk execution marker, got=%q", got)
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
				types.TextBlock{Type: "text", Text: "new-user"},
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

	result.Messages[0] = types.Message{Role: "assistant", Content: vai.Text("mutated")}
	if state.history[0].TextContent() != "new-user" {
		t.Fatalf("history aliasing detected, got=%q", state.history[0].TextContent())
	}
}

func TestSyncHistoryFromRunResult_FallbackCopiesResponseContentSlice(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{
		history: []vai.Message{
			{Role: "user", Content: vai.Text("u1")},
		},
	}
	orig := []types.ContentBlock{
		types.TextBlock{Type: "text", Text: "x"},
	}
	result := &vai.RunResult{
		Response: &vai.Response{
			MessageResponse: &types.MessageResponse{
				Type:    "message",
				Role:    "assistant",
				Content: orig,
			},
		},
	}
	syncHistoryFromRunResult(state, result, "")
	if len(state.history) != 2 {
		t.Fatalf("history len=%d, want 2", len(state.history))
	}
	cloned, ok := state.history[1].Content.([]types.ContentBlock)
	if !ok {
		t.Fatalf("history[1].Content type=%T, want []types.ContentBlock", state.history[1].Content)
	}
	if len(cloned) != 1 {
		t.Fatalf("len(content)=%d, want 1", len(cloned))
	}
	orig[0] = types.TextBlock{Type: "text", Text: "mutated"}
	tb, ok := cloned[0].(types.TextBlock)
	if !ok || tb.Text != "x" {
		t.Fatalf("content aliasing detected, got %#v", cloned[0])
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

func TestFormatError_IncludesCoreFields(t *testing.T) {
	t.Parallel()

	retryAfter := 7
	err := &core.Error{
		Type:          core.ErrAPI,
		Message:       "internal error",
		Code:          "INTERNAL",
		Param:         "messages[0]",
		RequestID:     "req_123",
		RetryAfter:    &retryAfter,
		ProviderError: map[string]any{"status": "RESOURCE_EXHAUSTED", "message": "quota exceeded"},
	}

	got := vai.FormatError(err)
	if !strings.Contains(got, "api_error: internal error") {
		t.Fatalf("missing base error text, got=%q", got)
	}
	if !strings.Contains(got, "param=messages[0]") {
		t.Fatalf("missing param details, got=%q", got)
	}
	if !strings.Contains(got, "code=INTERNAL") {
		t.Fatalf("missing code details, got=%q", got)
	}
	if !strings.Contains(got, "request_id=req_123") {
		t.Fatalf("missing request_id details, got=%q", got)
	}
	if !strings.Contains(got, "retry_after=7s") {
		t.Fatalf("missing retry_after details, got=%q", got)
	}
	if !strings.Contains(got, `provider_error={"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}`) {
		t.Fatalf("missing provider_error details, got=%q", got)
	}
}

func TestFormatError_UnwrapsCauseChain(t *testing.T) {
	t.Parallel()

	root := errors.New("root cause")
	wrapped := fmt.Errorf("layer 1: %w", root)
	err := fmt.Errorf("top level: %w", wrapped)

	got := vai.FormatError(err)
	if !strings.Contains(got, "top level: layer 1: root cause") {
		t.Fatalf("missing top-level text, got=%q", got)
	}
	if !strings.Contains(got, "caused by: layer 1: root cause") {
		t.Fatalf("missing first cause line, got=%q", got)
	}
	if !strings.Contains(got, "caused by: root cause") {
		t.Fatalf("missing root cause line, got=%q", got)
	}
}
