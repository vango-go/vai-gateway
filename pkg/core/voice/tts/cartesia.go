package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	cartesiaTTSBaseURL    = "https://api.cartesia.ai"
	cartesiaTTSWSBase     = "wss://api.cartesia.ai/tts/websocket"
	cartesiaTTSAPIVersion = "2025-04-16"
	cartesiaTTSDefaultModel = "sonic-3"

	// defaultMaxBufferDelayMs matches the Cartesia docs default.
	// Controls how long Cartesia buffers text before generating,
	// trading latency for quality.
	defaultMaxBufferDelayMs = 3000
)

// CartesiaProvider implements the TTS Provider interface using Cartesia's API.
type CartesiaProvider struct {
	apiKey     string
	httpClient *http.Client
	wsBaseURL  string
}

// NewCartesia creates a new Cartesia TTS provider.
func NewCartesia(apiKey string) *CartesiaProvider {
	return &CartesiaProvider{
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{},
		wsBaseURL:  cartesiaTTSWSBase,
	}
}

// NewCartesiaWithClient creates a new Cartesia TTS provider with a custom HTTP client.
func NewCartesiaWithClient(apiKey string, client *http.Client) *CartesiaProvider {
	if client == nil {
		client = &http.Client{}
	}
	return &CartesiaProvider{
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: client,
		wsBaseURL:  cartesiaTTSWSBase,
	}
}

func (c *CartesiaProvider) Name() string {
	return "cartesia"
}

// ---------------------------------------------------------------------------
// Synthesize — HTTP /tts/bytes (non-streaming, full audio blob)
// ---------------------------------------------------------------------------

