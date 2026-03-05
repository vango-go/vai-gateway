package vai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	liveSDKInputFormat       = "pcm_s16le"
	liveSDKInputSampleRateHz = 16000
	defaultLiveToolTimeout   = 30 * time.Second
)

// LivePlaybackState reports the local playback terminal state for a live turn.
type LivePlaybackState string

const (
	// LivePlaybackStateFinished indicates playback finished naturally.
	LivePlaybackStateFinished LivePlaybackState = "finished"
	// LivePlaybackStateStopped indicates playback was stopped early.
	LivePlaybackStateStopped LivePlaybackState = "stopped"
)

// LiveService provides access to the proxy-only /v1/live websocket API.
type LiveService struct {
	client *Client
}

// LiveConnectRequest configures the initial /v1/live session request.
type LiveConnectRequest struct {
	Request          MessageRequest  `json:"request"`
	Run              ServerRunConfig `json:"run,omitempty"`
	ServerTools      []string        `json:"server_tools,omitempty"`
	ServerToolConfig map[string]any  `json:"server_tool_config,omitempty"`
	Builtins         []string        `json:"builtins,omitempty"`
}

// LiveConnectOptions configures client-side live tool execution.
type LiveConnectOptions struct {
	Tools        []ToolWithHandler
	ToolHandlers map[string]ToolHandler
}

// LiveCallbacks are convenience handlers for processing a live session event stream.
type LiveCallbacks struct {
	StreamCallbacks

	OnSessionStarted    func(LiveSessionStartedEvent)
	OnUserTurnCommitted func(turnID string, audioBytes int)
	OnTurnComplete      func(turnID string, stopReason ServerRunStopReason, history []Message)
	OnTurnCancelled     func(turnID, reason string)
	OnAudioReset        func(turnID, reason string)
}

// LiveSession manages a connected /v1/live websocket session.
type LiveSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn *websocket.Conn

	sendMu sync.Mutex

	historyMu sync.RWMutex
	history   []Message

	errMu sync.RWMutex
	err   error

	toolHandlers map[string]ToolHandler
	toolTimeout  time.Duration

	events    chan LiveEvent
	procTools chan liveProcessToolEvent
	done      chan struct{}
	readDone  chan struct{}

	closeOnce sync.Once
	closed    atomic.Bool
	autoTools sync.WaitGroup
}

type liveProcessToolEvent struct {
	start  *ToolCallStartEvent
	result *ToolResultEvent
}

// Connect opens a live websocket session against the configured proxy gateway.
func (s *LiveService) Connect(ctx context.Context, req *LiveConnectRequest, opts *LiveConnectOptions) (*LiveSession, error) {
	if s == nil || s.client == nil {
		return nil, core.NewInvalidRequestError("client is not initialized")
	}
	if !s.client.isProxyMode() {
		return nil, core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}
	if req == nil {
		return nil, core.NewInvalidRequestError("req must not be nil")
	}

	preparedReq, handlers := prepareLiveConnectRequest(req, opts)

	endpoint, err := liveWebsocketURL(s.client.baseURL)
	if err != nil {
		return nil, err
	}
	headers := s.client.buildLiveHeaders()

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		return nil, &TransportError{Op: "CONNECT", URL: endpoint, Err: err}
	}

	startFrame := types.LiveStartFrame{
		Type:       "start",
		RunRequest: liveConnectRequestToRunRequest(preparedReq),
	}
	if err := conn.WriteJSON(startFrame); err != nil {
		_ = conn.Close()
		return nil, &TransportError{Op: "WRITE", URL: endpoint, Err: fmt.Errorf("send live start frame: %w", err)}
	}

	started, err := readLiveSessionStarted(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	if ctx != nil {
		sessionCtx, cancel = context.WithCancel(ctx)
	}

	session := &LiveSession{
		ctx:          sessionCtx,
		cancel:       cancel,
		conn:         conn,
		history:      cloneMessages(preparedReq.Request.Messages),
		toolHandlers: handlers,
		toolTimeout:  liveToolTimeout(preparedReq.Run),
		events:       make(chan LiveEvent, 64),
		procTools:    make(chan liveProcessToolEvent, 64),
		done:         make(chan struct{}),
		readDone:     make(chan struct{}),
	}

	go func() {
		<-sessionCtx.Done()
		session.shutdown()
	}()
	session.sendEvent(started)
	go session.readerLoop(endpoint)
	go session.awaitTermination()

	return session, nil
}

