package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/vango-go/vai-lite/pkg/core/types"
)

// anthropicRequest is the Anthropic API request format.
// Since Vango AI API is based on Anthropic, this is nearly identical.
type anthropicRequest struct {
	Model         string                 `json:"model"`
	Messages      []messageJSON          `json:"messages"`
	MaxTokens     int                    `json:"max_tokens"`
	System        any                    `json:"system,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	TopK          *int                   `json:"top_k,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	ToolChoice    *types.ToolChoice      `json:"tool_choice,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Metadata      map[string]any         `json:"metadata,omitempty"`
	OutputFormat  *anthropicOutputFormat `json:"output_format,omitempty"`
}

// anthropicOutputFormat represents structured output configuration.
type anthropicOutputFormat struct {
	Type   string            `json:"type"`             // "json_schema"
	Schema *types.JSONSchema `json:"schema,omitempty"` // The JSON schema
}

// messageJSON is the wire format for messages.
type messageJSON struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// anthropicTool represents a tool in Anthropic's format.
type anthropicTool struct {
	Type         string            `json:"type"`
	Name         string            `json:"name,omitempty"`
	Description  string            `json:"description,omitempty"`
	InputSchema  *types.JSONSchema `json:"input_schema,omitempty"`
	CacheControl *cacheControl     `json:"cache_control,omitempty"`

	// Web search specific fields
	MaxUses        int                 `json:"max_uses,omitempty"`
	AllowedDomains []string            `json:"allowed_domains,omitempty"`
	BlockedDomains []string            `json:"blocked_domains,omitempty"`
	UserLocation   *types.UserLocation `json:"user_location,omitempty"`

	// Computer use specific fields
	DisplayWidthPx  int `json:"display_width_px,omitempty"`
	DisplayHeightPx int `json:"display_height_px,omitempty"`
}

// cacheControl specifies prompt caching behavior.
type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// buildRequest converts a Vango AI request to an Anthropic request.
func buildRequest(req *types.MessageRequest) *anthropicRequest {
	anthReq := &anthropicRequest{
		Model:         req.Model, // Already stripped by Engine
		MaxTokens:     req.MaxTokens,
		System:        normalizeSystem(req.System),
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
		Metadata:      req.Metadata,
		ToolChoice:    req.ToolChoice,
	}

	// Set default max_tokens if not specified
	if anthReq.MaxTokens == 0 {
		anthReq.MaxTokens = DefaultMaxTokens
	}

	// Convert messages
	anthReq.Messages = convertMessages(req.Messages)

	// Convert tools
	if len(req.Tools) > 0 {
		anthReq.Tools = convertTools(req.Tools)
	}

	// Convert output format for structured outputs
	if req.OutputFormat != nil && req.OutputFormat.Type == "json_schema" {
		anthReq.OutputFormat = &anthropicOutputFormat{
			Type:   "json_schema",
			Schema: req.OutputFormat.JSONSchema,
		}
	}

	// Apply extensions (Anthropic-specific options)
	applyExtensions(anthReq, req.Extensions)

	return anthReq
}

// convertMessages converts Vango AI messages to Anthropic format.
func convertMessages(messages []types.Message) []messageJSON {
	result := make([]messageJSON, 0, len(messages))

	for _, msg := range messages {
		jsonMsg := messageJSON{
			Role: msg.Role,
		}

		// Convert content to JSON
		contentBlocks := msg.ContentBlocks()
		for _, block := range contentBlocks {
			sanitized, ok := sanitizeAnthropicInputBlock(block)
			if !ok {
				continue
			}
			blockJSON, err := json.Marshal(sanitized)
			if err != nil {
				continue // Skip invalid blocks
			}
			jsonMsg.Content = append(jsonMsg.Content, blockJSON)
		}

		// Anthropic rejects message text blocks with empty strings and can reject
		// messages that have no content. Drop empty messages after sanitization.
		if len(jsonMsg.Content) > 0 {
			result = append(result, jsonMsg)
		}
	}

	return result
}

