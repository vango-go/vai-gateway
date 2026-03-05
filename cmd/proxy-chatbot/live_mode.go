package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core/types"
	vai "github.com/vango-go/vai-lite/sdk"
)

const (
	liveClientInputSampleRateHz          = 16000
	liveClientDefaultOutputSampleRateHz  = 16000
	liveClientFallbackOutputSampleRateHz = 8000
	liveClientSilenceCommitMS            = 600

	liveDefaultRunMaxTurns      = 8
	liveDefaultRunMaxToolCalls  = 20
	liveDefaultRunTimeoutMS     = 60000
	liveDefaultRunToolTimeoutMS = 30000

	liveToolExecTimeout = 30 * time.Second
)

var liveProviderByokHeaders = map[string]string{
	"anthropic":  "X-Provider-Key-Anthropic",
	"openai":     "X-Provider-Key-OpenAI",
	"oai-resp":   "X-Provider-Key-OpenAI",
	"gem-dev":    "X-Provider-Key-Gemini",
	"gem-vert":   "X-Provider-Key-VertexAI",
	"groq":       "X-Provider-Key-Groq",
	"cerebras":   "X-Provider-Key-Cerebras",
	"openrouter": "X-Provider-Key-OpenRouter",
	"cartesia":   "X-Provider-Key-Cartesia",
	"elevenlabs": "X-Provider-Key-ElevenLabs",
	"tavily":     "X-Provider-Key-Tavily",
	"exa":        "X-Provider-Key-Exa",
	"firecrawl":  "X-Provider-Key-Firecrawl",
}

var (
	startLiveModeFunc      = startLiveMode
	newLivePCMRecorderFunc = newLivePCMRecorder
	newLivePCMPlayerFunc   = newPCMPlayerWithSampleRate
)

type liveModeSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn                       *websocket.Conn
	recorder                   *livePCMRecorder
	player                     *pcmPlayer
	playerRate                 int
	negotiatedOutputSampleRate int
	sendMu                     sync.Mutex
	toolMu                     sync.RWMutex
	tools                      map[string]vai.ToolHandler
	historyMu                  sync.RWMutex
	history                    []vai.Message

	out    io.Writer
	errOut io.Writer

	outputMu               sync.Mutex
	assistantLineOpen      bool
	talkLineOpen           bool
	audioUnavailableWarned bool

	errMu sync.RWMutex
	err   error

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

func startLiveMode(ctx context.Context, cfg chatConfig, state *chatRuntime, tools []vai.ToolWithHandler, out io.Writer, errOut io.Writer) (*liveModeSession, error) {
	if state == nil {
		return nil, errors.New("chat state must not be nil")
	}
	if !cfg.VoiceEnabled {
		return nil, errors.New("live mode requires -voice")
	}
	if strings.TrimSpace(cfg.VoiceID) == "" {
		return nil, errors.New("live mode requires --voice-id")
	}
	if strings.TrimSpace(cfg.ProviderKeys["cartesia"]) == "" {
		return nil, errors.New("live mode requires CARTESIA_API_KEY")
	}

	liveURL, err := liveWebsocketURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	headers := buildLiveWSHeaders(cfg)

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, liveURL, headers)
	if err != nil {
		return nil, fmt.Errorf("connect live websocket: %w", err)
	}

	history := append([]vai.Message(nil), state.history...)
	requestedOutputRate := liveRequestedOutputSampleRate(cfg.LiveOutputRate)
	startFrame, handlerMap := buildLiveStartFrame(cfg, state.currentModel, history, requestedOutputRate, tools)
	if err := conn.WriteJSON(startFrame); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send live start frame: %w", err)
	}

	started, err := readLiveSessionStarted(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	session := &liveModeSession{
		ctx:                        sessCtx,
		cancel:                     cancel,
		conn:                       conn,
		tools:                      handlerMap,
		history:                    append([]vai.Message(nil), history...),
		out:                        out,
		errOut:                     errOut,
		done:                       make(chan struct{}),
		negotiatedOutputSampleRate: started.OutputSampleRateHz,
	}
	if session.out == nil {
		session.out = os.Stdout
	}
	if session.errOut == nil {
		session.errOut = os.Stderr
	}

	if session.negotiatedOutputSampleRate <= 0 {
		session.negotiatedOutputSampleRate = liveClientDefaultOutputSampleRateHz
	}
	if session.negotiatedOutputSampleRate != requestedOutputRate {
		fmt.Fprintf(session.errOut, "live info: negotiated output sample rate %d (requested %d)\n", session.negotiatedOutputSampleRate, requestedOutputRate)
	}
	// Leave player nil here; first audio chunk creates it.
	// This binds playback after mic capture is already active in live mode.
	session.player = nil
	session.playerRate = 0

	recorder, recErr := newLivePCMRecorderFunc(func(chunk []byte) error {
		return session.sendBinary(chunk)
	})
	if recErr != nil {
		session.shutdown(false)
		return nil, fmt.Errorf("start live mic stream: %w", recErr)
	}
	session.recorder = recorder

	session.wg.Add(1)
	go session.readerLoop()
	go func() {
		<-session.ctx.Done()
		session.shutdown(false)
	}()

	return session, nil
}