// Events returns the typed live server event stream.
func (s *LiveSession) Events() <-chan LiveEvent {
	if s == nil {
		ch := make(chan LiveEvent)
		close(ch)
		return ch
	}
	return s.events
}

// Done closes when the live session has fully terminated.
func (s *LiveSession) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Err returns the terminal session error, if any.
func (s *LiveSession) Err() error {
	if s == nil {
		return nil
	}
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

// Close gracefully stops the live session and waits for termination.
func (s *LiveSession) Close() error {
	if s == nil {
		return nil
	}
	s.shutdown()
	<-s.done
	return s.Err()
}

// SendFrame sends a JSON live client frame on the websocket.
func (s *LiveSession) SendFrame(frame LiveClientFrame) error {
	if frame == nil {
		return core.NewInvalidRequestError("frame must not be nil")
	}
	return s.writeJSON(frame)
}

// SendAudio sends a binary PCM audio chunk to the live session.
func (s *LiveSession) SendAudio(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return s.writeBinary(pcm)
}

// SendToolResult sends a tool_result frame for a pending live tool execution.
func (s *LiveSession) SendToolResult(executionID string, content []ContentBlock, isError bool, errPayload any) error {
	return s.SendFrame(LiveToolResultFrame{
		Type:        "tool_result",
		ExecutionID: executionID,
		Content:     cloneContentBlocks(content),
		IsError:     isError,
		Error:       errPayload,
	})
}

// SendPlaybackMark sends a playback progress mark for a live turn.
func (s *LiveSession) SendPlaybackMark(turnID string, playedMS int) error {
	return s.SendFrame(LivePlaybackMarkFrame{
		Type:     "playback_mark",
		TurnID:   strings.TrimSpace(turnID),
		PlayedMS: playedMS,
	})
}

// SendPlaybackState reports local playback completion or stoppage for a live turn.
func (s *LiveSession) SendPlaybackState(turnID string, state LivePlaybackState) error {
	return s.SendFrame(LivePlaybackStateFrame{
		Type:   "playback_state",
		TurnID: strings.TrimSpace(turnID),
		State:  string(state),
	})
}

// HistorySnapshot returns the latest authoritative synced history.
func (s *LiveSession) HistorySnapshot() []Message {
	if s == nil {
		return nil
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	return cloneMessages(s.history)
}

// Process consumes the live event stream and dispatches convenience callbacks.
func (s *LiveSession) Process(callbacks LiveCallbacks) error {
	if s == nil {
		return nil
	}

	var text strings.Builder
	reportedTerminalErr := false
	events := s.Events()
	procTools := s.procTools

	for events != nil || procTools != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch e := event.(type) {
			case LiveSessionStartedEvent:
				if callbacks.OnSessionStarted != nil {
					callbacks.OnSessionStarted(e)
				}
			case LiveAssistantTextDeltaEvent:
				text.WriteString(e.Text)
				if callbacks.OnTextDelta != nil {
					callbacks.OnTextDelta(e.Text)
				}
			case LiveAudioChunkEvent:
				audioBytes, err := base64.StdEncoding.DecodeString(e.Audio)
				if err != nil {
					if callbacks.OnError != nil {
						callbacks.OnError(fmt.Errorf("decode live audio chunk: %w", err))
					}
					continue
				}
				if callbacks.OnAudioChunk != nil {
					callbacks.OnAudioChunk(audioBytes, e.Format)
				}
			case LiveAudioUnavailableEvent:
				if callbacks.OnAudioUnavailable != nil {
					callbacks.OnAudioUnavailable(e.Reason, e.Message)
				}
			case LiveUserTurnCommittedEvent:
				if callbacks.OnUserTurnCommitted != nil {
					callbacks.OnUserTurnCommitted(e.TurnID, e.AudioBytes)
				}
			case LiveTurnCompleteEvent:
				if callbacks.OnTurnComplete != nil {
					callbacks.OnTurnComplete(e.TurnID, e.StopReason, cloneMessages(e.History))
				}
			case LiveTurnCancelledEvent:
				if callbacks.OnTurnCancelled != nil {
					callbacks.OnTurnCancelled(e.TurnID, e.Reason)
				}
			case LiveAudioResetEvent:
				if callbacks.OnAudioReset != nil {
					callbacks.OnAudioReset(e.TurnID, e.Reason)
				}
			case LiveErrorEvent:
				if callbacks.OnError != nil {
					callbacks.OnError(liveErrorToError(e))
				}
				if e.Fatal {
					reportedTerminalErr = true
				}
			}
		case toolEvent, ok := <-procTools:
			if !ok {
				procTools = nil
				continue
			}
			if toolEvent.start != nil && callbacks.OnToolCallStart != nil {
				callbacks.OnToolCallStart(toolEvent.start.ID, toolEvent.start.Name, toolEvent.start.Input)
			}
			if toolEvent.result != nil && callbacks.OnToolResult != nil {
				callbacks.OnToolResult(toolEvent.result.ID, toolEvent.result.Name, cloneContentBlocks(toolEvent.result.Content), toolEvent.result.Error)
			}
		}
	}

	if err := s.Err(); err != nil {
		if callbacks.OnError != nil && !reportedTerminalErr {
			callbacks.OnError(err)
		}
		return err
	}

	_ = text
	return nil
}

