package main

import (
	"encoding/binary"
	"testing"
)

func TestEncodeWAV_Header(t *testing.T) {
	pcm := make([]byte, 320) // 10ms at 16kHz, 16-bit mono
	for i := range pcm {
		pcm[i] = byte(i % 256)
	}

	wav := encodeWAV(pcm, 16000, 16, 1)

	if len(wav) != 44+len(pcm) {
		t.Fatalf("len(wav) = %d, want %d", len(wav), 44+len(pcm))
	}

	// RIFF header
	if string(wav[0:4]) != "RIFF" {
		t.Errorf("chunk ID = %q, want RIFF", wav[0:4])
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+len(pcm)) {
		t.Errorf("chunk size = %d, want %d", got, 36+len(pcm))
	}
	if string(wav[8:12]) != "WAVE" {
		t.Errorf("format = %q, want WAVE", wav[8:12])
	}

	// fmt chunk
	if string(wav[12:16]) != "fmt " {
		t.Errorf("subchunk1 ID = %q, want 'fmt '", wav[12:16])
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := binary.LittleEndian.Uint32(wav[28:32]); got != 32000 {
		t.Errorf("byte rate = %d, want 32000", got)
	}
	if got := binary.LittleEndian.Uint16(wav[32:34]); got != 2 {
		t.Errorf("block align = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}

	// data chunk
	if string(wav[36:40]) != "data" {
		t.Errorf("subchunk2 ID = %q, want 'data'", wav[36:40])
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data size = %d, want %d", got, len(pcm))
	}

	// Verify PCM data is preserved
	for i := range pcm {
		if wav[44+i] != pcm[i] {
			t.Fatalf("data mismatch at byte %d: got %d, want %d", i, wav[44+i], pcm[i])
		}
	}
}