func buildLiveStartFrame(cfg chatConfig, model string, history []vai.Message, outputSampleRate int, chatTools []vai.ToolWithHandler) (types.LiveStartFrame, map[string]vai.ToolHandler) {
	requestTools, handlerMap, serverTools := partitionLiveTools(chatTools)
	serverToolConfig := buildLiveServerToolConfig(serverTools)

	if outputSampleRate <= 0 {
		outputSampleRate = liveClientDefaultOutputSampleRateHz
	}
	voice := vai.VoiceOutput(cfg.VoiceID, vai.WithAudioFormat(vai.AudioFormatPCM))
	if voice != nil && voice.Output != nil {
		voice.Output.SampleRate = outputSampleRate
	}

	start := types.LiveStartFrame{
		Type: "start",
		RunRequest: types.RunRequest{
			Request: types.MessageRequest{
				Model:      model,
				Messages:   append([]types.Message(nil), history...),
				System:     composeSystemPrompt(cfg.SystemPrompt, false),
				MaxTokens:  cfg.MaxTokens,
				Tools:      requestTools,
				ToolChoice: types.ToolChoiceAuto(),
				STTModel:   "cartesia/ink-whisper",
				TTSModel:   "cartesia/sonic-3",
				Voice:      voice,
			},
			Run: types.RunConfig{
				MaxTurns:      liveDefaultRunMaxTurns,
				MaxToolCalls:  liveDefaultRunMaxToolCalls,
				MaxTokens:     0,
				TimeoutMS:     liveDefaultRunTimeoutMS,
				ParallelTools: true,
				ToolTimeoutMS: liveDefaultRunToolTimeoutMS,
			},
			ServerTools: serverTools,
		},
	}
	if len(serverToolConfig) > 0 {
		start.RunRequest.ServerToolConfig = serverToolConfig
	}
	return start, handlerMap
}

func buildLiveServerToolConfig(serverTools []string) map[string]any {
	if len(serverTools) == 0 {
		return nil
	}
	cfg := make(map[string]any, len(serverTools))
	for _, name := range serverTools {
		switch strings.TrimSpace(name) {
		case "vai_web_search", "vai_web_fetch":
			cfg[name] = map[string]any{"provider": "tavily"}
		}
	}
	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

func partitionLiveTools(chatTools []vai.ToolWithHandler) ([]types.Tool, map[string]vai.ToolHandler, []string) {
	requestTools := make([]types.Tool, 0, len(chatTools))
	handlers := make(map[string]vai.ToolHandler, len(chatTools))
	serverTools := make([]string, 0, 2)
	seenServerTools := make(map[string]struct{}, 2)

	for _, tool := range chatTools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}

		switch name {
		case "vai_web_search", "vai_web_fetch":
			if _, seen := seenServerTools[name]; !seen {
				serverTools = append(serverTools, name)
				seenServerTools[name] = struct{}{}
			}
			continue
		case "talk_to_user":
			continue
		}

		requestTools = append(requestTools, tool.Tool)
		if tool.Handler != nil {
			handlers[name] = tool.Handler
		}
	}

	return requestTools, handlers, serverTools
}

func liveOutputRateCandidates(ratePolicy string) []int {
	switch strings.TrimSpace(ratePolicy) {
	case "16000":
		return []int{16000}
	case "8000":
		return []int{8000}
	case "24000":
		return []int{24000}
	default:
		return []int{liveClientDefaultOutputSampleRateHz, liveClientFallbackOutputSampleRateHz}
	}
}

func liveRequestedOutputSampleRate(ratePolicy string) int {
	candidates := liveOutputRateCandidates(ratePolicy)
	if len(candidates) == 0 {
		return liveClientDefaultOutputSampleRateHz
	}
	return candidates[0]
}

