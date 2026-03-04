// Command tts-test is a minimal proof-of-concept for Cartesia TTS streaming.
//
// It sends text to Cartesia's WebSocket TTS endpoint and writes raw PCM audio
// (signed 16-bit LE, mono, 24 kHz) to stdout.
//
// Usage:
//
//	go run ./cmd/tts-test | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
//	go run ./cmd/tts-test -text "Custom text" | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
//	go run ./cmd/tts-test -voice "voice-uuid" | sox -t raw -r 24000 -e signed -b 16 -c 1 - -d
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/dotenv"
	"github.com/vango-go/vai-lite/pkg/core/voice/tts"
)

const (
	defaultVoice = "a167e0f3-df7e-4d52-a9c3-f949145efdab"
	defaultText  = "Hello! This is a test of the Cartesia text to speech system. If you can hear this, the WebSocket integration is working correctly."
)

func main() {
	text := flag.String("text", defaultText, "text to synthesize")
	voice := flag.String("voice", defaultVoice, "Cartesia voice ID")
	model := flag.String("model", "sonic-3", "Cartesia model ID")
	timeout := flag.Duration("timeout", 30*time.Second, "overall timeout")
	flag.Parse()

	// Load .env for CARTESIA_API_KEY.
	_ = dotenv.LoadFile(".env")

	apiKey := strings.TrimSpace(os.Getenv("CARTESIA_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "CARTESIA_API_KEY is required (set in env or .env)")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	provider := tts.NewCartesia(apiKey)

	fmt.Fprintf(os.Stderr, "Connecting to Cartesia TTS (model=%s, voice=%s)...\n", *model, *voice)

	sc, err := provider.NewStreamingContext(ctx, tts.StreamingContextOptions{
		Model:      *model,
		Voice:      *voice,
		Format:     "pcm",
		SampleRate: 24000,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewStreamingContext: %v\n", err)
		os.Exit(1)
	}
	defer sc.Close()

	// Send the full text as a single final chunk.
	fmt.Fprintf(os.Stderr, "Sending text: %q\n", *text)
	if err := sc.SendText(*text, true); err != nil {
		fmt.Fprintf(os.Stderr, "SendText: %v\n", err)
		os.Exit(1)
	}

	// Read audio chunks and write raw PCM to stdout.
	totalBytes := 0
	chunks := 0
	for chunk := range sc.Audio() {
		n, err := os.Stdout.Write(chunk)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			os.Exit(1)
		}
		totalBytes += n
		chunks++
	}

	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "TTS error: %v\n", err)
		os.Exit(1)
	}

	durationSec := float64(totalBytes) / float64(24000*2) // 24kHz, 16-bit = 2 bytes/sample
	fmt.Fprintf(os.Stderr, "Done: %d chunks, %d bytes, ~%.1fs of audio\n", chunks, totalBytes, durationSec)
}