func (s *LiveSession) readerLoop(endpoint string) {
	defer close(s.readDone)

	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed.Load() && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.setErr(&TransportError{Op: "READ", URL: endpoint, Err: err})
			}
			return
		}
		if mt != websocket.TextMessage {
			continue
		}

		event, err := types.UnmarshalLiveServerEvent(data)
		if err != nil {
			s.setErr(fmt.Errorf("decode live event: %w", err))
			return
		}

		if toolCall, ok := event.(types.LiveToolCallEvent); ok {
			s.executeToolCall(toolCall)
		}
		if complete, ok := event.(types.LiveTurnCompleteEvent); ok {
			s.updateHistory(complete.History)
		}
		s.sendEvent(event)

		if liveErr, ok := event.(types.LiveErrorEvent); ok && liveErr.Fatal {
			s.setErr(liveErrorToError(liveErr))
			return
		}
	}
}

func (s *LiveSession) awaitTermination() {
	<-s.readDone
	s.autoTools.Wait()
	close(s.procTools)
	close(s.events)
	close(s.done)
}

func (s *LiveSession) executeToolCall(ev types.LiveToolCallEvent) {
	s.autoTools.Add(1)
	go func() {
		defer s.autoTools.Done()

		name := strings.TrimSpace(ev.Name)
		result := LiveToolResultFrame{
			Type:        "tool_result",
			ExecutionID: ev.ExecutionID,
		}

		s.emitToolProcessEvent(liveProcessToolEvent{
			start: &ToolCallStartEvent{
				ID:    ev.ExecutionID,
				Name:  name,
				Input: cloneMap(ev.Input),
			},
		})

		handler, ok := s.toolHandlers[name]
		if !ok || handler == nil {
			err := fmt.Errorf("unknown tool %q", name)
			result.IsError = true
			result.Error = err.Error()
			result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: err.Error()}}
			_ = s.SendFrame(result)
			s.emitToolProcessEvent(liveProcessToolEvent{
				result: &ToolResultEvent{
					ID:      ev.ExecutionID,
					Name:    name,
					Content: cloneContentBlocks(result.Content),
					Error:   err,
				},
			})
			return
		}

		inputJSON, err := json.Marshal(ev.Input)
		if err != nil {
			callErr := errors.New("invalid tool input")
			result.IsError = true
			result.Error = err.Error()
			result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: callErr.Error()}}
			_ = s.SendFrame(result)
			s.emitToolProcessEvent(liveProcessToolEvent{
				result: &ToolResultEvent{
					ID:      ev.ExecutionID,
					Name:    name,
					Content: cloneContentBlocks(result.Content),
					Error:   callErr,
				},
			})
			return
		}

		toolCtx := s.ctx
		cancel := func() {}
		if s.toolTimeout > 0 {
			toolCtx, cancel = context.WithTimeout(s.ctx, s.toolTimeout)
		}
		defer cancel()

		output, callErr := handler(toolCtx, inputJSON)
		if callErr != nil {
			result.IsError = true
			result.Error = callErr.Error()
			result.Content = []types.ContentBlock{
				types.TextBlock{Type: "text", Text: fmt.Sprintf("Error executing tool: %v", callErr)},
			}
			_ = s.SendFrame(result)
			s.emitToolProcessEvent(liveProcessToolEvent{
				result: &ToolResultEvent{
					ID:      ev.ExecutionID,
					Name:    name,
					Content: cloneContentBlocks(result.Content),
					Error:   callErr,
				},
			})
			return
		}

		result.Content = outputToContentBlocks(output)
		if len(result.Content) == 0 {
			result.Content = []types.ContentBlock{types.TextBlock{Type: "text", Text: ""}}
		}
		if err := s.SendFrame(result); err != nil {
			s.setErr(err)
			return
		}
		s.emitToolProcessEvent(liveProcessToolEvent{
			result: &ToolResultEvent{
				ID:      ev.ExecutionID,
				Name:    name,
				Content: cloneContentBlocks(result.Content),
			},
		})
	}()
}

