//go:build integration
// +build integration

package integration_test

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	vai "github.com/vango-go/vai-lite/sdk"
)

const integrationCartesiaVoiceID = "a0e99841-438c-4a64-b679-ae501e7d6091"

func voiceTestModel(t *testing.T) string {
	t.Helper()

	candidates := []struct {
		envKey string
		model  string
	}{
		{envKey: "ANTHROPIC_API_KEY", model: "anthropic/claude-haiku-4-5-20251001"},
		{envKey: "OPENAI_API_KEY", model: "oai-resp/gpt-5-mini"},
		{envKey: "GEMINI_API_KEY", model: "gem-dev/gemini-2.5-flash"},
		{envKey: "GOOGLE_API_KEY", model: "gem-dev/gemini-2.5-flash"},
		{envKey: "VERTEXAI_API_KEY", model: "gem-vert/gemini-3-flash-preview"},
		{envKey: "GROQ_API_KEY", model: "groq/moonshotai/kimi-k2-instruct-0905"},
	}

	for _, c := range candidates {
		if os.Getenv(c.envKey) != "" {
			return c.model
		}
	}

	t.Skip("no LLM API key found for voice integration test")
	return ""
}

func TestVoice_Create_STTRoundTripAndFinalAudio(t *testing.T) {
	requireAllVoiceKeys(t)
	model := voiceTestModel(t)
	ctx := testContext(t, 120*time.Second)

	pipeline := testClient.VoicePipeline()
	if pipeline == nil {
		t.Fatal("expected voice pipeline to be initialized")
	}

	seedCfg := vai.VoiceOutput(integrationCartesiaVoiceID, vai.WithAudioFormat(vai.AudioFormatWAV))
	seedAudio, err := pipeline.SynthesizeResponse(ctx, "Hello from Cartesia integration test.", seedCfg)
	if err != nil {
		t.Fatalf("failed generating seed audio for STT roundtrip: %v", err)
	}
	if len(seedAudio) == 0 {
		t.Fatal("seed audio is empty")
	}

	resp, err := testClient.Messages.Create(ctx, &vai.MessageRequest{
		Model: model,
		Messages: []vai.Message{
			{Role: "user", Content: vai.ContentBlocks(vai.Audio(seedAudio, "audio/wav"))},
		},
		Voice: vai.VoiceFull(
			integrationCartesiaVoiceID,
			vai.WithLanguage("en"),
			vai.WithAudioFormat(vai.AudioFormatWAV),
		),
		MaxTokens: 600,
	})
	if err != nil {
		t.Fatalf("Create voice request failed: %v", err)
	}

	if got := strings.TrimSpace(resp.UserTranscript()); got == "" {
		t.Fatalf("expected non-empty user transcript from STT")
	}

	audio := resp.AudioContent()
	if audio == nil {
		t.Fatalf("expected synthesized audio block in response")
	}
	decoded, err := base64.StdEncoding.DecodeString(audio.Source.Data)
	if err != nil {
		t.Fatalf("failed decoding response audio: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatalf("decoded response audio is empty")
	}
}

func TestVoice_Stream_EmitsAudioEventsAndFinalAudio(t *testing.T) {
	requireAllVoiceKeys(t)
	model := voiceTestModel(t)
	ctx := testContext(t, 120*time.Second)

	stream, err := testClient.Messages.Stream(ctx, &vai.MessageRequest{
		Model: model,
		Messages: []vai.Message{
			{Role: "user", Content: vai.Text("Give me a short two sentence summary of Mars.")},
		},
		Voice:     vai.VoiceOutput(integrationCartesiaVoiceID, vai.WithAudioFormat(vai.AudioFormatWAV)),
		MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("Stream voice request failed: %v", err)
	}
	defer stream.Close()

	audioDone := make(chan struct{})
	var chunkCount int
	var totalAudioBytes int
	go func() {
		defer close(audioDone)
		for chunk := range stream.AudioEvents() {
			chunkCount++
			totalAudioBytes += len(chunk.Data)
		}
	}()

	for range stream.Events() {
	}
	<-audioDone

	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("stream err: %v", err)
	}
	if chunkCount == 0 {
		t.Fatalf("expected audio chunks from Stream.AudioEvents")
	}
	if totalAudioBytes == 0 {
		t.Fatalf("expected non-empty audio chunks")
	}

	resp := stream.Response()
	if resp == nil || resp.AudioContent() == nil {
		t.Fatalf("expected final response audio block")
	}
}

func TestVoice_RunStream_EmitsAudioChunkEventAndFinalAudio(t *testing.T) {
	requireAllVoiceKeys(t)
	model := voiceTestModel(t)
	ctx := testContext(t, 150*time.Second)

	stream, err := testClient.Messages.RunStream(ctx, &vai.MessageRequest{
		Model: model,
		Messages: []vai.Message{
			{Role: "user", Content: vai.Text("Explain what a binary search tree is in two concise paragraphs.")},
		},
		Voice:     vai.VoiceOutput(integrationCartesiaVoiceID, vai.WithAudioFormat(vai.AudioFormatWAV)),
		MaxTokens: 800,
	})
	if err != nil {
		t.Fatalf("RunStream voice request failed: %v", err)
	}
	defer stream.Close()

	audioChunkEvents := 0
	for event := range stream.Events() {
		if _, ok := event.(vai.AudioChunkEvent); ok {
			audioChunkEvents++
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("run stream err: %v", err)
	}
	if audioChunkEvents == 0 {
		t.Fatalf("expected AudioChunkEvent events")
	}

	result := stream.Result()
	if result == nil || result.Response == nil || result.Response.AudioContent() == nil {
		t.Fatalf("expected final run response audio block")
	}
}
