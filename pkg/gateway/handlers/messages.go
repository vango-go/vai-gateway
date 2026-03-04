package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/stt"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
	"github.com/vango-go/vai-lite/pkg/gateway/compat"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
	"github.com/vango-go/vai-lite/pkg/gateway/limits"
	"github.com/vango-go/vai-lite/pkg/gateway/mw"
	"github.com/vango-go/vai-lite/pkg/gateway/principal"
	"github.com/vango-go/vai-lite/pkg/gateway/ratelimit"
	"github.com/vango-go/vai-lite/pkg/gateway/sse"
)

type ProviderFactory interface {
	New(providerName, apiKey string) (core.Provider, error)
}

type MessagesHandler struct {
	Config     config.Config
	Upstreams  ProviderFactory
	HTTPClient *http.Client
	Logger     *slog.Logger
	Limiter    *ratelimit.Limiter
	Lifecycle  *lifecycle.Lifecycle
}

func (h MessagesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		reqID, _ := mw.RequestIDFrom(r.Context())
		h.writeErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "method not allowed",
			Code:      "method_not_allowed",
			RequestID: reqID,
		}, http.StatusMethodNotAllowed)
		return
	}

	reqID, _ := mw.RequestIDFrom(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, h.Config.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeErrorJSON(w, reqID, core.NewInvalidRequestError("failed to read request body"), http.StatusBadRequest)
		return
	}

	req, err := types.UnmarshalMessageRequestStrict(body)
	if err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}

	if err := limits.ValidateMessageRequest(req, h.Config); err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}

	if len(h.Config.ModelAllowlist) > 0 {
		if _, ok := h.Config.ModelAllowlist[req.Model]; !ok {
			h.writeErrorJSON(w, reqID, &core.Error{
				Type:      core.ErrPermission,
				Message:   "model is not allowlisted",
				Param:     "model",
				RequestID: reqID,
			}, http.StatusForbidden)
			return
		}
	}

	providerName, modelName, err := core.ParseModelString(req.Model)
	if err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}
	if providerName == "gemini" || providerName == "gemini-oauth" {
		h.writeErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrInvalidRequest,
			Message:   "provider has been removed; use gem-vert/<model> or gem-dev/<model>",
			Param:     "model",
			RequestID: reqID,
		}, http.StatusBadRequest)
		return
	}

	upstreamKeyHeader, ok := compat.ProviderKeyHeader(providerName)
	if !ok {
		h.writeErrorJSON(w, reqID, core.NewInvalidRequestErrorWithParam("unsupported provider", "model"), http.StatusBadRequest)
		return
	}
	upstreamKey := strings.TrimSpace(r.Header.Get(upstreamKeyHeader))
	if upstreamKey == "" {
		h.writeErrorJSON(w, reqID, &core.Error{
			Type:      core.ErrAuthentication,
			Message:   "missing upstream provider api key header",
			Param:     upstreamKeyHeader,
			Code:      "provider_key_missing",
			RequestID: reqID,
		}, http.StatusUnauthorized)
		return
	}

	if compatIssues := compat.ValidateMessageRequest(req, providerName, req.Model); len(compatIssues) > 0 {
		h.writeErrorJSON(w, reqID, &core.Error{
			Type:         core.ErrInvalidRequest,
			Message:      fmt.Sprintf("Request is incompatible with provider %s and model %s", providerName, modelName),
			CompatIssues: compatIssues,
			RequestID:    reqID,
		}, http.StatusBadRequest)
		return
	}

	provider, err := h.Upstreams.New(providerName, upstreamKey)
	if err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}

	workingReq := *req
	workingReq.Model = modelName

	// Request-scoped timeout. Streaming requests use the SSE max duration.
	ctx := r.Context()
	if req.Stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Config.SSEMaxStreamDuration)
		defer cancel()
	} else if h.Config.HandlerTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Config.HandlerTimeout)
		defer cancel()
	}

	var voicePipeline *voice.Pipeline
	var userTranscript string
	needsSTT := types.RequestHasAudioSTT(req)
	needsTTS := req.Voice != nil && req.Voice.Output != nil
	if h.Logger != nil {
		h.Logger.Info("voice check", "needsSTT", needsSTT, "needsTTS", needsTTS, "hasVoice", req.Voice != nil, "hasCartesiaHeader", r.Header.Get("X-Provider-Key-Cartesia") != "")
	}
	if needsSTT || needsTTS {
		cartesiaKey := strings.TrimSpace(r.Header.Get("X-Provider-Key-Cartesia"))
		if cartesiaKey == "" {
			h.writeErrorJSON(w, reqID, &core.Error{
				Type:      core.ErrAuthentication,
				Message:   "missing voice provider api key header",
				Param:     "X-Provider-Key-Cartesia",
				RequestID: reqID,
			}, http.StatusUnauthorized)
			return
		}

		voiceHTTPClient := h.HTTPClient
		if voiceHTTPClient == nil {
			voiceHTTPClient = &http.Client{}
		}
		voicePipeline = voice.NewPipelineWithProviders(
			stt.NewCartesiaWithClient(cartesiaKey, voiceHTTPClient),
			tts.NewCartesiaWithClient(cartesiaKey, voiceHTTPClient),
		)

		if needsSTT {
			processedReq, transcript, err := voice.PreprocessMessageRequestAudioSTT(ctx, voicePipeline, &workingReq)
			if err != nil {
				h.writeErr(w, reqID, err, false)
				return
			}
			workingReq = *processedReq
			userTranscript = transcript
		}
	}

	if req.Stream {
		if h.Lifecycle != nil && h.Lifecycle.IsDraining() {
			h.writeErrorJSON(w, reqID, &core.Error{
				Type:      core.ErrOverloaded,
				Message:   "gateway is draining",
				Code:      "draining",
				RequestID: reqID,
			}, 529)
			return
		}

		p := principal.Resolve(r, h.Config)

		if h.Limiter != nil && h.Config.LimitMaxConcurrentStreams > 0 {
			dec := h.Limiter.AcquireStream(p.Key, time.Now())
			if !dec.Allowed {
				if dec.RetryAfter > 0 {
					w.Header().Set("Retry-After", itoa(dec.RetryAfter))
				}
				h.writeErrorJSON(w, reqID, core.NewRateLimitError("too many concurrent streams", dec.RetryAfter), http.StatusTooManyRequests)
				return
			}
			if dec.Permit != nil {
				defer dec.Permit.Release()
			}
		}

		h.serveStream(w, r, ctx, reqID, provider, &workingReq, voicePipeline, userTranscript)
		return
	}

	resp, err := provider.CreateMessage(ctx, &workingReq)
	if err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}

	if userTranscript != "" {
		if resp.Metadata == nil {
			resp.Metadata = make(map[string]any)
		}
		resp.Metadata["user_transcript"] = userTranscript
	}

	if voicePipeline != nil && req.Voice != nil && req.Voice.Output != nil {
		if err := voice.AppendVoiceOutputToMessageResponse(ctx, voicePipeline, req.Voice, req.TTSModel, resp); err != nil {
			h.writeErr(w, reqID, err, false)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Model", resp.Model)
	w.Header().Set("X-Input-Tokens", itoa(resp.Usage.InputTokens))
	w.Header().Set("X-Output-Tokens", itoa(resp.Usage.OutputTokens))
	w.Header().Set("X-Total-Tokens", itoa(resp.Usage.TotalTokens))
	_ = json.NewEncoder(w).Encode(resp)
}

func (h MessagesHandler) serveStream(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	reqID string,
	provider core.Provider,
	req *types.MessageRequest,
	voicePipeline *voice.Pipeline,
	userTranscript string,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sw, err := sse.New(w)
	if err != nil {
		h.writeErr(w, reqID, err, false)
		return
	}

	// Keepalive pings when the upstream is quiet.
	// Track last *non-ping* activity so pings don't suppress themselves.
	var lastNonPingActivity atomic.Int64
	lastNonPingActivity.Store(time.Now().UnixNano())
	send := func(event string, data any) error {
		if err := sw.Send(event, data); err != nil {
			return err
		}
		lastNonPingActivity.Store(time.Now().UnixNano())
		return nil
	}

	pingInterval := h.Config.SSEPingInterval
	if pingInterval > 0 {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-ticker.C:
					last := time.Unix(0, lastNonPingActivity.Load())
					if t.Sub(last) < pingInterval {
						continue
					}
					_ = sw.Send("ping", types.PingEvent{Type: "ping"})
				}
			}
		}()
	}

	reqCopy := *req
	reqCopy.Stream = true
	stream, err := provider.StreamMessage(ctx, &reqCopy)
	if err != nil {
		h.writeErr(w, reqID, err, true)
		return
	}
	defer func() { _ = stream.Close() }()

	// Voice output side-channel: feed text deltas into streaming TTS and emit audio_chunk SSE events.
	var ttsCtx *tts.StreamingContext
	var ttsStream *voice.StreamingTTS
	audioDone := make(chan struct{})
	audioBytesCh := make(chan []byte, 100)
	audioStopped := false
	audioFailed := false
	ttsFedText := false
	sampleRateHz := 24000
	if req.Voice != nil && req.Voice.Output != nil && req.Voice.Output.SampleRate > 0 {
		sampleRateHz = req.Voice.Output.SampleRate
	}

	if voicePipeline != nil && req.Voice != nil && req.Voice.Output != nil {
		resolvedTTSModel, resolveErr := voice.ResolveTTSModel(req.TTSModel)
		if resolveErr != nil {
			h.writeErr(w, reqID, resolveErr, true)
			return
		}
		if h.Logger != nil {
			h.Logger.Info("tts setup", "model", resolvedTTSModel.Model, "voice", req.Voice.Output.Voice)
		}
		var err error
		ttsCtx, err = voicePipeline.NewStreamingTTSContext(ctx, req.Voice, resolvedTTSModel.Model)
		if err != nil {
			if h.Logger != nil {
				h.Logger.Error("tts context failed", "error", err)
			}
			h.writeErr(w, reqID, err, true)
			return
		}
		if h.Logger != nil {
			h.Logger.Info("tts context created")
		}
		ttsStream = voice.NewStreamingTTS(ttsCtx, voice.StreamingTTSOptions{BufferAudio: false})

		go func() {
			defer close(audioDone)
			defer close(audioBytesCh)

			chunks := 0
			audioStream := ttsStream.Audio()
			for {
				select {
				case <-ctx.Done():
					return
				case chunk, ok := <-audioStream:
					if !ok {
						return
					}
					if len(chunk) == 0 {
						continue
					}
					chunks++
					if chunks == 1 && h.Logger != nil {
						h.Logger.Info("tts first audio chunk", "bytes", len(chunk))
					}
					select {
					case audioBytesCh <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	} else {
		close(audioBytesCh)
		close(audioDone)
	}

	sendAudioChunk := func(chunk []byte, isFinal bool) error {
		ev := types.AudioChunkEvent{
			Type:         "audio_chunk",
			Format:       "pcm_s16le",
			Audio:        base64.StdEncoding.EncodeToString(chunk),
			SampleRateHz: sampleRateHz,
			IsFinal:      isFinal,
		}
		return send(ev.EventType(), ev)
	}

	type nextResult struct {
		ev  types.StreamEvent
		err error
	}

	idleTimeout := h.Config.StreamIdleTimeout
	var idleTimer *time.Timer
	var idleTimerCh <-chan time.Time
	if idleTimeout > 0 {
		idleTimer = time.NewTimer(idleTimeout)
		idleTimerCh = idleTimer.C
		defer idleTimer.Stop()
	}
	resetIdleTimer := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleTimeout)
	}

	nextCh := make(chan nextResult, 1)
	go func() {
		defer close(nextCh)
		for {
			ev, err := stream.Next()
			select {
			case nextCh <- nextResult{ev: ev, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Audio chunk lookahead buffer (so we can mark the final non-empty chunk).
	var pendingAudio []byte
	flushPendingAudio := func(final bool) error {
		if audioStopped || len(pendingAudio) == 0 {
			pendingAudio = nil
			return nil
		}
		chunk := pendingAudio
		pendingAudio = nil
		return sendAudioChunk(chunk, final)
	}

	handleAudioChunk := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		if audioStopped {
			return nil
		}
		// Emit the previous chunk (non-final) and buffer the new one.
		if err := flushPendingAudio(false); err != nil {
			return err
		}
		pendingAudio = chunk
		return nil
	}

	emitTTSUnavailable := func(err error) {
		if err == nil || audioFailed {
			return
		}
		audioFailed = true
		audioStopped = true
		pendingAudio = nil
		_ = send("audio_unavailable", types.AudioUnavailableEvent{
			Type:    "audio_unavailable",
			Reason:  "tts_failed",
			Message: "TTS synthesis failed: " + err.Error(),
		})
		if ttsCtx != nil {
			_ = ttsCtx.Close()
		}
		ttsStream = nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = stream.Close()
			if ttsCtx != nil {
				_ = ttsCtx.Close()
			}
			// If this was a max-duration timeout (not client disconnect), emit a terminal SSE error event.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
				h.writeErr(w, reqID, ctx.Err(), true)
			}
			return

		case <-idleTimerCh:
			_ = stream.Close()
			if ttsCtx != nil {
				_ = ttsCtx.Close()
			}
			h.writeErr(w, reqID, core.NewAPIError("upstream stream idle timeout"), true)
			return

		case chunk, ok := <-audioBytesCh:
			if !ok {
				audioBytesCh = nil
				continue
			}
			if err := handleAudioChunk(chunk); err != nil {
				_ = stream.Close()
				if ttsCtx != nil {
					_ = ttsCtx.Close()
				}
				return
			}

		case res, ok := <-nextCh:
			if !ok {
				goto done
			}
			resetIdleTimer()
			if res.ev != nil {
				if userTranscript != "" {
					switch ev := res.ev.(type) {
					case types.MessageStartEvent:
						if ev.Message.Metadata == nil {
							ev.Message.Metadata = make(map[string]any)
						}
						ev.Message.Metadata["user_transcript"] = userTranscript
						res.ev = ev
					case *types.MessageStartEvent:
						if ev.Message.Metadata == nil {
							ev.Message.Metadata = make(map[string]any)
						}
						ev.Message.Metadata["user_transcript"] = userTranscript
					}
				}

				// Feed text deltas into TTS.
				if ttsStream != nil {
					if cbs, ok := res.ev.(types.ContentBlockStartEvent); ok {
						if tb, ok := cbs.ContentBlock.(types.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
							ttsFedText = true
							if sendErr := ttsStream.OnTextDelta(tb.Text); sendErr != nil {
								emitTTSUnavailable(sendErr)
							}
						}
					}
					if cbd, ok := res.ev.(types.ContentBlockDeltaEvent); ok {
						if td, ok := cbd.Delta.(types.TextDelta); ok {
							ttsFedText = true
							if sendErr := ttsStream.OnTextDelta(td.Text); sendErr != nil {
								emitTTSUnavailable(sendErr)
							}
						}
					}
				}

				if sendErr := send(res.ev.EventType(), res.ev); sendErr != nil {
					_ = stream.Close()
					if ttsCtx != nil {
						_ = ttsCtx.Close()
					}
					return
				}
			}

			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					goto done
				}
				h.writeErr(w, reqID, res.err, true)
				if ttsCtx != nil {
					_ = ttsCtx.Close()
				}
				return
			}
		}
	}

done:
	if ttsStream != nil && !audioStopped && ttsFedText {
		if h.Logger != nil {
			h.Logger.Info("tts flushing")
		}
		flushErrCh := make(chan error, 1)
		go func(stream *voice.StreamingTTS) {
			flushErrCh <- stream.Flush()
		}(ttsStream)
		flushDone := false
		for !flushDone || audioBytesCh != nil {
			select {
			case flushErr := <-flushErrCh:
				flushDone = true
				if flushErr != nil {
					emitTTSUnavailable(flushErr)
				}
			case chunk, ok := <-audioBytesCh:
				if !ok {
					audioBytesCh = nil
					continue
				}
				if err := handleAudioChunk(chunk); err != nil {
					return
				}
			case <-ctx.Done():
				if ttsCtx != nil {
					_ = ttsCtx.Close()
				}
				return
			}
		}
	} else if ttsStream != nil {
		// No text was fed (e.g. tool-use only turn) or audio already failed.
		_ = ttsStream.Close()
		ttsStream = nil
	} else if ttsCtx != nil {
		_ = ttsCtx.Close()
	}

	<-audioDone

	if ttsStream != nil && !audioStopped {
		if err := ttsStream.Err(); err != nil {
			emitTTSUnavailable(err)
		}
	}
	if ttsStream != nil {
		_ = ttsStream.Close()
		ttsStream = nil
	}

	// Drain any remaining audio bytes and emit the final chunk marker if applicable.
	if audioBytesCh != nil {
		if audioStopped {
			for range audioBytesCh {
			}
		} else {
			for chunk := range audioBytesCh {
				if err := handleAudioChunk(chunk); err != nil {
					return
				}
			}
		}
	}
	if !audioStopped {
		_ = flushPendingAudio(true)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func (h MessagesHandler) writeErr(w http.ResponseWriter, reqID string, err error, isStream bool) {
	coreErr, status := coreErrorFrom(err, reqID)

	if isStream {
		// If headers already started, best-effort send SSE error event.
		sw, sseErr := sse.New(w)
		if sseErr == nil {
			_ = sw.Send("error", types.ErrorEvent{
				Type:  "error",
				Error: toTypesError(coreErr),
			})
		}
		_ = status
		return
	}

	writeCoreErrorJSON(w, reqID, coreErr, status)
}

func (h MessagesHandler) writeErrorJSON(w http.ResponseWriter, reqID string, coreErr *core.Error, status int) {
	writeCoreErrorJSON(w, reqID, coreErr, status)
}
