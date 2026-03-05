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
	defaultBaseURL        = "http://127.0.0.1:8080"
	defaultModel          = "oai-resp/gpt-5-mini"
	defaultMaxTokens      = 512
	defaultTimeout        = 300 * time.Second
	defaultLiveOutputRate = "auto"
)

const talkToUserSystemInstruction = `You are in voice mode. Note that the user's responses are transcribed via STT, you may need to use some judgement on potentiall misspellings.`

type chatConfig struct {
	BaseURL        string
	Model          string
	MaxTokens      int
	Timeout        time.Duration
	SystemPrompt   string
	GatewayAPIKey  string
	ProviderKeys   map[string]string
	VoiceEnabled   bool
	VoiceID        string
	LiveOutputRate string
}

type streamPrintState struct {
	sawText                bool
	text                   strings.Builder
	audioUnavailableWarned bool

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
	fs.BoolVar(&cfg.VoiceEnabled, "voice", false, "enable voice output (TTS via gateway)")
	fs.StringVar(&cfg.VoiceID, "voice-id", "a167e0f3-df7e-4d52-a9c3-f949145efdab", "Cartesia voice ID for TTS")
	fs.StringVar(&cfg.LiveOutputRate, "live-output-rate", defaultLiveOutputRate, "live mode output sample rate: auto, 16000, 8000, or 24000")

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
	cfg.LiveOutputRate = strings.ToLower(strings.TrimSpace(cfg.LiveOutputRate))
	cfg.ProviderKeys = canonicalizeProviderKeys(cfg.ProviderKeys)
	if cfg.LiveOutputRate == "" {
		cfg.LiveOutputRate = defaultLiveOutputRate
	}

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
	switch cfg.LiveOutputRate {
	case "auto", "16000", "8000", "24000":
	default:
		return chatConfig{}, errors.New("live-output-rate must be one of: auto, 16000, 8000, 24000")
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

func buildChatTools(cfg chatConfig) []vai.ToolWithHandler {
	tools := []vai.ToolWithHandler{
		vai.VAIWebSearch(vai.Tavily),
		vai.VAIWebFetch(vai.Tavily),
	}
	if !cfg.VoiceEnabled {
		type talkToUserInput struct {
			Content string `json:"content" desc:"Exact text to speak to the user"`
		}
		tools = append(tools, vai.MakeTool("talk_to_user", "Speak text directly to the user via the voice/output channel.", func(ctx context.Context, input talkToUserInput) (string, error) {
			return "delivered", nil
		}))
	}
	return tools
}

type chatRuntime struct {
	currentModel  string
	history       []vai.Message
	recorder      *pcmRecorder // non-nil while recording mic audio
	pendingPlayer *pcmPlayer   // prewarmed on /st, consumed by next /sp turn only
}

var (
	newPCMPlayerFunc    = newPCMPlayer
	newPCMRecorderFunc  = newPCMRecorder
	stopPCMRecorderFunc = func(r *pcmRecorder) ([]byte, error) {
		if r == nil {
			return nil, nil
		}
		return r.Stop()
	}
	closePCMPlayerFunc = func(p *pcmPlayer) error {
		if p == nil {
			return nil
		}
		return p.Close()
	}
	killPCMPlayerFunc = func(p *pcmPlayer) error {
		if p == nil {
			return nil
		}
		return p.Kill()
	}
)

func closePlayerWithDebug(player *pcmPlayer, label string) {
	if player == nil {
		return
	}
	if err := closePCMPlayerFunc(player); err != nil {
		if strings.TrimSpace(label) == "" {
			label = "player.Close"
		}
		fmt.Fprintf(os.Stderr, "[debug] %s error: %v\n", label, err)
	}
}

func killPlayerWithDebug(player *pcmPlayer, label string) {
	if player == nil {
		return
	}
	if err := killPCMPlayerFunc(player); err != nil {
		if strings.TrimSpace(label) == "" {
			label = "player.Kill"
		}
		fmt.Fprintf(os.Stderr, "[debug] %s error: %v\n", label, err)
	}
}

func closeAndClearPendingPlayer(state *chatRuntime) {
	if state == nil || state.pendingPlayer == nil {
		return
	}
	closePlayerWithDebug(state.pendingPlayer, "pending player cleanup")
	state.pendingPlayer = nil
}

func consumePendingPlayer(state *chatRuntime) *pcmPlayer {
	if state == nil {
		return nil
	}
	player := state.pendingPlayer
	state.pendingPlayer = nil
	return player
}

func prewarmPendingPlayer(state *chatRuntime, errOut io.Writer) {
	if state == nil {
		return
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	closeAndClearPendingPlayer(state)
	player, err := newPCMPlayerFunc()
	if err != nil {
		fmt.Fprintf(errOut, "audio prewarm warning: %v\n", err)
		return
	}
	state.pendingPlayer = player
	if player.cmd != nil && player.cmd.Process != nil {
		fmt.Fprintf(os.Stderr, "[debug] prewarmed player created, pid=%d\n", player.cmd.Process.Pid)
	} else {
		fmt.Fprintln(os.Stderr, "[debug] prewarmed player created")
	}
}

func startRecordingSession(state *chatRuntime, errOut io.Writer) error {
	if state == nil {
		return errors.New("chat state must not be nil")
	}
	prewarmPendingPlayer(state, errOut)
	rec, recErr := newPCMRecorderFunc()
	if recErr != nil {
		closeAndClearPendingPlayer(state)
		return recErr
	}
	state.recorder = rec
	return nil
}

func stopRecordingSession(state *chatRuntime) ([]byte, *pcmPlayer, error) {
	if state == nil {
		return nil, nil, errors.New("chat state must not be nil")
	}
	if state.recorder == nil {
		return nil, nil, errors.New("not recording")
	}
	prewarmed := consumePendingPlayer(state)
	audioWAV, stopErr := stopPCMRecorderFunc(state.recorder)
	state.recorder = nil
	if stopErr != nil || audioWAV == nil {
		closePlayerWithDebug(prewarmed, "prewarmed player cleanup")
		prewarmed = nil
	}
	return audioWAV, prewarmed, stopErr
}

func cleanupAudioState(state *chatRuntime) {
	if state == nil {
		return
	}
	if state.recorder != nil {
		_, _ = stopPCMRecorderFunc(state.recorder)
		state.recorder = nil
	}
	closeAndClearPendingPlayer(state)
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

func shouldUseTurnVoice(cfg chatConfig, audioWAV []byte) bool {
	return cfg.VoiceEnabled && len(audioWAV) > 0
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
	if cfg.VoiceEnabled {
		fmt.Fprintf(out, "Voice output enabled (voice=%s)\n", cfg.VoiceID)
		fmt.Fprintln(out, "Tools enabled: vai_web_search, vai_web_fetch")
	} else {
		fmt.Fprintln(out, "Tools enabled: talk_to_user, vai_web_search, vai_web_fetch")
	}
	if cfg.VoiceEnabled {
		fmt.Fprintln(out, "Type /exit or /quit to stop. /st to record, /sp to stop & send, /live to start live mode, /live off to exit live mode. /model to view model.")
	} else {
		fmt.Fprintln(out, "Type /exit or /quit to stop. Use /model to view and /model:{provider}/{model} to switch.")
	}

	scanner := bufio.NewScanner(in)
	state := chatRuntime{
		currentModel: cfg.Model,
		history:      make([]vai.Message, 0, 32),
	}
	var liveSession *liveModeSession

	for {
		maybeCloseFinishedLiveSession(&state, &liveSession, errOut)
		if state.recorder != nil {
			fmt.Fprint(out, "[rec] > ")
		} else if liveSession != nil {
			fmt.Fprint(out, "[live] > ")
		} else {
			fmt.Fprint(out, "> ")
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read input: %w", err)
			}
			if liveSession != nil {
				_ = liveSession.Close()
				syncHistoryFromLiveSession(&state, liveSession)
				liveSession = nil
			}
			cleanupAudioState(&state)
			fmt.Fprintln(out)
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			if liveSession != nil {
				_ = liveSession.Close()
				syncHistoryFromLiveSession(&state, liveSession)
				liveSession = nil
			}
			cleanupAudioState(&state)
			fmt.Fprintln(out, "bye")
			return nil
		}

		if isLiveModeOnCommand(line) {
			if liveSession != nil {
				fmt.Fprintln(errOut, "live mode already active — type /live off to exit")
				continue
			}
			if state.recorder != nil {
				fmt.Fprintln(errOut, "stop recording first (/sp) before starting live mode")
				continue
			}
			if err := validateModelForLive(state.currentModel, cfg); err != nil {
				fmt.Fprintf(errOut, "live mode error: %v\n", err)
				continue
			}
			session, err := startLiveModeFunc(ctx, cfg, &state, chatTools, out, errOut)
			if err != nil {
				fmt.Fprintf(errOut, "live mode error: %v\n", err)
				continue
			}
			liveSession = session
			fmt.Fprintln(out, "Live mode active. Speak naturally; type /live off to return.")
			continue
		}

		if isLiveModeOffCommand(line) {
			if liveSession == nil {
				fmt.Fprintln(errOut, "live mode is not active")
				continue
			}
			_ = liveSession.Close()
			syncHistoryFromLiveSession(&state, liveSession)
			liveSession = nil
			fmt.Fprintln(out, "Live mode stopped.")
			continue
		}

		if liveSession != nil {
			if handled, err := handleSlashCommand(line, &state, cfg, out, errOut); err != nil {
				return err
			} else if handled {
				continue
			}
			fmt.Fprintln(errOut, "live mode is active — type /live off to return to typed turns")
			continue
		}

		// Recording commands (voice mode only).
		if line == "/st" {
			if !cfg.VoiceEnabled {
				fmt.Fprintln(errOut, "voice not enabled (use -voice flag)")
				continue
			}
			if state.recorder != nil {
				fmt.Fprintln(errOut, "already recording — type /sp to stop")
				continue
			}
			if recErr := startRecordingSession(&state, errOut); recErr != nil {
				fmt.Fprintf(errOut, "mic error: %v\n", recErr)
				continue
			}
			fmt.Fprintln(out, "Recording... type /sp to stop and send")
			continue
		}

		var audioWAV []byte
		var turnPrewarmedPlayer *pcmPlayer
		if line == "/sp" {
			if state.recorder == nil {
				fmt.Fprintln(errOut, "not recording — type /st to start")
				continue
			}
			var spErr error
			audioWAV, turnPrewarmedPlayer, spErr = stopRecordingSession(&state)
			if spErr != nil {
				fmt.Fprintf(errOut, "recording error: %v\n", spErr)
				continue
			}
			if audioWAV == nil {
				fmt.Fprintln(errOut, "no audio recorded")
				continue
			}
			fmt.Fprintf(out, "Sending audio (%d bytes)...\n", len(audioWAV))
		}

		// While recording, ignore non-command input.
		if state.recorder != nil {
			fmt.Fprintln(out, "Recording... type /sp to stop")
			continue
		}

		if handled, err := handleSlashCommand(line, &state, cfg, out, errOut); err != nil {
			return err
		} else if handled {
			continue
		}

		beforeLen := len(state.history)
		var userContent any
		if audioWAV != nil {
			userContent = vai.ContentBlocks(vai.AudioSTT(audioWAV, "audio/wav"))
		} else {
			userContent = vai.Text(line)
		}
		state.history = append(state.history, vai.Message{
			Role:    "user",
			Content: userContent,
		})

		req := &vai.MessageRequest{
			Model:     state.currentModel,
			Messages:  state.history,
			MaxTokens: cfg.MaxTokens,
		}
		turnVoiceEnabled := shouldUseTurnVoice(cfg, audioWAV)
		req.System = composeSystemPrompt(cfg.SystemPrompt, cfg.VoiceEnabled)
		if turnVoiceEnabled {
			req.Voice = vai.VoiceOutput(cfg.VoiceID)
		}

		turnCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		stream, err := client.Messages.RunStream(turnCtx, req, vai.WithTools(chatTools...))
		if err != nil {
			cancel()
			closePlayerWithDebug(turnPrewarmedPlayer, "prewarmed player cleanup")
			turnPrewarmedPlayer = nil
			state.history = state.history[:beforeLen]
			fmt.Fprintf(errOut, "run stream setup error: %s\n", vai.FormatError(err))
			continue
		}

		var player *pcmPlayer
		if turnVoiceEnabled {
			if turnPrewarmedPlayer != nil {
				player = turnPrewarmedPlayer
				turnPrewarmedPlayer = nil
				if player.cmd != nil && player.cmd.Process != nil {
					fmt.Fprintf(os.Stderr, "[debug] using prewarmed player, pid=%d\n", player.cmd.Process.Pid)
				} else {
					fmt.Fprintln(os.Stderr, "[debug] using prewarmed player")
				}
			} else {
				var playerErr error
				player, playerErr = newPCMPlayerFunc()
				if playerErr != nil {
					fmt.Fprintf(errOut, "audio player error: %v\n", playerErr)
				} else {
					fmt.Fprintf(os.Stderr, "[debug] player created, pid=%d\n", player.cmd.Process.Pid)
				}
			}
		}

		printState := streamPrintState{}
		processText, processErr := stream.Process(buildStreamCallbacks(out, &printState, player))

		if player != nil {
			closePlayerWithDebug(player, "player.Close")
			player = nil
		}
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

func buildStreamCallbacks(out io.Writer, printState *streamPrintState, player *pcmPlayer) vai.StreamCallbacks {
	if out == nil {
		out = os.Stdout
	}
	if printState == nil {
		printState = &streamPrintState{}
	}
	initToolStreamState(printState)
	return vai.StreamCallbacks{
		OnAudioChunk: func(data []byte, format string) {
			fmt.Fprintf(os.Stderr, "[debug] OnAudioChunk: %d bytes, format=%q\n", len(data), format)
			if player != nil {
				n, writeErr := player.Write(data)
				if writeErr != nil {
					fmt.Fprintf(os.Stderr, "[debug] player.Write ERROR: %v\n", writeErr)
				} else if n != len(data) {
					fmt.Fprintf(os.Stderr, "[debug] player.Write short: wrote %d of %d bytes\n", n, len(data))
				}
			}
		},
		OnAudioUnavailable: func(reason, message string) {
			writeAudioUnavailableWarning(os.Stderr, &printState.audioUnavailableWarned, reason, message)
		},
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
			writeToolExecutionMarker(out, name, func() {
				closeOpenToolStreamLine(out, printState)
				closeOpenTalkStreamLines(out, printState)
			})
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

func composeSystemPrompt(userPrompt string, voiceEnabled bool) string {
	base := strings.TrimSpace(userPrompt)
	if voiceEnabled {
		// When voice is enabled, the assistant's streamed text IS the speech.
		// No talk_to_user tool, no special instruction needed.
		return base
	}
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