// Synthesize converts text to audio via Cartesia's HTTP endpoint.
// Returns complete audio in the requested format (wav/mp3/pcm).
func (c *CartesiaProvider) Synthesize(ctx context.Context, text string, opts SynthesizeOptions) (*Synthesis, error) {
	if c == nil || strings.TrimSpace(c.apiKey) == "" {
		return nil, fmt.Errorf("cartesia api key is required")
	}
	if strings.TrimSpace(opts.Voice) == "" {
		return nil, fmt.Errorf("voice id is required")
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = cartesiaTTSDefaultModel
	}

	outFmt := buildHTTPOutputFormat(opts)
	body := cartesiaHTTPRequest{
		ModelID:    model,
		Transcript: text,
		Voice: cartesiaVoiceSpec{
			Mode: "id",
			ID:   strings.TrimSpace(opts.Voice),
		},
		OutputFormat: outFmt,
	}
	if opts.Language != "" {
		body.Language = opts.Language
	}
	if opts.Speed != 0 || opts.Volume != 0 || opts.Emotion != "" {
		body.GenerationConfig = &cartesiaGenerationConfig{
			Speed:   opts.Speed,
			Volume:  opts.Volume,
			Emotion: opts.Emotion,
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cartesiaTTSBaseURL+"/tts/bytes", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Cartesia-Version", cartesiaTTSAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cartesia tts request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return &Synthesis{Audio: nil, Format: getFormat(opts.Format)}, nil
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cartesia tts error %d: %s", resp.StatusCode, string(errBody))
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &Synthesis{
		Audio:  audio,
		Format: getFormat(opts.Format),
	}, nil
}

// ---------------------------------------------------------------------------
// SynthesizeStream — thin wrapper over NewStreamingContext
// ---------------------------------------------------------------------------

// SynthesizeStream converts text to streaming audio.
// Creates a streaming context, sends the full text, and bridges audio chunks
// into a SynthesisStream.
func (c *CartesiaProvider) SynthesizeStream(ctx context.Context, text string, opts SynthesizeOptions) (*SynthesisStream, error) {
	sc, err := c.NewStreamingContext(ctx, StreamingContextOptions{
		Model:      opts.Model,
		Voice:      opts.Voice,
		Language:   opts.Language,
		Speed:      opts.Speed,
		Volume:     opts.Volume,
		Emotion:    opts.Emotion,
		Format:     opts.Format,
		SampleRate: opts.SampleRate,
	})
	if err != nil {
		return nil, err
	}

	stream := NewSynthesisStream()
	if err := sc.SendText(text, false); err != nil {
		_ = sc.Close()
		return nil, err
	}
	if err := sc.Flush(); err != nil {
		_ = sc.Close()
		return nil, err
	}

	go func() {
		defer stream.FinishSending()
		defer sc.Close()
		for chunk := range sc.Audio() {
			if !stream.Send(chunk) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			stream.SetError(err)
		}
	}()

	return stream, nil
}

// ---------------------------------------------------------------------------
// NewStreamingContext — WebSocket, always raw PCM
// ---------------------------------------------------------------------------

// NewStreamingContext creates a streaming TTS context over Cartesia's WebSocket.
// Text is sent incrementally via SendText(), audio received via Audio().
// Always emits raw/pcm_s16le at the configured sample rate.
func (c *CartesiaProvider) NewStreamingContext(ctx context.Context, opts StreamingContextOptions) (*StreamingContext, error) {
	if c == nil || strings.TrimSpace(c.apiKey) == "" {
		return nil, fmt.Errorf("cartesia api key is required")
	}
	voiceID := strings.TrimSpace(opts.Voice)
	if voiceID == "" {
		return nil, fmt.Errorf("voice id is required")
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = cartesiaTTSDefaultModel
	}

	sampleRate := opts.SampleRate
	if sampleRate == 0 {
		sampleRate = 24000
	}

	maxBufferDelay := opts.MaxBufferDelayMs
	if maxBufferDelay == 0 {
		maxBufferDelay = defaultMaxBufferDelayMs
	}

	// Build WebSocket URL with API key and version as query params.
	wsURL, err := buildCartesiaWSURL(c.wsBaseURL, c.apiKey)
	if err != nil {
		return nil, err
	}

	// Connect WebSocket.
	header := http.Header{}
	header.Set("X-API-Key", c.apiKey)
	header.Set("Cartesia-Version", cartesiaTTSAPIVersion)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("cartesia ws connect: %w", err)
	}

	sc := NewStreamingContext()
	contextID := generateContextID()
	outputFormat := cartesiaOutputFormat{
		Container:  "raw",
		Encoding:   "pcm_s16le",
		SampleRate: sampleRate,
	}

	// Track whether we've sent the final chunk.
	var finalSent atomic.Bool

	// Close coordination.
	ctxDone := make(chan struct{})
	var closeOnce sync.Once
	closeConn := func() error {
		var closeErr error
		closeOnce.Do(func() {
			close(ctxDone)
			closeErr = conn.Close()
		})
		return closeErr
	}

	// SendFunc: write JSON messages with continue semantics.
	sc.SendFunc = func(text string, isFinal bool) error {
		msg := cartesiaStreamingRequest{
			ModelID:    model,
			Transcript: text,
			Voice: cartesiaVoiceSpec{
				Mode: "id",
				ID:   voiceID,
			},
			OutputFormat:     outputFormat,
			ContextID:        contextID,
			Continue:         !isFinal,
			MaxBufferDelayMs: maxBufferDelay,
		}
		if opts.Language != "" {
			msg.Language = opts.Language
		}

		if isFinal {
			finalSent.Store(true)
		}

		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(msg)
	}

	sc.CloseFunc = closeConn

	// Background goroutine: read WebSocket responses, decode audio, push to channel.
	go func() {
		defer sc.FinishAudio()
		defer closeConn()

		for {
			select {
			case <-ctx.Done():
				sc.SetError(ctx.Err())
				return
			case <-ctxDone:
				return
			default:
			}

			_, data, err := conn.ReadMessage()
			if err != nil {
				// Normal close paths — don't treat as error.
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					return
				}
				// If we initiated close, the read will fail — that's expected.
				select {
				case <-ctxDone:
					return
				default:
				}
				sc.SetError(fmt.Errorf("cartesia ws read: %w", err))
				return
			}

			var msg cartesiaWSResponse
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "chunk":
				if msg.Data == "" {
					continue
				}
				audio, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil {
					sc.SetError(fmt.Errorf("decode audio chunk: %w", err))
					return
				}
				if len(audio) > 0 {
					if !sc.PushAudio(audio) {
						return
					}
				}

			case "flush_done":
				// Acknowledgment — continue reading.
				continue

			case "done":
				// Only treat as terminal after we've sent the final chunk.
				// Some Cartesia protocol versions emit intermediate "done"
				// while the context is still open (continue=true).
				if finalSent.Load() {
					return
				}
				continue

			case "error":
				errMsg := msg.Error
				if errMsg == "" {
					errMsg = "unknown cartesia error"
				}
				sc.SetError(fmt.Errorf("cartesia tts error: %s", errMsg))
				return
			}
		}
	}()

	return sc, nil
}

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

