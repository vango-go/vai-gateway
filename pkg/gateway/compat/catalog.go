package compat

import "strings"

// Support is a tri-state capability marker.
type Support int

const (
	SupportUnknown Support = iota
	SupportSupported
	SupportUnsupported
)

// ModelCapabilities describes per-model feature support.
type ModelCapabilities struct {
	Streaming        Support
	Tools            Support
	NativeWebSearch  Support
	NativeCodeExec   Support
	Vision           Support
	Documents        Support
	StructuredOutput Support
	Thinking         Support
}

// CatalogModel is a model entry returned by the static compatibility catalog.
type CatalogModel struct {
	ID           string
	Provider     string
	Name         string
	Capabilities ModelCapabilities
}

var curatedModels = []CatalogModel{
	{
		ID:       "anthropic/claude-sonnet-4",
		Provider: "anthropic",
		Name:     "claude-sonnet-4",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "openai/gpt-4o",
		Provider: "openai",
		Name:     "gpt-4o",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportUnsupported,
			NativeCodeExec:   SupportUnsupported,
			Vision:           SupportSupported,
			Documents:        SupportUnsupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportUnsupported,
		},
	},
	{
		ID:       "openai/gpt-4o-mini",
		Provider: "openai",
		Name:     "gpt-4o-mini",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportUnsupported,
			NativeCodeExec:   SupportUnsupported,
			Vision:           SupportSupported,
			Documents:        SupportUnsupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportUnsupported,
		},
	},
	{
		ID:       "oai-resp/gpt-5",
		Provider: "oai-resp",
		Name:     "gpt-5",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "oai-resp/gpt-5-mini",
		Provider: "oai-resp",
		Name:     "gpt-5-mini",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "gem-dev/gemini-2.5-pro",
		Provider: "gem-dev",
		Name:     "gemini-2.5-pro",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "gem-dev/gemini-2.5-flash",
		Provider: "gem-dev",
		Name:     "gemini-2.5-flash",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "gem-vert/gemini-2.5-pro",
		Provider: "gem-vert",
		Name:     "gemini-2.5-pro",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "gem-vert/gemini-2.5-flash",
		Provider: "gem-vert",
		Name:     "gemini-2.5-flash",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "gem-vert/gemini-3-flash-preview",
		Provider: "gem-vert",
		Name:     "gemini-3-flash-preview",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportSupported,
			NativeCodeExec:   SupportSupported,
			Vision:           SupportSupported,
			Documents:        SupportSupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportSupported,
		},
	},
	{
		ID:       "groq/llama-3.3-70b-versatile",
		Provider: "groq",
		Name:     "llama-3.3-70b-versatile",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportUnsupported,
			NativeCodeExec:   SupportUnsupported,
			Vision:           SupportSupported,
			Documents:        SupportUnsupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportUnsupported,
		},
	},
	{
		ID:       "cerebras/llama-3.3-70b",
		Provider: "cerebras",
		Name:     "llama-3.3-70b",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportUnsupported,
			NativeCodeExec:   SupportUnsupported,
			Vision:           SupportUnsupported,
			Documents:        SupportUnsupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportUnsupported,
		},
	},
	{
		ID:       "openrouter/openai/gpt-4o",
		Provider: "openrouter",
		Name:     "openai/gpt-4o",
		Capabilities: ModelCapabilities{
			Streaming:        SupportSupported,
			Tools:            SupportSupported,
			NativeWebSearch:  SupportUnsupported,
			NativeCodeExec:   SupportUnsupported,
			Vision:           SupportSupported,
			Documents:        SupportUnsupported,
			StructuredOutput: SupportSupported,
			Thinking:         SupportUnsupported,
		},
	},
}

// CatalogModels returns a copy of the curated model catalog.
func CatalogModels() []CatalogModel {
	out := make([]CatalogModel, len(curatedModels))
	copy(out, curatedModels)
	return out
}

// LookupModel returns a curated model entry by canonical model id.
func LookupModel(modelID string) (CatalogModel, bool) {
	for _, m := range curatedModels {
		if m.ID == modelID {
			return m, true
		}
	}
	return CatalogModel{}, false
}

// SplitModelID splits a model id into provider and model name.
// It splits on the first slash only.
func SplitModelID(modelID string) (provider string, name string, ok bool) {
	modelID = strings.TrimSpace(modelID)
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ProviderKeyHeader maps a provider to the required BYOK header.
func ProviderKeyHeader(provider string) (string, bool) {
	switch provider {
	case "anthropic":
		return "X-Provider-Key-Anthropic", true
	case "openai":
		return "X-Provider-Key-OpenAI", true
	case "oai-resp":
		return "X-Provider-Key-OpenAI", true
	case "gem-dev":
		return "X-Provider-Key-Gemini", true
	case "gem-vert":
		return "X-Provider-Key-VertexAI", true
	case "groq":
		return "X-Provider-Key-Groq", true
	case "cerebras":
		return "X-Provider-Key-Cerebras", true
	case "openrouter":
		return "X-Provider-Key-OpenRouter", true
	default:
		return "", false
	}
}
