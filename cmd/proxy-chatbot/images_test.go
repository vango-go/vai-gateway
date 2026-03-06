package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vai "github.com/vango-go/vai-lite/sdk"
)

func TestRefreshImageStoreFromHistory_DedupesCanonicalToolResultImage(t *testing.T) {
	t.Parallel()

	image := vai.Image([]byte("hello"), "image/png")
	state := &chatRuntime{
		imageStore: newChatImageStore(),
		history: []vai.Message{
			{
				Role: "user",
				Content: []vai.ContentBlock{
					vai.ToolResult("call_1", []vai.ContentBlock{image}),
				},
			},
		},
	}

	newImages := refreshImageStoreFromHistory(state)
	if len(newImages) != 1 {
		t.Fatalf("len(newImages)=%d, want 1", len(newImages))
	}
	if newImages[0].ID != "image_1" {
		t.Fatalf("id=%q, want image_1", newImages[0].ID)
	}

	var out bytes.Buffer
	announceNewImages(&out, newImages)
	if !strings.Contains(out.String(), "[image_1] image/png available. Use /download image_1") {
		t.Fatalf("unexpected announcement: %q", out.String())
	}

	if again := refreshImageStoreFromHistory(state); len(again) != 0 {
		t.Fatalf("len(again)=%d, want 0", len(again))
	}
}

func TestHandleDownloadCommand_SavesBase64Image(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "saved.png")
	state := &chatRuntime{
		imageStore: newChatImageStore(),
		history: []vai.Message{
			{
				Role:    "assistant",
				Content: []vai.ContentBlock{vai.Image([]byte("hello"), "image/png")},
			},
		},
	}

	var out bytes.Buffer
	var errOut bytes.Buffer
	handled, err := handleSlashCommand("/download image_1 "+target, state, chatConfig{}, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("saved data=%q, want %q", string(data), "hello")
	}
	if !strings.Contains(out.String(), "saved [image_1] to") {
		t.Fatalf("unexpected stdout: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", errOut.String())
	}
}

func TestHandleDownloadCommand_UnknownImage(t *testing.T) {
	t.Parallel()

	state := &chatRuntime{imageStore: newChatImageStore()}
	var out bytes.Buffer
	var errOut bytes.Buffer

	handled, err := handleSlashCommand("/download image_9", state, chatConfig{}, &out, &errOut)
	if err != nil {
		t.Fatalf("handleSlashCommand error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if !strings.Contains(errOut.String(), `download error: unknown image id "image_9"`) {
		t.Fatalf("unexpected stderr: %q", errOut.String())
	}
}
