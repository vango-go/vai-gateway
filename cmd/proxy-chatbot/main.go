package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/dotenv"
	"github.com/vango-go/vai-lite/pkg/core"
	vai "github.com/vango-go/vai-lite/sdk"
)

const (
	defaultBaseURL   = "http://127.0.0.1:8080"
	defaultModel     = "oai-resp/gpt-5-mini"
	defaultMaxTokens = 512
	defaultTimeout   = 90 * time.Second
)

const talkToUserSystemInstruction = `You must use the talk_to_user tool for any user-facing speech.
When talking to the user, call talk_to_user with JSON arguments shaped like {"content":"..."}.
You may use other tools (e.g. web tools) first, then talk_to_user to present results.
Do not duplicate the same spoken content as plain assistant text.`

type chatConfig struct {
	BaseURL       string
	Model         string
	MaxTokens     int
	Timeout       time.Duration
	SystemPrompt  string
	GatewayAPIKey string
	ProviderKeys  map[string]string
}

type streamPrintState struct {
	sawText bool
	text    strings.Builder

	toolMetaByIndex         map[int]toolMeta
	toolLineStartedByIndex  map[int]bool
	openToolLineIndex       int
	openToolLineAtLineStart bool
	toolStateInitialized    bool

	talkDecoderByIndex          map[int]*vai.ToolArgStringDecoder
	talkPrintedByIndex          map[int]bool
	talkEndedWithNewlineByIndex map[int]bool
}

type toolMeta struct {
	id   string
	name string
}

func parseChatConfig(args []string, getenv func(string) string) (chatConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := chatConfig{}
	fs := flag.NewFlagSet("proxy-chatbot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.BaseURL, "base-url", defaultBaseURL, "VAI gateway base URL")
	fs.StringVar(&cfg.Model, "model", defaultModel, "provider/model to use")
	fs.IntVar(&cfg.MaxTokens, "max-tokens", defaultMaxTokens, "max output tokens per turn")
	fs.DurationVar(&cfg.Timeout, "timeout", defaultTimeout, "per-turn timeout (e.g. 90s)")
	fs.StringVar(&cfg.SystemPrompt, "system", "", "optional system prompt")
	fs.StringVar(&cfg.GatewayAPIKey, "gateway-api-key", strings.TrimSpace(getenv("VAI_GATEWAY_API_KEY")), "optional gateway api key (or VAI_GATEWAY_API_KEY)")

	if err := fs.Parse(args); err != nil {
		return chatConfig{}, err
	}

	cfg.ProviderKeys = collectProviderKeys(getenv)

	canonicalCfg, err := canonicalizeAndValidateChatConfig(cfg)
	if err != nil {
		return chatConfig{}, err
	}
	return canonicalCfg, nil
}

func collectProviderKeys(getenv func(string) string) map[string]string {
	keys := make(map[string]string)
	setIfPresent := func(provider, envName string) {
		if v := strings.TrimSpace(getenv(envName)); v != "" {
			keys[provider] = v
		}
	}

	setIfPresent("anthropic", "ANTHROPIC_API_KEY")
	setIfPresent("openai", "OPENAI_API_KEY")
	setIfPresent("groq", "GROQ_API_KEY")
	setIfPresent("cerebras", "CEREBRAS_API_KEY")
	setIfPresent("openrouter", "OPENROUTER_API_KEY")
	setIfPresent("gem-dev", "GEMINI_API_KEY")
	if _, ok := keys["gem-dev"]; !ok {
		setIfPresent("gem-dev", "GOOGLE_API_KEY")
	}
	setIfPresent("gem-vert", "VERTEXAI_API_KEY")
	setIfPresent("cartesia", "CARTESIA_API_KEY")
	setIfPresent("elevenlabs", "ELEVENLABS_API_KEY")
	setIfPresent("tavily", "TAVILY_API_KEY")
	setIfPresent("exa", "EXA_API_KEY")
	setIfPresent("firecrawl", "FIRECRAWL_API_KEY")

	return keys
}

func parseModelRef(model string) (provider string, modelName string, canonicalModel string, err error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", "", errors.New("model must not be empty")
	}

	provider, modelName, err = core.ParseModelString(model)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid model %q: expected provider/model", model)
	}

	provider = strings.ToLower(strings.TrimSpace(provider))
	modelName = strings.TrimSpace(modelName)
	if provider == "" || modelName == "" {
		return "", "", "", fmt.Errorf("invalid model %q: provider and model must be non-empty", model)
	}

	return provider, modelName, provider + "/" + modelName, nil
}