func (s *LiveSession) emitToolProcessEvent(event liveProcessToolEvent) {
	select {
	case s.procTools <- event:
	default:
	}
}

func (s *LiveSession) sendEvent(event LiveEvent) {
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *LiveSession) updateHistory(history []types.Message) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = cloneMessages(history)
}

func (s *LiveSession) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *LiveSession) shutdown() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		if s.conn != nil {
			_ = s.writeJSONFrame(types.LiveStopFrame{Type: "stop"})
			_ = s.conn.Close()
		}
	})
}

func (s *LiveSession) writeJSON(frame any) error {
	if s == nil || s.conn == nil {
		return errors.New("live websocket is not connected")
	}
	if s.closed.Load() {
		return errors.New("live websocket is closed")
	}
	return s.writeJSONFrame(frame)
}

func (s *LiveSession) writeJSONFrame(frame any) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := s.conn.WriteJSON(frame); err != nil {
		return err
	}
	return nil
}

func (s *LiveSession) writeBinary(data []byte) error {
	if s == nil || s.conn == nil {
		return errors.New("live websocket is not connected")
	}
	if s.closed.Load() {
		return errors.New("live websocket is closed")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func prepareLiveConnectRequest(req *LiveConnectRequest, opts *LiveConnectOptions) (*LiveConnectRequest, map[string]ToolHandler) {
	out := cloneLiveConnectRequest(req)
	handlers := make(map[string]ToolHandler)
	extraTools := make([]types.Tool, 0)

	if opts != nil {
		for _, tool := range opts.Tools {
			if tool.Handler != nil && strings.TrimSpace(tool.Name) != "" {
				handlers[strings.TrimSpace(tool.Name)] = tool.Handler
			}
			extraTools = append(extraTools, tool.Tool)
		}
		for name, handler := range opts.ToolHandlers {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" || handler == nil {
				continue
			}
			handlers[trimmed] = handler
		}
	}

	out.Request.Tools = mergeTools(out.Request.Tools, extraTools)
	return out, handlers
}

func cloneLiveConnectRequest(req *LiveConnectRequest) *LiveConnectRequest {
	if req == nil {
		return nil
	}
	out := *req
	out.Request.Messages = cloneMessages(req.Request.Messages)
	out.Request.Tools = append([]types.Tool(nil), req.Request.Tools...)
	out.ServerTools = append([]string(nil), req.ServerTools...)
	out.Builtins = append([]string(nil), req.Builtins...)
	if req.ServerToolConfig != nil {
		out.ServerToolConfig = cloneMap(req.ServerToolConfig)
	}
	if req.Request.Metadata != nil {
		out.Request.Metadata = cloneMap(req.Request.Metadata)
	}
	if req.Request.Extensions != nil {
		out.Request.Extensions = cloneMap(req.Request.Extensions)
	}
	return &out
}

func liveConnectRequestToRunRequest(req *LiveConnectRequest) types.RunRequest {
	if req == nil {
		return types.RunRequest{}
	}
	return types.RunRequest{
		Request:          req.Request,
		Run:              req.Run,
		ServerTools:      append([]string(nil), req.ServerTools...),
		ServerToolConfig: cloneMap(req.ServerToolConfig),
		Builtins:         append([]string(nil), req.Builtins...),
	}
}

func liveToolTimeout(cfg ServerRunConfig) time.Duration {
	if cfg.ToolTimeoutMS > 0 {
		return time.Duration(cfg.ToolTimeoutMS) * time.Millisecond
	}
	return defaultLiveToolTimeout
}

func (c *Client) buildLiveHeaders() http.Header {
	headers := make(http.Header)
	headers.Set(vaiVersionHeader, vaiVersionValue)
	if c.gatewayAPIKey != "" {
		headers.Set("Authorization", "Bearer "+c.gatewayAPIKey)
	}
	for provider, key := range c.providerKeys {
		header, ok := providerByokHeaders[strings.ToLower(strings.TrimSpace(provider))]
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		headers.Set(header, key)
	}
	return headers
}

func liveWebsocketURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", core.NewInvalidRequestError("proxy mode is not enabled (set WithBaseURL)")
	}

	u, err := url.Parse(baseURL)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return "", core.NewInvalidRequestError("invalid gateway base URL")
	}
	if u.User != nil {
		return "", core.NewInvalidRequestError("gateway base URL must not include credentials")
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", core.NewInvalidRequestError("invalid gateway base URL")
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

	event, err := types.UnmarshalLiveServerEvent(data)
	if err != nil {
		return started, fmt.Errorf("decode live start ack: %w", err)
	}

	switch ev := event.(type) {
	case types.LiveErrorEvent:
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "live session failed before start"
		}
		if strings.TrimSpace(ev.Code) != "" {
			return started, fmt.Errorf("%s (%s)", msg, ev.Code)
		}
		return started, errors.New(msg)
	case types.LiveSessionStartedEvent:
		if err := validateLiveSessionStarted(ev); err != nil {
			return started, err
		}
		return ev, nil
	default:
		return started, fmt.Errorf("unexpected live startup event %q", event.LiveServerEventType())
	}
}