// convertTools converts Vango AI tools to Anthropic format.
func convertTools(tools []types.Tool) []anthropicTool {
	result := make([]anthropicTool, 0, len(tools))

	for _, tool := range tools {
		switch tool.Type {
		case types.ToolTypeFunction:
			result = append(result, anthropicTool{
				Type:        "custom", // Anthropic uses "custom" for function tools
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
			})

		case types.ToolTypeWebSearch:
			webTool := anthropicTool{
				Type: "web_search_20250305",
				Name: "web_search",
			}
			// Apply config if provided
			if cfg, ok := tool.Config.(*types.WebSearchConfig); ok && cfg != nil {
				webTool.MaxUses = cfg.MaxUses
				webTool.AllowedDomains = cfg.AllowedDomains
				webTool.BlockedDomains = cfg.BlockedDomains
				webTool.UserLocation = cfg.UserLocation
			} else if cfg, ok := tool.Config.(types.WebSearchConfig); ok {
				webTool.MaxUses = cfg.MaxUses
				webTool.AllowedDomains = cfg.AllowedDomains
				webTool.BlockedDomains = cfg.BlockedDomains
				webTool.UserLocation = cfg.UserLocation
			}
			result = append(result, webTool)

		case types.ToolTypeWebFetch:
			fetchTool := anthropicTool{
				Type: "web_fetch_20250910",
				Name: "web_fetch",
			}
			// Apply config if provided
			if cfg, ok := tool.Config.(*types.WebFetchConfig); ok && cfg != nil {
				fetchTool.MaxUses = cfg.MaxUses
				fetchTool.AllowedDomains = cfg.AllowedDomains
				fetchTool.BlockedDomains = cfg.BlockedDomains
			} else if cfg, ok := tool.Config.(types.WebFetchConfig); ok {
				fetchTool.MaxUses = cfg.MaxUses
				fetchTool.AllowedDomains = cfg.AllowedDomains
				fetchTool.BlockedDomains = cfg.BlockedDomains
			}
			result = append(result, fetchTool)

		case types.ToolTypeCodeExecution:
			result = append(result, anthropicTool{
				Type: "code_execution_20250522",
				Name: "code_execution",
			})

		case types.ToolTypeComputerUse:
			compTool := anthropicTool{
				Type: "computer_20250124",
				Name: "computer",
			}
			// Apply config if provided
			if cfg, ok := tool.Config.(*types.ComputerUseConfig); ok && cfg != nil {
				compTool.DisplayWidthPx = cfg.DisplayWidth
				compTool.DisplayHeightPx = cfg.DisplayHeight
			} else if cfg, ok := tool.Config.(types.ComputerUseConfig); ok {
				compTool.DisplayWidthPx = cfg.DisplayWidth
				compTool.DisplayHeightPx = cfg.DisplayHeight
			}
			result = append(result, compTool)

		case types.ToolTypeTextEditor:
			result = append(result, anthropicTool{
				Type: "text_editor_20250124",
				Name: "str_replace_editor",
			})
		}
	}

	return result
}

// applyExtensions applies Anthropic-specific extensions.
func applyExtensions(req *anthropicRequest, ext map[string]any) {
	if ext == nil {
		return
	}

	anthExt, ok := ext["anthropic"].(map[string]any)
	if !ok {
		return
	}

	// Handle thinking extension
	if _, ok := anthExt["thinking"].(map[string]any); ok {
		// Thinking is handled via beta headers and model config
		// The request structure doesn't need modification
	}
}

// stripProviderPrefix removes the provider prefix from a model string.
// "anthropic/claude-sonnet-4" -> "claude-sonnet-4"
func stripProviderPrefix(model string) string {
	if idx := strings.Index(model, "/"); idx != -1 {
		return model[idx+1:]
	}
	return model
}

// normalizeSystem converts the System field to a format the API accepts.
// Anthropic accepts either a string or []ContentBlock, but not a single ContentBlock.
// This function normalizes a single ContentBlock to []ContentBlock.
func normalizeSystem(system any) any {
	if system == nil {
		return nil
	}

	// If it's already a string, return as-is
	if s, ok := system.(string); ok {
		if s == "" {
			return nil
		}
		return s
	}

	// If it's already a slice, sanitize before returning.
	if blocks, ok := system.([]types.ContentBlock); ok {
		out := make([]types.ContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if sanitized, ok := sanitizeAnthropicInputBlock(block); ok {
				out = append(out, sanitized)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	// If it's a single ContentBlock, sanitize and wrap in a slice.
	if block, ok := system.(types.ContentBlock); ok {
		if sanitized, ok := sanitizeAnthropicInputBlock(block); ok {
			return []types.ContentBlock{sanitized}
		}
		return nil
	}

	// For any other type, return as-is and let the API validate
	return system
}

func sanitizeAnthropicInputBlock(block types.ContentBlock) (types.ContentBlock, bool) {
	switch b := block.(type) {
	case types.TextBlock:
		if b.Text == "" {
			return nil, false
		}
		return b, true
	case *types.TextBlock:
		if b == nil || b.Text == "" {
			return nil, false
		}
		return *b, true
	case types.ToolResultBlock:
		sanitized := b
		if len(b.Content) > 0 {
			sanitized.Content = make([]types.ContentBlock, 0, len(b.Content))
			for _, child := range b.Content {
				if cleaned, ok := sanitizeAnthropicInputBlock(child); ok {
					sanitized.Content = append(sanitized.Content, cleaned)
				}
			}
		}
		return sanitized, true
	case *types.ToolResultBlock:
		if b == nil {
			return nil, false
		}
		sanitized := *b
		if len(b.Content) > 0 {
			sanitized.Content = make([]types.ContentBlock, 0, len(b.Content))
			for _, child := range b.Content {
				if cleaned, ok := sanitizeAnthropicInputBlock(child); ok {
					sanitized.Content = append(sanitized.Content, cleaned)
				}
			}
		}
		return sanitized, true
	default:
		return block, true
	}
}