func requiredKeySpec(provider string) (envHint string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return "ANTHROPIC_API_KEY", true
	case "openai", "oai-resp":
		return "OPENAI_API_KEY", true
	case "groq":
		return "GROQ_API_KEY", true
	case "cerebras":
		return "CEREBRAS_API_KEY", true
	case "openrouter":
		return "OPENROUTER_API_KEY", true
	case "gem-dev":
		return "GEMINI_API_KEY (or GOOGLE_API_KEY)", true
	case "gem-vert":
		return "VERTEXAI_API_KEY", true
	default:
		return "", false
	}
}

func hasKeyForProvider(provider string, keyStore map[string]string) bool {
	if keyStore == nil {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "oai-resp":
		return strings.TrimSpace(keyStore["openai"]) != ""
	case "gem-dev":
		return strings.TrimSpace(keyStore["gem-dev"]) != ""
	case "gem-vert":
		return strings.TrimSpace(keyStore["gem-vert"]) != ""
	default:
		return strings.TrimSpace(keyStore[strings.ToLower(strings.TrimSpace(provider))]) != ""
	}
}

func validateModelWithKeys(model string, keys map[string]string) (string, error) {
	provider, _, canonicalModel, err := parseModelRef(model)
	if err != nil {
		return "", err
	}

	if envHint, ok := requiredKeySpec(provider); ok && !hasKeyForProvider(provider, keys) {
		return "", fmt.Errorf("missing provider key for %s (set %s)", provider, envHint)
	}
	return canonicalModel, nil
}

func validateChatConfig(cfg chatConfig) error {
	_, err := canonicalizeAndValidateChatConfig(cfg)
	return err
}

func canonicalizeAndValidateChatConfig(cfg chatConfig) (chatConfig, error) {
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.GatewayAPIKey = strings.TrimSpace(cfg.GatewayAPIKey)
	cfg.ProviderKeys = canonicalizeProviderKeys(cfg.ProviderKeys)

	if cfg.BaseURL == "" {
		return chatConfig{}, errors.New("base-url must not be empty")
	}
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil || strings.TrimSpace(baseURL.Scheme) == "" || strings.TrimSpace(baseURL.Host) == "" {
		return chatConfig{}, errors.New("base-url must be a valid absolute URL")
	}
	if baseURL.User != nil {
		return chatConfig{}, errors.New("base-url must not include credentials")
	}
	canonicalModel, err := validateModelWithKeys(cfg.Model, cfg.ProviderKeys)
	if err != nil {
		return chatConfig{}, err
	}
	cfg.Model = canonicalModel
	if cfg.MaxTokens <= 0 {
		return chatConfig{}, errors.New("max-tokens must be > 0")
	}
	if cfg.Timeout <= 0 {
		return chatConfig{}, errors.New("timeout must be > 0")
	}
	if !hasKeyForProvider("tavily", cfg.ProviderKeys) {
		return chatConfig{}, errors.New("TAVILY_API_KEY is required to enable gateway vai_web_search and vai_web_fetch")
	}
	return cfg, nil
}

func canonicalizeProviderKeys(keys map[string]string) map[string]string {
	if len(keys) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(keys))
	for k, v := range keys {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		out[key] = val
	}
	return out
}

