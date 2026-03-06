package servertools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

type imageRefRegistryContextKey struct{}

type ImageRefRegistry struct {
	byID          map[string]types.ImageBlock
	byFingerprint map[string]string
	count         int
}

func NewImageRefRegistry() *ImageRefRegistry {
	return &ImageRefRegistry{
		byID:          make(map[string]types.ImageBlock),
		byFingerprint: make(map[string]string),
	}
}

func BuildImageRefRegistry(messages []types.Message) *ImageRefRegistry {
	reg := NewImageRefRegistry()
	for i := range messages {
		registerImageRefsInBlocks(reg, messages[i].ContentBlocks())
	}
	return reg
}

func BuildPlannerMessages(messages []types.Message, includeImages bool) ([]types.Message, *ImageRefRegistry) {
	reg := BuildImageRefRegistry(messages)
	return injectPlannerImageRefs(messages, reg, includeImages), reg
}

func ContextWithImageRefRegistry(ctx context.Context, reg *ImageRefRegistry) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if reg == nil {
		return ctx
	}
	return context.WithValue(ctx, imageRefRegistryContextKey{}, reg)
}

func ImageRefRegistryFromContext(ctx context.Context) *ImageRefRegistry {
	if ctx == nil {
		return nil
	}
	reg, _ := ctx.Value(imageRefRegistryContextKey{}).(*ImageRefRegistry)
	return reg
}

func (r *ImageRefRegistry) Lookup(id string) (types.ImageBlock, bool) {
	if r == nil {
		return types.ImageBlock{}, false
	}
	block, ok := r.byID[strings.TrimSpace(id)]
	return block, ok
}

func (r *ImageRefRegistry) Register(block types.ImageBlock) string {
	if r == nil {
		return ""
	}
	fp := imageFingerprint(block)
	if id, ok := r.byFingerprint[fp]; ok {
		return id
	}
	r.count++
	id := fmt.Sprintf("img-%02d", r.count)
	r.byFingerprint[fp] = id
	r.byID[id] = block
	return id
}

func (r *ImageRefRegistry) IDFor(block types.ImageBlock) string {
	if r == nil {
		return ""
	}
	fp := imageFingerprint(block)
	if id, ok := r.byFingerprint[fp]; ok {
		return id
	}
	return ""
}

func InjectImageRefText(messages []types.Message, reg *ImageRefRegistry) []types.Message {
	return injectPlannerImageRefs(messages, reg, true)
}

func injectPlannerImageRefs(messages []types.Message, reg *ImageRefRegistry, includeImages bool) []types.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]types.Message, len(messages))
	for i := range messages {
		msg := messages[i]
		blocks := msg.ContentBlocks()
		if len(blocks) == 0 {
			out[i] = msg
			continue
		}
		out[i] = types.Message{
			Role:    msg.Role,
			Content: injectImageRefBlocks(blocks, reg, includeImages),
		}
	}
	return out
}

func registerImageRefsInBlocks(reg *ImageRefRegistry, blocks []types.ContentBlock) {
	for i := range blocks {
		switch b := blocks[i].(type) {
		case types.ImageBlock:
			reg.Register(b)
		case *types.ImageBlock:
			if b != nil {
				reg.Register(*b)
			}
		case types.ToolResultBlock:
			registerImageRefsInBlocks(reg, b.Content)
		case *types.ToolResultBlock:
			if b != nil {
				registerImageRefsInBlocks(reg, b.Content)
			}
		}
	}
}

func injectImageRefBlocks(blocks []types.ContentBlock, reg *ImageRefRegistry, includeImages bool) []types.ContentBlock {
	out := make([]types.ContentBlock, 0, len(blocks)+2)
	for i := range blocks {
		switch b := blocks[i].(type) {
		case types.ImageBlock:
			id := reg.Register(b)
			out = append(out, types.TextBlock{Type: "text", Text: id})
			if includeImages {
				out = append(out, b)
			} else {
				out = append(out, types.TextBlock{Type: "text", Text: "[image omitted for non-vision model]"})
			}
		case *types.ImageBlock:
			if b == nil {
				continue
			}
			id := reg.Register(*b)
			out = append(out, types.TextBlock{Type: "text", Text: id})
			if includeImages {
				out = append(out, *b)
			} else {
				out = append(out, types.TextBlock{Type: "text", Text: "[image omitted for non-vision model]"})
			}
		case types.ToolResultBlock:
			cloned := b
			cloned.Content = injectImageRefBlocks(cloned.Content, reg, includeImages)
			out = append(out, cloned)
		case *types.ToolResultBlock:
			if b == nil {
				continue
			}
			cloned := *b
			cloned.Content = injectImageRefBlocks(cloned.Content, reg, includeImages)
			out = append(out, cloned)
		default:
			out = append(out, blocks[i])
		}
	}
	return out
}

func imageFingerprint(block types.ImageBlock) string {
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
