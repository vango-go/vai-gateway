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
	if cfg.ProviderKeys["tavily"] != "tvly-test" {
		t.Fatalf("ProviderKeys[tavily]=%q, want %q", cfg.ProviderKeys["tavily"], "tvly-test")
	}
	if cfg.LiveOutputRate != defaultLiveOutputRate {
		t.Fatalf("LiveOutputRate=%q, want %q", cfg.LiveOutputRate, defaultLiveOutputRate)
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

func TestParseChatConfig_LiveOutputRateValidation(t *testing.T) {
	t.Parallel()

	cfg, err := parseChatConfig([]string{"--live-output-rate", "16000"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig error: %v", err)
	}
	if cfg.LiveOutputRate != "16000" {
		t.Fatalf("LiveOutputRate=%q, want %q", cfg.LiveOutputRate, "16000")
	}

	_, err = parseChatConfig([]string{"--live-output-rate", "12345"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err == nil || !strings.Contains(err.Error(), "live-output-rate must be one of: auto, 16000, 8000, 24000") {
		t.Fatalf("expected live-output-rate validation error, got %v", err)
	}

	cfg, err = parseChatConfig([]string{"--live-output-rate", "24000"}, envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai-test",
		"TAVILY_API_KEY": "tvly-test",
	}))
	if err != nil {
		t.Fatalf("parseChatConfig error: %v", err)
	}
	if cfg.LiveOutputRate != "24000" {
		t.Fatalf("LiveOutputRate=%q, want %q", cfg.LiveOutputRate, "24000")
	}
}

func TestValidateChatConfig_TimeoutAndMaxTokens(t *testing.T) {
	t.Parallel()

	base := chatConfig{
		BaseURL: "http://127.0.0.1:8080",
		Model:   "oai-resp/gpt-5-mini",
		ProviderKeys: map[string]string{
			"openai": "sk-openai-test",
			"tavily": "tvly-test",
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
	if keys["gem-dev"] != "google-key" {
		t.Fatalf("gem-dev key fallback=%q, want %q", keys["gem-dev"], "google-key")
	}

	keys = collectProviderKeys(envMap(map[string]string{
		"GEMINI_API_KEY": "gemini-key",
		"GOOGLE_API_KEY": "google-key",
	}))
	if keys["gem-dev"] != "gemini-key" {
		t.Fatalf("gem-dev key precedence=%q, want %q", keys["gem-dev"], "gemini-key")
	}

	keys = collectProviderKeys(envMap(map[string]string{
		"VERTEXAI_API_KEY": "vertex-key",
	}))
	if keys["gem-vert"] != "vertex-key" {
		t.Fatalf("gem-vert key=%q, want %q", keys["gem-vert"], "vertex-key")
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
			"gem-dev":   "sk-gem-dev-test",
			"gem-vert":  "sk-gem-vert-test",
		},
	})
	client := vai.NewClient(opts...)

	if got := client.Engine().GetAPIKey("openai"); got != "sk-openai-test" {
		t.Fatalf("openai key=%q", got)
	}
	if got := client.Engine().GetAPIKey("anthropic"); got != "sk-ant-test" {
		t.Fatalf("anthropic key=%q", got)
	}
	if got := client.Engine().GetAPIKey("gem-dev"); got != "sk-gem-dev-test" {
		t.Fatalf("gem-dev key=%q", got)
	}
	if got := client.Engine().GetAPIKey("gem-vert"); got != "sk-gem-vert-test" {
		t.Fatalf("gem-vert key=%q", got)
	}
}

func TestBuildChatTools_DefaultNamesAndHandlers(t *testing.T) {
	t.Parallel()

	tools := buildChatTools(chatConfig{})
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
	for i, tool := range tools {
		if tool.Handler == nil {
			t.Fatalf("tools[%d].Handler is nil", i)
		}
	}
}

func TestBuildChatTools_VoiceMode_ExcludesTalkToUser(t *testing.T) {
	t.Parallel()

	tools := buildChatTools(chatConfig{VoiceEnabled: true})
	if len(tools) != 2 {
		t.Fatalf("len(tools)=%d, want 2", len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "talk_to_user" {
			t.Fatalf("voice mode should not include talk_to_user tool")
		}
	}
}

func TestComposeSystemPrompt_AppendsTalkInstruction(t *testing.T) {
	t.Parallel()

	withoutBase := composeSystemPrompt("", false)
	if !strings.Contains(withoutBase, `talk_to_user`) {
		t.Fatalf("missing talk_to_user instruction in prompt: %q", withoutBase)
	}
	if !strings.Contains(withoutBase, `{"content":"..."}`) {
		t.Fatalf("missing canonical content shape in prompt: %q", withoutBase)
	}

	withBase := composeSystemPrompt("Be concise.", false)
	if !strings.Contains(withBase, "Be concise.") {
		t.Fatalf("missing user prompt prefix: %q", withBase)
	}
	if !strings.Contains(withBase, talkToUserSystemInstruction) {
		t.Fatalf("missing enforced talk instruction suffix: %q", withBase)
	}

	// Voice mode: no talk_to_user instruction
	voicePrompt := composeSystemPrompt("Be concise.", true)
	if strings.Contains(voicePrompt, "talk_to_user") {
		t.Fatalf("voice mode should not include talk_to_user instruction: %q", voicePrompt)
	}
	if voicePrompt != "Be concise." {
		t.Fatalf("voice mode prompt=%q, want %q", voicePrompt, "Be concise.")
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
		BaseURL:   "http://127.0.0.1:8080",
		Model:     " OpenAI/gpt-5-mini ",
		MaxTokens: 128,
		Timeout:   2 * time.Second,
		ProviderKeys: map[string]string{
			"openai": "sk-openai-test",
			"tavily": "tvly-test",
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

func TestShouldUseTurnVoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cfg   chatConfig
		audio []byte
		want  bool
	}{
		{
			name:  "voice disabled ignores recorded audio",
			cfg:   chatConfig{VoiceEnabled: false},
			audio: []byte{0x01},
			want:  false,
		},
		{
			name:  "voice enabled typed turn stays text-only",
			cfg:   chatConfig{VoiceEnabled: true},
			audio: nil,
			want:  false,
		},
		{
			name:  "voice enabled empty audio stays text-only",
			cfg:   chatConfig{VoiceEnabled: true},
			audio: []byte{},
			want:  false,
		},
		{
			name:  "voice enabled recorded turn uses tts",
			cfg:   chatConfig{VoiceEnabled: true},
			audio: []byte{0x01},
			want:  true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldUseTurnVoice(tc.cfg, tc.audio)
			if got != tc.want {
				t.Fatalf("shouldUseTurnVoice=%v, want %v (audioLen=%d, voiceEnabled=%v)", got, tc.want, len(tc.audio), tc.cfg.VoiceEnabled)
			}
		})
	}
}

func TestBuildStreamCallbacks_PrintsSingleLineToolStreamAndText(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &streamPrintState{}
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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
	callbacks := buildStreamCallbacks(&out, state, nil)

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

func TestWriteAudioUnavailableWarning_EmitsOncePerTurn(t *testing.T) {
	t.Parallel()

	var errOut bytes.Buffer
	state := &streamPrintState{}

	writeAudioUnavailableWarning(&errOut, state, "tts_failed", "first failure")
	writeAudioUnavailableWarning(&errOut, state, "tts_failed", "second failure")

	got := errOut.String()
	if strings.Count(got, "audio unavailable") != 1 {
		t.Fatalf("warning count=%d, want 1; output=%q", strings.Count(got, "audio unavailable"), got)
	}
	if !strings.Contains(got, "first failure") {
		t.Fatalf("missing first warning message, output=%q", got)
	}
	if strings.Contains(got, "second failure") {
		t.Fatalf("unexpected second warning message, output=%q", got)
	}

	writeAudioUnavailableWarning(&errOut, &streamPrintState{}, "tts_failed", "new turn")
	if strings.Count(errOut.String(), "audio unavailable") != 2 {
		t.Fatalf("warning count for two turns=%d, want 2; output=%q", strings.Count(errOut.String(), "audio unavailable"), errOut.String())
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

func withAudioRuntimeHooks(
	t *testing.T,
	newPlayer func() (*pcmPlayer, error),
	newRecorder func() (*pcmRecorder, error),
	stopRecorder func(*pcmRecorder) ([]byte, error),
	closePlayer func(*pcmPlayer) error,
) {
	t.Helper()

	oldNewPlayer := newPCMPlayerFunc
	oldNewRecorder := newPCMRecorderFunc
	oldStopRecorder := stopPCMRecorderFunc
	oldClosePlayer := closePCMPlayerFunc

	if newPlayer != nil {
		newPCMPlayerFunc = newPlayer
	}
	if newRecorder != nil {
		newPCMRecorderFunc = newRecorder
	}
	if stopRecorder != nil {
		stopPCMRecorderFunc = stopRecorder
	}
	if closePlayer != nil {
		closePCMPlayerFunc = closePlayer
	}

	t.Cleanup(func() {
		newPCMPlayerFunc = oldNewPlayer
		newPCMRecorderFunc = oldNewRecorder
		stopPCMRecorderFunc = oldStopRecorder
		closePCMPlayerFunc = oldClosePlayer
	})
}

func TestStartRecordingSession_PrewarmsPlayerAndRecorder(t *testing.T) {
	player := &pcmPlayer{}
	rec := &pcmRecorder{}

	withAudioRuntimeHooks(
		t,
		func() (*pcmPlayer, error) { return player, nil },
		func() (*pcmRecorder, error) { return rec, nil },
		nil,
		nil,
	)

	state := &chatRuntime{}
	var errOut bytes.Buffer
	if err := startRecordingSession(state, &errOut); err != nil {
		t.Fatalf("startRecordingSession error: %v", err)
	}
	if state.pendingPlayer != player {
		t.Fatalf("pendingPlayer=%p, want %p", state.pendingPlayer, player)
	}
	if state.recorder != rec {
		t.Fatalf("recorder=%p, want %p", state.recorder, rec)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr output: %q", errOut.String())
	}
}

func TestStartRecordingSession_PrewarmFailureStillStartsRecorder(t *testing.T) {
	rec := &pcmRecorder{}

	withAudioRuntimeHooks(
		t,
		func() (*pcmPlayer, error) { return nil, errors.New("no audio output") },
		func() (*pcmRecorder, error) { return rec, nil },
		nil,
		nil,
	)

	state := &chatRuntime{}
	var errOut bytes.Buffer
	if err := startRecordingSession(state, &errOut); err != nil {
		t.Fatalf("startRecordingSession error: %v", err)
	}
	if state.pendingPlayer != nil {
		t.Fatalf("pendingPlayer=%p, want nil", state.pendingPlayer)
	}
	if state.recorder != rec {
		t.Fatalf("recorder=%p, want %p", state.recorder, rec)
	}
	if !strings.Contains(errOut.String(), "audio prewarm warning") {
		t.Fatalf("expected prewarm warning, got %q", errOut.String())
	}
}

func TestStartRecordingSession_RecorderFailureClosesPrewarmedPlayer(t *testing.T) {
	player := &pcmPlayer{}
	closeCalls := 0

	withAudioRuntimeHooks(
		t,
		func() (*pcmPlayer, error) { return player, nil },
		func() (*pcmRecorder, error) { return nil, errors.New("mic unavailable") },
		nil,
		func(p *pcmPlayer) error {
			if p == player {
				closeCalls++
			}
			return nil
		},
	)

	state := &chatRuntime{}
	err := startRecordingSession(state, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mic unavailable") {
		t.Fatalf("expected mic error, got %v", err)
	}
	if state.pendingPlayer != nil {
		t.Fatalf("pendingPlayer=%p, want nil", state.pendingPlayer)
	}
	if state.recorder != nil {
		t.Fatalf("recorder=%p, want nil", state.recorder)
	}
	if closeCalls != 1 {
		t.Fatalf("closeCalls=%d, want 1", closeCalls)
	}
}

func TestStopRecordingSession_ConsumesPrewarmedPlayerOnce(t *testing.T) {
	player := &pcmPlayer{}
	state := &chatRuntime{
		recorder:      &pcmRecorder{},
		pendingPlayer: player,
	}

	withAudioRuntimeHooks(
		t,
		nil,
		nil,
		func(_ *pcmRecorder) ([]byte, error) { return []byte{0x01, 0x02}, nil },
		func(_ *pcmPlayer) error { return nil },
	)

	audio, prewarmed, err := stopRecordingSession(state)
	if err != nil {
		t.Fatalf("stopRecordingSession error: %v", err)
	}
	if len(audio) != 2 {
		t.Fatalf("len(audio)=%d, want 2", len(audio))
	}
	if prewarmed != player {
		t.Fatalf("prewarmed=%p, want %p", prewarmed, player)
	}
	if state.pendingPlayer != nil {
		t.Fatalf("pendingPlayer=%p, want nil", state.pendingPlayer)
	}
	if state.recorder != nil {
		t.Fatalf("recorder=%p, want nil", state.recorder)
	}
}

func TestStopRecordingSession_NoAudioClosesConsumedPrewarmedPlayer(t *testing.T) {
	player := &pcmPlayer{}
	closeCalls := 0
	state := &chatRuntime{
		recorder:      &pcmRecorder{},
		pendingPlayer: player,
	}

	withAudioRuntimeHooks(
		t,
		nil,
		nil,
		func(_ *pcmRecorder) ([]byte, error) { return nil, nil },
		func(p *pcmPlayer) error {
			if p == player {
				closeCalls++
			}
			return nil
		},
	)

	audio, prewarmed, err := stopRecordingSession(state)
	if err != nil {
		t.Fatalf("stopRecordingSession error: %v", err)
	}
	if audio != nil {
		t.Fatalf("audio=%v, want nil", audio)
	}
	if prewarmed != nil {
		t.Fatalf("prewarmed=%p, want nil", prewarmed)
	}
	if closeCalls != 1 {
		t.Fatalf("closeCalls=%d, want 1", closeCalls)
	}
	if state.pendingPlayer != nil {
		t.Fatalf("pendingPlayer=%p, want nil", state.pendingPlayer)
	}
}

func TestStopRecordingSession_ErrorClosesConsumedPrewarmedPlayer(t *testing.T) {
	player := &pcmPlayer{}
	closeCalls := 0
	state := &chatRuntime{
		recorder:      &pcmRecorder{},
		pendingPlayer: player,
	}

	withAudioRuntimeHooks(
		t,
		nil,
		nil,
		func(_ *pcmRecorder) ([]byte, error) { return nil, errors.New("stop failed") },
		func(p *pcmPlayer) error {
			if p == player {
				closeCalls++
			}
			return nil
		},
	)

	audio, prewarmed, err := stopRecordingSession(state)
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("expected stop error, got %v", err)
	}
	if audio != nil {
		t.Fatalf("audio=%v, want nil", audio)
	}
	if prewarmed != nil {
		t.Fatalf("prewarmed=%p, want nil", prewarmed)
	}
	if closeCalls != 1 {
		t.Fatalf("closeCalls=%d, want 1", closeCalls)
	}
}

func TestCleanupAudioState_ClosesPendingPlayerAndStopsRecorder(t *testing.T) {
	player := &pcmPlayer{}
	state := &chatRuntime{
		recorder:      &pcmRecorder{},
		pendingPlayer: player,
	}
	stopCalls := 0
	closeCalls := 0

	withAudioRuntimeHooks(
		t,
		nil,
		nil,
		func(_ *pcmRecorder) ([]byte, error) {
			stopCalls++
			return nil, nil
		},
		func(p *pcmPlayer) error {
			if p == player {
				closeCalls++
			}
			return nil
		},
	)

	cleanupAudioState(state)
	if stopCalls != 1 {
		t.Fatalf("stopCalls=%d, want 1", stopCalls)
	}
	if closeCalls != 1 {
		t.Fatalf("closeCalls=%d, want 1", closeCalls)
	}
	if state.recorder != nil {
		t.Fatalf("recorder=%p, want nil", state.recorder)
	}
	if state.pendingPlayer != nil {
		t.Fatalf("pendingPlayer=%p, want nil", state.pendingPlayer)
	}
}