func buildClientOptions(cfg chatConfig) []vai.ClientOption {
	opts := []vai.ClientOption{
		vai.WithBaseURL(cfg.BaseURL),
	}
	if cfg.GatewayAPIKey != "" {
		opts = append(opts, vai.WithGatewayAPIKey(cfg.GatewayAPIKey))
	}

	// Keep provider registration explicit and stable.
	for _, provider := range []string{"anthropic", "openai", "groq", "cerebras", "openrouter", "gem-dev", "gem-vert", "cartesia", "elevenlabs", "tavily", "exa", "firecrawl"} {
		if key := strings.TrimSpace(cfg.ProviderKeys[provider]); key != "" {
			opts = append(opts, vai.WithProviderKey(provider, key))
		}
	}
	return opts
}

func buildChatTools(_ chatConfig) []vai.ToolWithHandler {
	type talkToUserInput struct {
		Content string `json:"content" desc:"Exact text to speak to the user"`
	}
	return []vai.ToolWithHandler{
		vai.VAIWebSearch(vai.Tavily),
		vai.VAIWebFetch(vai.Tavily),
		vai.MakeTool("talk_to_user", "Speak text directly to the user via the voice/output channel.", func(ctx context.Context, input talkToUserInput) (string, error) {
			return "delivered", nil
		}),
	}
}

type chatRuntime struct {
	currentModel string
	history      []vai.Message
}

func handleSlashCommand(line string, state *chatRuntime, cfg chatConfig, out io.Writer, errOut io.Writer) (handled bool, err error) {
	if state == nil {
		return false, errors.New("chat state must not be nil")
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}

	switch {
	case line == "/model":
		fmt.Fprintf(out, "current model: %s\n", state.currentModel)
		return true, nil
	case strings.HasPrefix(line, "/model:"):
		target := strings.TrimSpace(strings.TrimPrefix(line, "/model:"))
		nextModel, validationErr := validateModelWithKeys(target, cfg.ProviderKeys)
		if validationErr != nil {
			fmt.Fprintf(errOut, "model switch error: %v\n", validationErr)
			return true, nil
		}
		prev := state.currentModel
		state.currentModel = nextModel
		fmt.Fprintf(out, "model switched: %s -> %s\n", prev, state.currentModel)
		return true, nil
	default:
		return false, nil
	}
}

func runChatbot(ctx context.Context, cfg chatConfig, in io.Reader, out io.Writer, errOut io.Writer) error {
	canonicalCfg, err := canonicalizeAndValidateChatConfig(cfg)
	if err != nil {
		return err
	}
	cfg = canonicalCfg

	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}

	client := vai.NewClient(buildClientOptions(cfg)...)
	chatTools := buildChatTools(cfg)

	fmt.Fprintf(out, "Proxy chatbot connected to %s using %s\n", cfg.BaseURL, cfg.Model)
	fmt.Fprintln(out, "Tools enabled: talk_to_user, vai_web_search, vai_web_fetch")
	fmt.Fprintln(out, "Type /exit or /quit to stop. Use /model to view and /model:{provider}/{model} to switch.")

	scanner := bufio.NewScanner(in)
	state := chatRuntime{
		currentModel: cfg.Model,
		history:      make([]vai.Message, 0, 32),
	}

	for {
		fmt.Fprint(out, "> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read input: %w", err)
			}
			fmt.Fprintln(out)
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			fmt.Fprintln(out, "bye")
			return nil
		}

		if handled, err := handleSlashCommand(line, &state, cfg, out, errOut); err != nil {
			return err
		} else if handled {
			continue
		}

		beforeLen := len(state.history)
		state.history = append(state.history, vai.Message{
			Role:    "user",
			Content: vai.Text(line),
		})

		req := &vai.MessageRequest{
			Model:     state.currentModel,
			Messages:  state.history,
			MaxTokens: cfg.MaxTokens,
		}
		req.System = composeSystemPrompt(cfg.SystemPrompt)

		turnCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		stream, err := client.Messages.RunStream(turnCtx, req, vai.WithTools(chatTools...))
		if err != nil {
			cancel()
			state.history = state.history[:beforeLen]
			fmt.Fprintf(errOut, "run stream setup error: %s\n", vai.FormatError(err))
			continue
		}

		printState := streamPrintState{}
		processText, processErr := stream.Process(buildStreamCallbacks(out, &printState))
		_ = stream.Close()

		if processErr != nil {
			cancel()
			state.history = state.history[:beforeLen]
			fmt.Fprintln(out)
			fmt.Fprintf(errOut, "run stream error: %s\n", vai.FormatError(processErr))
			continue
		}

		result := stream.Result()
		assistantText, shouldPrintFallback := resolveAssistantDisplay(printState.sawText, printState.text.String(), processText, result)
		if shouldPrintFallback {
			fmt.Fprint(out, assistantText)
		}
		fmt.Fprintln(out)
		cancel()

		syncHistoryFromRunResult(&state, result, assistantText)
	}
}

