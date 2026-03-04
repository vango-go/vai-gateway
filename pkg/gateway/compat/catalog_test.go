package compat

import "testing"

func TestProviderKeyHeader_GemDev(t *testing.T) {
	header, ok := ProviderKeyHeader("gem-dev")
	if !ok {
		t.Fatal("expected provider key header for gem-dev")
	}
	if header != "X-Provider-Key-Gemini" {
		t.Fatalf("header=%q, want X-Provider-Key-Gemini", header)
	}
}

func TestProviderKeyHeader_GemVert(t *testing.T) {
	header, ok := ProviderKeyHeader("gem-vert")
	if !ok {
		t.Fatal("expected provider key header for gem-vert")
	}
	if header != "X-Provider-Key-VertexAI" {
		t.Fatalf("header=%q, want X-Provider-Key-VertexAI", header)
	}
}