func readLiveSessionStarted(conn *websocket.Conn) (types.LiveSessionStartedEvent, error) {
	var started types.LiveSessionStartedEvent
	if conn == nil {
		return started, errors.New("live websocket is not connected")
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	mt, data, err := conn.ReadMessage()
	if err != nil {
		return started, fmt.Errorf("read live session start ack: %w", err)
	}
	if mt != websocket.TextMessage {
		return started, errors.New("unexpected non-text frame during live startup")
	}

	etype, err := decodeLiveEventType(data)
	if err != nil {
		return started, fmt.Errorf("decode live start ack: %w", err)
	}
	if etype == "error" {
		var ev types.LiveErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return started, fmt.Errorf("live session failed before start")
		}
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "live session failed before start"
		}
		if strings.TrimSpace(ev.Code) != "" {
			return started, fmt.Errorf("%s (%s)", msg, ev.Code)
		}
		return started, errors.New(msg)
	}
	if etype != "session_started" {
		return started, fmt.Errorf("unexpected live startup event %q", etype)
	}

	if err := json.Unmarshal(data, &started); err != nil {
		return started, fmt.Errorf("decode session_started event: %w", err)
	}
	if started.InputFormat != "pcm_s16le" || started.InputSampleRateHz != liveClientInputSampleRateHz {
		return started, fmt.Errorf("unsupported live input audio contract: input=%s sample_rate=%d", started.InputFormat, started.InputSampleRateHz)
	}
	if started.OutputFormat != "pcm_s16le" {
		return started, fmt.Errorf("unsupported live output audio format: %s", started.OutputFormat)
	}
	if started.OutputSampleRateHz <= 0 {
		return started, fmt.Errorf("unsupported live output sample rate: %d", started.OutputSampleRateHz)
	}
	return started, nil
}

func liveWebsocketURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("base-url must not be empty")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base-url: %w", err)
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", errors.New("base-url must include a host")
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported base-url scheme %q", u.Scheme)
	}

	u.RawQuery = ""
	u.Fragment = ""
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "" || basePath == "/" {
		u.Path = "/v1/live"
	} else {
		u.Path = basePath + "/v1/live"
	}
	u.RawPath = ""

	return u.String(), nil
}

func buildLiveWSHeaders(cfg chatConfig) http.Header {
	headers := make(http.Header)
	headers.Set("X-VAI-Version", "1")
	if strings.TrimSpace(cfg.GatewayAPIKey) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.GatewayAPIKey))
	}

	for provider, key := range cfg.ProviderKeys {
		header, ok := liveProviderByokHeaders[strings.ToLower(strings.TrimSpace(provider))]
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "" {
			continue
		}
		headers.Set(header, strings.TrimSpace(key))
	}
	return headers
}

func (s *liveModeSession) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