type cartesiaHTTPRequest struct {
	ModelID          string                    `json:"model_id"`
	Transcript       string                    `json:"transcript"`
	Voice            cartesiaVoiceSpec         `json:"voice"`
	OutputFormat     cartesiaHTTPOutputFormat  `json:"output_format"`
	Language         string                    `json:"language,omitempty"`
	GenerationConfig *cartesiaGenerationConfig `json:"generation_config,omitempty"`
}

type cartesiaStreamingRequest struct {
	ModelID          string              `json:"model_id"`
	Transcript       string              `json:"transcript"`
	Voice            cartesiaVoiceSpec   `json:"voice"`
	OutputFormat     cartesiaOutputFormat `json:"output_format"`
	ContextID        string              `json:"context_id"`
	Continue         bool                `json:"continue"`
	MaxBufferDelayMs int                 `json:"max_buffer_delay_ms,omitempty"`
	Language         string              `json:"language,omitempty"`
}

type cartesiaVoiceSpec struct {
	Mode string `json:"mode"`
	ID   string `json:"id"`
}

// cartesiaOutputFormat is for WebSocket streaming (always raw PCM).
type cartesiaOutputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

// cartesiaHTTPOutputFormat supports all container types for HTTP synthesis.
type cartesiaHTTPOutputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding,omitempty"`
	SampleRate int    `json:"sample_rate"`
	BitRate    int    `json:"bit_rate,omitempty"`
}

type cartesiaGenerationConfig struct {
	Speed   float64 `json:"speed,omitempty"`
	Volume  float64 `json:"volume,omitempty"`
	Emotion string  `json:"emotion,omitempty"`
}

type cartesiaWSResponse struct {
	Type       string `json:"type"`
	Data       string `json:"data,omitempty"`
	Done       bool   `json:"done"`
	StatusCode int    `json:"status_code"`
	ContextID  string `json:"context_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildCartesiaWSURL(base, apiKey string) (string, error) {
	if strings.TrimSpace(base) == "" {
		base = cartesiaTTSWSBase
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid cartesia ws url: %w", err)
	}
	q := u.Query()
	q.Set("cartesia_version", cartesiaTTSAPIVersion)
	q.Set("api_key", apiKey)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func buildHTTPOutputFormat(opts SynthesizeOptions) cartesiaHTTPOutputFormat {
	sampleRate := opts.SampleRate
	if sampleRate == 0 {
		sampleRate = 24000
	}

	format := strings.TrimSpace(strings.ToLower(opts.Format))
	switch format {
	case "mp3":
		return cartesiaHTTPOutputFormat{
			Container:  "mp3",
			SampleRate: sampleRate,
			BitRate:    128000,
		}
	case "pcm", "raw":
		return cartesiaHTTPOutputFormat{
			Container:  "raw",
			Encoding:   "pcm_s16le",
			SampleRate: sampleRate,
		}
	default:
		return cartesiaHTTPOutputFormat{
			Container:  "wav",
			Encoding:   "pcm_s16le",
			SampleRate: sampleRate,
		}
	}
}

func generateContextID() string {
	// Use a simple timestamp-based ID. Unique enough for a single WebSocket
	// connection where context IDs just need to be distinct per session.
	return fmt.Sprintf("ctx-%d", time.Now().UnixNano())
}