func buildStreamCallbacks(out io.Writer, printState *streamPrintState) vai.StreamCallbacks {
	if out == nil {
		out = os.Stdout
	}
	if printState == nil {
		printState = &streamPrintState{}
	}
	initToolStreamState(printState)
	return vai.StreamCallbacks{
		OnTextDelta: func(text string) {
			closeOpenToolStreamLine(out, printState)
			closeOpenTalkStreamLines(out, printState)
			printState.sawText = true
			printState.text.WriteString(text)
			fmt.Fprint(out, text)
		},
		OnToolUseStart: func(index int, id, name string) {
			meta := printState.toolMetaByIndex[index]
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				meta.id = trimmed
			}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				meta.name = trimmed
			}
			printState.toolMetaByIndex[index] = meta
		},
		OnToolInputDelta: func(index int, id, name, partialJSON string) {
			meta := printState.toolMetaByIndex[index]
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				meta.id = trimmed
			}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				meta.name = trimmed
			}
			printState.toolMetaByIndex[index] = meta

			if isTalkToUserTool(meta.name) {
				closeOpenToolStreamLine(out, printState)
				decoder := printState.talkDecoderByIndex[index]
				if decoder == nil {
					decoder = vai.NewToolArgStringDecoder(vai.ToolArgStringDecoderOptions{
						Keys: []string{"content", "message", "text"},
					})
					printState.talkDecoderByIndex[index] = decoder
				}
				update := decoder.Push(partialJSON)
				if !update.Found || update.Delta == "" {
					return
				}
				emitTalkSuffix(out, printState, index, update.Delta)
				return
			}
			streamToolInputDelta(out, printState, index, displayToolName(meta.name), partialJSON)
		},
		OnToolUseStop: func(index int, id, name string) {
			meta := printState.toolMetaByIndex[index]
			toolName := strings.TrimSpace(meta.name)
			if toolName == "" {
				toolName = strings.TrimSpace(name)
			}

			if printState.openToolLineIndex == index {
				closeOpenToolStreamLine(out, printState)
			}
			if isTalkToUserTool(toolName) && printState.talkPrintedByIndex[index] && !printState.talkEndedWithNewlineByIndex[index] {
				fmt.Fprintln(out)
			}
			delete(printState.toolMetaByIndex, index)
			delete(printState.toolLineStartedByIndex, index)
			delete(printState.talkDecoderByIndex, index)
			delete(printState.talkPrintedByIndex, index)
			delete(printState.talkEndedWithNewlineByIndex, index)
		},
		OnToolCallStart: func(id, name string, _ map[string]any) {
			closeOpenToolStreamLine(out, printState)
			closeOpenTalkStreamLines(out, printState)
			if isTalkToUserTool(name) {
				return
			}
			fmt.Fprintf(out, "[tool] %s\n", name)
		},
	}
}

func resolveAssistantDisplay(sawText bool, streamedText string, processText string, result *vai.RunResult) (string, bool) {
	if sawText {
		return strings.TrimSpace(streamedText), false
	}
	text := ""
	if result != nil && result.Response != nil {
		text = strings.TrimSpace(result.Response.TextContent())
	}
	if text == "" {
		text = strings.TrimSpace(processText)
	}
	return text, text != ""
}