func (s *liveModeSession) Err() error {
	if s == nil {
		return nil
	}
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

func (s *liveModeSession) setErr(err error) {
	if s == nil || err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *liveModeSession) HistorySnapshot() []vai.Message {
	if s == nil {
		return nil
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	return append([]vai.Message(nil), s.history...)
}

func (s *liveModeSession) updateHistory(history []types.Message) {
	if s == nil {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append([]vai.Message(nil), history...)
}

func (s *liveModeSession) sendJSON(v any) error {
	if s == nil || s.conn == nil {
		return errors.New("live websocket is not connected")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.WriteJSON(v)
}

func (s *liveModeSession) sendBinary(data []byte) error {
	if s == nil || s.conn == nil {
		return errors.New("live websocket is not connected")
	}
	if len(data) == 0 {
		return nil
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (s *liveModeSession) Close() error {
	if s == nil {
		return nil
	}
	s.shutdown(true)
	return s.Err()
}

func (s *liveModeSession) shutdown(wait bool) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.conn != nil {
			_ = s.sendJSON(types.LiveStopFrame{Type: "stop"})
		}
		if s.recorder != nil {
			if err := s.recorder.Close(); err != nil {
				s.setErr(err)
			}
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.player != nil {
			closePlayerWithDebug(s.player, "live player close")
			s.player = nil
		}
		if s.done != nil {
			close(s.done)
		}
	})
	if wait {
		s.wg.Wait()
	}
}

func (s *liveModeSession) readerLoop() {
	defer s.wg.Done()
	defer s.shutdown(false)

	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.setErr(fmt.Errorf("live websocket closed unexpectedly: %w", err))
			}
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		if err := s.handleServerEvent(data); err != nil {
			s.setErr(err)
			return
		}
	}
}

func (s *liveModeSession) handleServerEvent(data []byte) error {
	etype, err := decodeLiveEventType(data)
	if err != nil {
		fmt.Fprintf(s.errOut, "live event parse warning: %v\n", err)
		return nil
	}

	switch etype {
	case "assistant_text_delta":
		var ev types.LiveAssistantTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode assistant_text_delta: %w", err)
		}
		s.writeAssistantDelta(ev.Text)
	case "talk_to_user_text_delta":
		var ev types.LiveTalkToUserTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode talk_to_user_text_delta: %w", err)
		}
		s.writeTalkDelta(ev.Text)
	case "audio_chunk":
		var ev types.LiveAudioChunkEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode audio_chunk: %w", err)
		}
		s.handleAudioChunk(ev)
	case "tool_call":
		var ev types.LiveToolCallEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode tool_call: %w", err)
		}
		go s.handleToolCall(ev)
	case "user_turn_committed":
		s.outputMu.Lock()
		s.closeOpenLinesLocked()
		s.audioUnavailableWarned = false
		s.outputMu.Unlock()
	case "turn_complete":
		var ev types.LiveTurnCompleteEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode turn_complete: %w", err)
		}
		s.outputMu.Lock()
		s.closeOpenLinesLocked()
		s.audioUnavailableWarned = false
		s.outputMu.Unlock()
		s.updateHistory(ev.History)
	case "audio_unavailable":
		var ev types.LiveAudioUnavailableEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode audio_unavailable: %w", err)
		}
		s.writeAudioUnavailable(ev.Reason, ev.Message)
	case "error":
		var ev types.LiveErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode error event: %w", err)
		}
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "live session error"
		}
		if ev.Fatal {
			if strings.TrimSpace(ev.Code) != "" {
				return fmt.Errorf("%s (%s)", msg, ev.Code)
			}
			return errors.New(msg)
		}
		if strings.TrimSpace(ev.Code) != "" {
			fmt.Fprintf(s.errOut, "live warning: %s (%s)\n", msg, ev.Code)
		} else {
			fmt.Fprintf(s.errOut, "live warning: %s\n", msg)
		}
	case "session_started":
		// Already consumed during startup. Ignore repeats.
	default:
		fmt.Fprintf(s.errOut, "live warning: unsupported event type %q\n", etype)
	}

	return nil
}

func (s *liveModeSession) handleAudioChunk(ev types.LiveAudioChunkEvent) {
	if s == nil {
		return
	}

	format := strings.TrimSpace(strings.ToLower(ev.Format))
	if format != "" && format != "pcm_s16le" {
		fmt.Fprintf(s.errOut, "live audio format warning: unsupported format %q\n", ev.Format)
		return
	}

	rate := ev.SampleRateHz
	if rate <= 0 {
		rate = s.negotiatedOutputSampleRate
	}
	if rate <= 0 {
		rate = liveClientDefaultOutputSampleRateHz
	}

	if s.player == nil || s.playerRate != rate {
		if s.player != nil {
			closePlayerWithDebug(s.player, "live player reconfigure")
		}
		player, err := newLivePCMPlayerFunc(rate)
		if err != nil {
			s.player = nil
			s.playerRate = 0
			fmt.Fprintf(s.errOut, "live audio player warning: %v\n", err)
			return
		}
		s.player = player
		s.playerRate = rate
	}

	audioBytes, err := base64.StdEncoding.DecodeString(ev.Audio)
	if err != nil {
		fmt.Fprintf(s.errOut, "live audio decode warning: %v\n", err)
		return
	}
	if len(audioBytes) == 0 {
		return
	}
	if _, err := s.player.Write(audioBytes); err != nil {
		fmt.Fprintf(s.errOut, "live audio playback warning: %v\n", err)
	}
}

func (s *liveModeSession) handleToolCall(ev types.LiveToolCallEvent) {
	result := types.LiveToolResultFrame{
		Type:        "tool_result",
		ExecutionID: ev.ExecutionID,
	}

	name := strings.TrimSpace(ev.Name)
	s.toolMu.RLock()
	handler, ok := s.tools[name]
	s.toolMu.RUnlock()
	if !ok || handler == nil {
		result.IsError = true
		result.Error = fmt.Sprintf("unknown tool %q", name)
		result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: fmt.Sprintf("unknown tool %q", name)}}
		_ = s.sendJSON(result)
		return
	}

	inputJSON, err := json.Marshal(ev.Input)
	if err != nil {
		result.IsError = true
		result.Error = err.Error()
		result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: "invalid tool input"}}
		_ = s.sendJSON(result)
		return
	}

	toolCtx, cancel := context.WithTimeout(s.ctx, liveToolExecTimeout)
	defer cancel()
	output, callErr := handler(toolCtx, inputJSON)
	if callErr != nil {
		result.IsError = true
		result.Error = callErr.Error()
		result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: fmt.Sprintf("Error executing tool: %v", callErr)}}
		_ = s.sendJSON(result)
		return
	}

	result.Content = toolOutputToContentBlocks(output)
	if len(result.Content) == 0 {
		result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: ""}}
	}
	_ = s.sendJSON(result)
}

