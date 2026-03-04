// Command tts-stream-test proves the streaming TTS pipeline works end-to-end:
// simulated LLM token delivery → SentenceBuffer → Cartesia WebSocket → PCM audio.
//
// This exercises the exact code path used by the SDK (voice.StreamingTTS) in
// production, but with controlled token input instead of a live LLM.
//
// Usage:
//
//	go run ./cmd/tts-stream-test | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
//	go run ./cmd/tts-stream-test -mode stdin | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
//	go run ./cmd/tts-stream-test -delay 20ms | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/dotenv"
	"github.com/vango-go/vai-lite/pkg/core/voice"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

const (
	defaultVoice = "a167e0f3-df7e-4d52-a9c3-f949145efdab"
	defaultText  = `The quick brown fox jumps over the lazy dog. Meanwhile, scientists at the research lab discovered something remarkable! They found that the quantum entanglement experiment yielded unexpected results. Dr. Smith was particularly excited about the implications for future research. "This could change everything," she said. What do you think about that?`
)

func main() {
	mode := flag.String("mode", "simulate", "mode: simulate or stdin")
	voiceID := flag.String("voice", defaultVoice, "Cartesia voice ID")
	model := flag.String("model", "sonic-3", "Cartesia model ID")
	delay := flag.Duration("delay", 40*time.Millisecond, "inter-token delay (simulate mode)")
	text := flag.String("text", defaultText, "paragraph to synthesize (simulate mode)")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Parse()

	_ = dotenv.LoadFile(".env")

	apiKey := strings.TrimSpace(os.Getenv("CARTESIA_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "CARTESIA_API_KEY is required (set in env or .env)")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	provider := tts.NewCartesia(apiKey)
	start := time.Now()

	log := func(format string, args ...any) {
		elapsed := time.Since(start).Seconds()
		fmt.Fprintf(os.Stderr, "  [%6.2fs] "+format+"\n", append([]any{elapsed}, args...)...)
	}

	fmt.Fprintf(os.Stderr, "Connecting to Cartesia TTS (model=%s, voice=%s)...\n", *model, *voiceID)

	sc, err := provider.NewStreamingContext(ctx, tts.StreamingContextOptions{
		Model:      *model,
		Voice:      *voiceID,
		Format:     "pcm",
		SampleRate: 24000,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewStreamingContext: %v\n", err)
		os.Exit(1)
	}

	// Wrap SendFunc to log when sentences are dispatched to Cartesia.
	originalSend := sc.SendFunc
	sc.SendFunc = func(txt string, isFinal bool) error {
		label := "sentence"
		if isFinal {
			label = "flush"
		}
		truncated := txt
		if len(truncated) > 80 {
			truncated = truncated[:77] + "..."
		}
		log("[%s] %q (isFinal=%v)", label, truncated, isFinal)
		return originalSend(txt, isFinal)
	}

	stream := voice.NewStreamingTTS(sc, voice.StreamingTTSOptions{})
	fmt.Fprintln(os.Stderr, "StreamingTTS created, feeding tokens...")

	// Producer: feed tokens in a goroutine.
	producerDone := make(chan error, 1)
	go func() {
		var feedErr error
		switch *mode {
		case "simulate":
			feedErr = feedSimulated(stream, *text, *delay, log)
		case "stdin":
			feedErr = feedStdin(stream, log)
		default:
			feedErr = fmt.Errorf("unknown mode: %s", *mode)
		}
		if feedErr != nil {
			producerDone <- feedErr
			return
		}

		log("flushing remaining text...")
		if err := stream.Flush(); err != nil {
			producerDone <- fmt.Errorf("flush: %w", err)
			return
		}
		producerDone <- nil
	}()

	// Consumer: read audio chunks and write PCM to stdout.
	totalBytes := 0
	chunks := 0
	firstAudioAt := time.Duration(0)

	for chunk := range stream.Audio() {
		if chunks == 0 {
			firstAudioAt = time.Since(start)
			log("[audio] first chunk received (time-to-first-audio: %s)", firstAudioAt.Round(time.Millisecond))
		}
		n, err := os.Stdout.Write(chunk)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			os.Exit(1)
		}
		totalBytes += n
		chunks++
	}

	// Wait for producer to finish (should already be done since audio channel is closed).
	if err := <-producerDone; err != nil {
		fmt.Fprintf(os.Stderr, "producer error: %v\n", err)
		os.Exit(1)
	}

	if err := stream.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close error: %v\n", err)
		os.Exit(1)
	}

	durationSec := float64(totalBytes) / float64(24000*2)
	fmt.Fprintf(os.Stderr, "\nDone: %d chunks, %d bytes, ~%.1fs audio, time-to-first-audio: %s, total wall: %s\n",
		chunks, totalBytes, durationSec,
		firstAudioAt.Round(time.Millisecond),
		time.Since(start).Round(time.Millisecond))
}

// feedSimulated splits text into tokens (words) and feeds them one by one
// with the given delay, simulating LLM token streaming.
func feedSimulated(stream *voice.StreamingTTS, text string, delay time.Duration, log func(string, ...any)) error {
	tokens := tokenize(text)
	log("feeding %d tokens with %s delay...", len(tokens), delay)

	for i, token := range tokens {
		if err := stream.OnTextDelta(token); err != nil {
			return fmt.Errorf("OnTextDelta (token %d): %w", i, err)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	log("all %d tokens fed", len(tokens))
	return nil
}

// feedStdin reads lines from stdin and feeds each as a text delta.
func feedStdin(stream *voice.StreamingTTS, log func(string, ...any)) error {
	log("reading from stdin (type text, press Enter to send, Ctrl+D to finish)...")
	scanner := bufio.NewScanner(os.Stdin)
	lines := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines++
		log("[input] %q", line)
		// Add trailing space so sentence detection works across lines.
		if err := stream.OnTextDelta(line + " "); err != nil {
			return fmt.Errorf("OnTextDelta (line %d): %w", lines, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	log("stdin closed after %d lines", lines)
	return nil
}

// tokenize splits text into LLM-like tokens: roughly one word each,
// preserving trailing spaces so the sentence buffer works correctly.
func tokenize(text string) []string {
	var tokens []string
	current := strings.Builder{}

	for _, r := range text {
		current.WriteRune(r)
		// Split after spaces (each token = word + trailing space).
		if r == ' ' || r == '\n' {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	// Remaining text (last word, no trailing space).
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}
