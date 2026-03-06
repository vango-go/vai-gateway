package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

type chatImageAsset struct {
	ID        string
	Block     types.ImageBlock
	MediaType string
}

type chatImageStore struct {
	nextIndex       int
	byID            map[string]chatImageAsset
	idByFingerprint map[string]string
	announcedByID   map[string]bool
}

func newChatImageStore() *chatImageStore {
	return &chatImageStore{
		byID:            make(map[string]chatImageAsset),
		idByFingerprint: make(map[string]string),
		announcedByID:   make(map[string]bool),
	}
}

func refreshImageStoreFromHistory(state *chatRuntime) []chatImageAsset {
	if state == nil {
		return nil
	}
	if state.imageStore == nil {
		state.imageStore = newChatImageStore()
	}
	newAssets := make([]chatImageAsset, 0, 2)
	for i := range state.history {
		registerMessageImages(state.imageStore, state.history[i], &newAssets)
	}
	return newAssets
}

func announceNewImages(out io.Writer, images []chatImageAsset) {
	if out == nil || len(images) == 0 {
		return
	}
	for _, img := range images {
		label := "image"
		if strings.TrimSpace(img.MediaType) != "" {
			label = strings.TrimSpace(img.MediaType)
		}
		fmt.Fprintf(out, "[%s] %s available. Use /download %s\n", img.ID, label, img.ID)
	}
}

func handleDownloadCommand(line string, state *chatRuntime, out io.Writer, errOut io.Writer) error {
	if state == nil {
		return errors.New("chat state must not be nil")
	}
	if out == nil {
		out = os.Stdout
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || len(fields) > 3 {
		return errors.New("usage: /download {image_id} [path]")
	}
	if state.imageStore == nil {
		state.imageStore = newChatImageStore()
	}
	refreshImageStoreFromHistory(state)
	asset, ok := state.imageStore.byID[fields[1]]
	if !ok {
		return fmt.Errorf("unknown image id %q", fields[1])
	}
	targetPath := ""
	if len(fields) == 3 {
		targetPath = fields[2]
	}
	savedPath, err := downloadImageAsset(context.Background(), asset, targetPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "saved [%s] to %s\n", asset.ID, savedPath)
	return nil
}

func registerMessageImages(store *chatImageStore, message types.Message, newAssets *[]chatImageAsset) {
	registerContentImages(store, message.ContentBlocks(), newAssets)
}

func registerContentImages(store *chatImageStore, blocks []types.ContentBlock, newAssets *[]chatImageAsset) {
	for i := range blocks {
		switch b := blocks[i].(type) {
		case types.ImageBlock:
			maybeRegisterImage(store, b, newAssets)
		case *types.ImageBlock:
			if b != nil {
				maybeRegisterImage(store, *b, newAssets)
			}
		case types.ToolResultBlock:
			registerContentImages(store, b.Content, newAssets)
		case *types.ToolResultBlock:
			if b != nil {
				registerContentImages(store, b.Content, newAssets)
			}
		}
	}
}

func maybeRegisterImage(store *chatImageStore, block types.ImageBlock, newAssets *[]chatImageAsset) {
	fp := chatImageFingerprint(block)
	if existingID, ok := store.idByFingerprint[fp]; ok {
		if _, ok := store.byID[existingID]; ok {
			return
		}
	}
	store.nextIndex++
	id := fmt.Sprintf("image_%d", store.nextIndex)
	asset := chatImageAsset{
		ID:        id,
		Block:     block,
		MediaType: normalizeImageMediaType(block.Source.MediaType, block.Source.URL),
	}
	store.idByFingerprint[fp] = id
	store.byID[id] = asset
	if !store.announcedByID[id] && newAssets != nil {
		*newAssets = append(*newAssets, asset)
		store.announcedByID[id] = true
	}
}

func chatImageFingerprint(block types.ImageBlock) string {
	h := sha256.New()
	_, _ = h.Write([]byte(block.Source.Type))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(block.Source.MediaType))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(block.Source.URL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(block.Source.Data))
	return hex.EncodeToString(h.Sum(nil))
}

func downloadImageAsset(ctx context.Context, asset chatImageAsset, targetPath string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	targetPath = strings.TrimSpace(targetPath)
	mediaType := normalizeImageMediaType(asset.MediaType, asset.Block.Source.URL)
	if targetPath == "" {
		ext := extensionForImage(mediaType, asset.Block.Source.URL)
		targetPath = asset.ID + ext
	}
	if !filepath.IsAbs(targetPath) {
		abs, err := filepath.Abs(targetPath)
		if err != nil {
			return "", err
		}
		targetPath = abs
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", err
	}

	switch strings.ToLower(strings.TrimSpace(asset.Block.Source.Type)) {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(asset.Block.Source.Data)
		if err != nil {
			return "", fmt.Errorf("decode image data: %w", err)
		}
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return "", err
		}
		return targetPath, nil
	case "url":
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, asset.Block.Source.URL, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("download failed with status %s", resp.Status)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return "", err
		}
		return targetPath, nil
	default:
		return "", errors.New("unsupported image source type")
	}
}

func normalizeImageMediaType(mediaType, rawURL string) string {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "" {
		return mediaType
	}
	return mediaTypeFromPath(rawURL)
}

func extensionForImage(mediaType, rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(urlPath(rawURL))))
	if ext != "" {
		return ext
	}
	return ".img"
}

func mediaTypeFromPath(rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(urlPath(rawURL)))) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return ""
	}
}

func urlPath(raw string) string {
	parsed, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Path == "" {
		return raw
	}
	return parsed.Path
}