func toolOutputToContentBlocks(output any) []types.ContentBlock {
	switch v := output.(type) {
	case string:
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: v}}
	case []types.ContentBlock:
		return append([]types.ContentBlock(nil), v...)
	case types.ContentBlock:
		return []types.ContentBlock{v}
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return []types.ContentBlock{types.TextBlock{Type: "text", Text: fmt.Sprintf("%v", v)}}
		}
		return []types.ContentBlock{types.TextBlock{Type: "text", Text: string(encoded)}}
	}
}

func (s *liveModeSession) writeAssistantDelta(text string) {
	if text == "" {
		return
	}
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.talkLineOpen {
		fmt.Fprintln(s.out)
		s.talkLineOpen = false
	}
	if !s.assistantLineOpen {
		fmt.Fprint(s.out, "assistant: ")
		s.assistantLineOpen = true
	}
	fmt.Fprint(s.out, text)
}

func (s *liveModeSession) writeTalkDelta(text string) {
	if text == "" {
		return
	}
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.assistantLineOpen {
		fmt.Fprintln(s.out)
		s.assistantLineOpen = false
	}
	if !s.talkLineOpen {
		fmt.Fprint(s.out, "talk_to_user: ")
		s.talkLineOpen = true
	}
	fmt.Fprint(s.out, text)
}

func (s *liveModeSession) closeOpenLinesLocked() {
	if s.assistantLineOpen {
		fmt.Fprintln(s.out)
		s.assistantLineOpen = false
	}
	if s.talkLineOpen {
		fmt.Fprintln(s.out)
		s.talkLineOpen = false
	}
}

func (s *liveModeSession) writeAudioUnavailable(reason, message string) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.audioUnavailableWarned {
		return
	}
	s.audioUnavailableWarned = true
	reason = strings.TrimSpace(reason)
	message = strings.TrimSpace(message)
	switch {
	case reason != "" && message != "":
		fmt.Fprintf(s.errOut, "audio unavailable (reason=%s): %s\n", reason, message)
	case reason != "":
		fmt.Fprintf(s.errOut, "audio unavailable (reason=%s)\n", reason)
	case message != "":
		fmt.Fprintf(s.errOut, "audio unavailable: %s\n", message)
	default:
		fmt.Fprintln(s.errOut, "audio unavailable")
	}
}

func decodeLiveEventType(data []byte) (string, error) {
	var holder struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &holder); err != nil {
		return "", err
	}
	etype := strings.TrimSpace(holder.Type)
	if etype == "" {
		return "", errors.New("missing event type")
	}
	return etype, nil
}

func syncHistoryFromLiveSession(state *chatRuntime, session *liveModeSession) {
	if state == nil || session == nil {
		return
	}
	state.history = session.HistorySnapshot()
}

func reportClosedLiveSession(session *liveModeSession, errOut io.Writer) {
	if session == nil {
		return
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	if err := session.Err(); err != nil {
		fmt.Fprintf(errOut, "live session ended: %v\n", err)
	}
}

func validateModelForLive(model string, cfg chatConfig) error {
	provider, _, _, err := parseModelRef(model)
	if err != nil {
		return err
	}
	header, ok := liveProviderByokHeaders[provider]
	if !ok {
		return fmt.Errorf("unsupported live provider %q", provider)
	}
	if key := strings.TrimSpace(cfg.ProviderKeys[provider]); key == "" {
		if provider == "oai-resp" && strings.TrimSpace(cfg.ProviderKeys["openai"]) != "" {
			return nil
		}
		envHint, _ := requiredKeySpec(provider)
		if envHint == "" {
			envHint = header
		}
		return fmt.Errorf("missing provider key for %s (set %s)", provider, envHint)
	}
	return nil
}

func isLiveModeOffCommand(line string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(line))
	return trimmed == "/live off"
}

func isLiveModeOnCommand(line string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(line))
	return trimmed == "/live"
}

func maybeCloseFinishedLiveSession(state *chatRuntime, session **liveModeSession, errOut io.Writer) {
	if session == nil || *session == nil {
		return
	}
	select {
	case <-(*session).Done():
		syncHistoryFromLiveSession(state, *session)
		reportClosedLiveSession(*session, errOut)
		*session = nil
	default:
	}
}