func syncHistoryFromRunResult(state *chatRuntime, result *vai.RunResult, assistantText string) {
	if state == nil {
		return
	}
	if result != nil && len(result.Messages) > 0 {
		state.history = append([]vai.Message(nil), result.Messages...)
		return
	}
	if result != nil && result.Response != nil && len(result.Response.Content) > 0 {
		contentCopy := append([]vai.ContentBlock(nil), result.Response.Content...)
		state.history = append(state.history, vai.Message{
			Role:    "assistant",
			Content: contentCopy,
		})
		return
	}
	if strings.TrimSpace(assistantText) != "" {
		state.history = append(state.history, vai.Message{
			Role:    "assistant",
			Content: vai.Text(strings.TrimSpace(assistantText)),
		})
	}
}

func composeSystemPrompt(userPrompt string) string {
	base := strings.TrimSpace(userPrompt)
	enforced := strings.TrimSpace(talkToUserSystemInstruction)
	if base == "" {
		return enforced
	}
	return base + "\n\n" + enforced
}

func initToolStreamState(state *streamPrintState) {
	if state == nil {
		return
	}
	if !state.toolStateInitialized {
		state.openToolLineIndex = -1
		state.toolStateInitialized = true
	}
	if state.toolMetaByIndex == nil {
		state.toolMetaByIndex = make(map[int]toolMeta)
	}
	if state.toolLineStartedByIndex == nil {
		state.toolLineStartedByIndex = make(map[int]bool)
	}
	if state.talkDecoderByIndex == nil {
		state.talkDecoderByIndex = make(map[int]*vai.ToolArgStringDecoder)
	}
	if state.talkPrintedByIndex == nil {
		state.talkPrintedByIndex = make(map[int]bool)
	}
	if state.talkEndedWithNewlineByIndex == nil {
		state.talkEndedWithNewlineByIndex = make(map[int]bool)
	}
}

func displayToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func isTalkToUserTool(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "talk_to_user")
}

func streamToolInputDelta(out io.Writer, state *streamPrintState, index int, toolName, partialJSON string) {
	if state.openToolLineIndex != index {
		closeOpenToolStreamLine(out, state)
		state.openToolLineIndex = index
		state.openToolLineAtLineStart = false
		state.toolLineStartedByIndex[index] = false
	}
	if !state.toolLineStartedByIndex[index] {
		fmt.Fprintf(out, "\nTool-%s: ", toolName)
		state.toolLineStartedByIndex[index] = true
	}
	if partialJSON == "" {
		return
	}

	fmt.Fprint(out, partialJSON)
	state.openToolLineAtLineStart = partialJSON[len(partialJSON)-1] == '\n'
}

func closeOpenTalkStreamLines(out io.Writer, state *streamPrintState) {
	if state == nil {
		return
	}
	for index := range state.talkPrintedByIndex {
		if state.talkPrintedByIndex[index] && !state.talkEndedWithNewlineByIndex[index] {
			fmt.Fprintln(out)
			state.talkEndedWithNewlineByIndex[index] = true
		}
	}
}

func emitTalkSuffix(out io.Writer, state *streamPrintState, index int, suffix string) {
	if suffix == "" {
		return
	}
	fmt.Fprint(out, suffix)
	state.talkPrintedByIndex[index] = true
	state.talkEndedWithNewlineByIndex[index] = suffix[len(suffix)-1] == '\n'
}

func closeOpenToolStreamLine(out io.Writer, state *streamPrintState) {
	if state == nil || state.openToolLineIndex < 0 {
		return
	}
	index := state.openToolLineIndex
	if state.toolLineStartedByIndex[index] {
		if !state.openToolLineAtLineStart {
			fmt.Fprintln(out)
		}
		state.toolLineStartedByIndex[index] = false
	}
	state.openToolLineIndex = -1
	state.openToolLineAtLineStart = false
}

func main() {
	if err := dotenv.LoadFile(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "proxy-chatbot: %v\n", err)
		os.Exit(1)
	}

	cfg, err := parseChatConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy-chatbot: %v\n", err)
		os.Exit(1)
	}

	if err := runChatbot(context.Background(), cfg, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "proxy-chatbot: %v\n", err)
		os.Exit(1)
	}
}
