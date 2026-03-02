package mw

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/gateway/auth"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
)

type ctxKeyRequestID struct{}

func RequestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKeyRequestID{}).(string)
	return id, ok && id != ""
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = "req_" + randHex(10)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func Auth(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID, _ := RequestIDFrom(r.Context())

		switch cfg.AuthMode {
		case config.AuthModeDisabled:
			next.ServeHTTP(w, r)
			return
		case config.AuthModeOptional, config.AuthModeRequired:
		default:
			writeJSONError(w, http.StatusInternalServerError, &core.Error{
				Type:      core.ErrAPI,
				Message:   "invalid auth_mode",
				RequestID: reqID,
			})
			return
		}

		token, ok := auth.ParseBearer(r)
		if !ok {
			if cfg.AuthMode == config.AuthModeRequired {
				writeJSONError(w, http.StatusUnauthorized, &core.Error{
					Type:      core.ErrAuthentication,
					Message:   "missing bearer token",
					Param:     "Authorization",
					RequestID: reqID,
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := cfg.APIKeys[token]; !ok {
			writeJSONError(w, http.StatusUnauthorized, &core.Error{
				Type:      core.ErrAuthentication,
				Message:   "invalid api key",
				RequestID: reqID,
			})
			return
		}
		p := &auth.Principal{APIKey: token}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				reqID, _ := RequestIDFrom(r.Context())
				panicText := fmt.Sprint(v)
				stack := string(debug.Stack())
				if logger != nil {
					logger.Error("panic", "panic", panicText, "request_id", reqID, "stack", stack)
				}
				writeJSONError(w, http.StatusInternalServerError, &core.Error{
					Type:      core.ErrAPI,
					Message:   "internal error",
					RequestID: reqID,
					Code:      "panic",
					ProviderError: map[string]any{
						"panic": panicText,
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

type loggingResponseWriterFlusher struct {
	*loggingResponseWriter
	flusher http.Flusher
}

func (w *loggingResponseWriterFlusher) Flush() {
	w.flusher.Flush()
}

type loggingResponseWriterHijacker struct {
	*loggingResponseWriter
	hijacker http.Hijacker
}

func (w *loggingResponseWriterHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijacker.Hijack()
}

type loggingResponseWriterFlusherHijacker struct {
	*loggingResponseWriter
	flusher  http.Flusher
	hijacker http.Hijacker
}

func (w *loggingResponseWriterFlusherHijacker) Flush() {
	w.flusher.Flush()
}

func (w *loggingResponseWriterFlusherHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijacker.Hijack()
}

func wrapLoggingResponseWriter(w http.ResponseWriter) (http.ResponseWriter, *loggingResponseWriter) {
	base := &loggingResponseWriter{
		ResponseWriter: w,
		status:         http.StatusOK,
	}

	flusher, hasFlusher := w.(http.Flusher)
	hijacker, hasHijacker := w.(http.Hijacker)

	switch {
	case hasFlusher && hasHijacker:
		return &loggingResponseWriterFlusherHijacker{
			loggingResponseWriter: base,
			flusher:               flusher,
			hijacker:              hijacker,
		}, base
	case hasFlusher:
		return &loggingResponseWriterFlusher{
			loggingResponseWriter: base,
			flusher:               flusher,
		}, base
	case hasHijacker:
		return &loggingResponseWriterHijacker{
			loggingResponseWriter: base,
			hijacker:              hijacker,
		}, base
	default:
		return base, base
	}
}

func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped, state := wrapLoggingResponseWriter(w)
		next.ServeHTTP(wrapped, r)
		if logger == nil {
			return
		}
		reqID, _ := RequestIDFrom(r.Context())
		logger.Info("request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", state.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func randHex(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should not fail in practice; fall back to time-based entropy.
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b)
}

type errorEnvelope struct {
	Error *core.Error `json:"error"`
}

func writeJSONError(w http.ResponseWriter, status int, err *core.Error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: err})
}