func validateLiveSessionStarted(started types.LiveSessionStartedEvent) error {
	if started.InputFormat != liveSDKInputFormat || started.InputSampleRateHz != liveSDKInputSampleRateHz {
		return fmt.Errorf("unsupported live input audio contract: input=%s sample_rate=%d", started.InputFormat, started.InputSampleRateHz)
	}
	if started.OutputFormat != liveSDKInputFormat {
		return fmt.Errorf("unsupported live output audio format: %s", started.OutputFormat)
	}
	if started.OutputSampleRateHz <= 0 {
		return fmt.Errorf("unsupported live output sample rate: %d", started.OutputSampleRateHz)
	}
	return nil
}

func liveErrorToError(ev LiveErrorEvent) error {
	msg := strings.TrimSpace(ev.Message)
	if msg == "" {
		msg = "live session error"
	}
	if strings.TrimSpace(ev.Code) != "" {
		return fmt.Errorf("%s (%s)", msg, ev.Code)
	}
	return errors.New(msg)
}

func cloneMessages(in []types.Message) []types.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.Message, len(in))
	copy(out, in)
	return out
}

func cloneContentBlocks(in []types.ContentBlock) []types.ContentBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.ContentBlock, len(in))
	copy(out, in)
	return out
}

func cloneMap[T any](in map[string]T) map[string]T {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]T, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
