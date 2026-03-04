package compat

import (
	"fmt"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

type providerPolicy struct {
	UnsupportedBlocks map[string]struct{}
	UnsupportedTools  map[string]struct{}
	Thinking          Support
	OutputFormat      Support
}

var providerPolicies = map[string]providerPolicy{
	"anthropic": {
		UnsupportedBlocks: setOf("video"),
		UnsupportedTools:  setOf(types.ToolTypeFileSearch),
		Thinking:          SupportSupported,
		OutputFormat:      SupportSupported,
	},
	"openai": {
		UnsupportedBlocks: setOf("video", "document", "thinking", "server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebSearch,
			types.ToolTypeWebFetch,
			types.ToolTypeCodeExecution,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportUnsupported,
		OutputFormat: SupportSupported,
	},
	"oai-resp": {
		UnsupportedBlocks: setOf("video", "server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebFetch,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportSupported,
		OutputFormat: SupportSupported,
	},
	"gem-dev": {
		UnsupportedBlocks: setOf("server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebFetch,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportSupported,
		OutputFormat: SupportSupported,
	},
	"gem-vert": {
		UnsupportedBlocks: setOf("server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebFetch,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportSupported,
		OutputFormat: SupportSupported,
	},
	"groq": {
		UnsupportedBlocks: setOf("video", "document", "thinking", "server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebSearch,
			types.ToolTypeWebFetch,
			types.ToolTypeCodeExecution,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportUnsupported,
		OutputFormat: SupportSupported,
	},
	"cerebras": {
		UnsupportedBlocks: setOf("image", "audio", "video", "document", "thinking", "server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebSearch,
			types.ToolTypeWebFetch,
			types.ToolTypeCodeExecution,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportUnsupported,
		OutputFormat: SupportSupported,
	},
	"openrouter": {
		UnsupportedBlocks: setOf("video", "document", "thinking", "server_tool_use", "web_search_tool_result"),
		UnsupportedTools: setOf(
			types.ToolTypeWebSearch,
			types.ToolTypeWebFetch,
			types.ToolTypeCodeExecution,
			types.ToolTypeFileSearch,
			types.ToolTypeComputerUse,
			types.ToolTypeTextEditor,
		),
		Thinking:     SupportUnsupported,
		OutputFormat: SupportSupported,
	},
}

var modelPolicyOverrides = map[string]providerPolicy{}

func setOf(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

func resolvePolicy(provider, modelID string) providerPolicy {
	policy, ok := providerPolicies[provider]
	if !ok {
		return providerPolicy{}
	}
	if override, ok := modelPolicyOverrides[modelID]; ok {
		for t := range override.UnsupportedBlocks {
			if policy.UnsupportedBlocks == nil {
				policy.UnsupportedBlocks = make(map[string]struct{})
			}
			policy.UnsupportedBlocks[t] = struct{}{}
		}
		for t := range override.UnsupportedTools {
			if policy.UnsupportedTools == nil {
				policy.UnsupportedTools = make(map[string]struct{})
			}
			policy.UnsupportedTools[t] = struct{}{}
		}
		if override.Thinking != SupportUnknown {
			policy.Thinking = override.Thinking
		}
		if override.OutputFormat != SupportUnknown {
			policy.OutputFormat = override.OutputFormat
		}
	}
	return policy
}

// ValidateMessageRequest validates provider/model compatibility and returns
// all incompatibility issues found. Unknown capabilities are not rejected.
func ValidateMessageRequest(req *types.MessageRequest, provider, modelID string) []core.CompatibilityIssue {
	if req == nil {
		return nil
	}

	// Enforce compatibility only when the catalog asserts support/unsupported.
	// Unknown models are pass-through.
	model, ok := LookupModel(modelID)
	if !ok || model.Provider != provider {
		return nil
	}

	policy := resolvePolicy(provider, modelID)
	if model.Capabilities.Thinking != SupportUnknown {
		policy.Thinking = model.Capabilities.Thinking
	}
	if model.Capabilities.StructuredOutput != SupportUnknown {
		policy.OutputFormat = model.Capabilities.StructuredOutput
	}
	issues := make([]core.CompatibilityIssue, 0, 4)

	switch system := req.System.(type) {
	case []types.ContentBlock:
		for i, block := range system {
			validateBlock(block, fmt.Sprintf("system[%d]", i), provider, policy, &issues)
		}
	}

	for i, msg := range req.Messages {
		for j, block := range msg.ContentBlocks() {
			validateBlock(block, fmt.Sprintf("messages[%d].content[%d]", i, j), provider, policy, &issues)
		}
	}

	for i, tool := range req.Tools {
		if _, unsupported := policy.UnsupportedTools[tool.Type]; unsupported {
			issues = append(issues, newIssue(
				fmt.Sprintf("tools[%d].type", i),
				"unsupported_tool_type",
				fmt.Sprintf("%s tools are not supported by %s", tool.Type, provider),
			))
		}
	}

	if req.OutputFormat != nil && policy.OutputFormat == SupportUnsupported {
		issues = append(issues, newIssue(
			"output_format",
			"unsupported_output_format",
			"structured output is not supported by this model",
		))
	}

	return issues
}

func validateBlock(
	block types.ContentBlock,
	param string,
	provider string,
	policy providerPolicy,
	issues *[]core.CompatibilityIssue,
) {
	blockType := contentBlockType(block)
	if blockType == "" {
		return
	}

	if blockType == "thinking" && policy.Thinking == SupportUnsupported {
		*issues = append(*issues, newIssue(
			param,
			"unsupported_thinking",
			"extended thinking is not supported by this model",
		))
	} else if _, unsupported := policy.UnsupportedBlocks[blockType]; unsupported {
		*issues = append(*issues, newIssue(
			param,
			"unsupported_content_block",
			fmt.Sprintf("%s blocks are not supported by %s", blockType, provider),
		))
	}

	switch b := block.(type) {
	case types.ToolResultBlock:
		for i, nested := range b.Content {
			validateBlock(nested, fmt.Sprintf("%s.content[%d]", param, i), provider, policy, issues)
		}
	case *types.ToolResultBlock:
		if b == nil {
			return
		}
		for i, nested := range b.Content {
			validateBlock(nested, fmt.Sprintf("%s.content[%d]", param, i), provider, policy, issues)
		}
	}
}

func contentBlockType(block types.ContentBlock) string {
	switch b := block.(type) {
	case types.TextBlock:
		return b.Type
	case *types.TextBlock:
		if b != nil {
			return b.Type
		}
	case types.ImageBlock:
		return b.Type
	case *types.ImageBlock:
		if b != nil {
			return b.Type
		}
	case types.AudioBlock:
		return b.Type
	case *types.AudioBlock:
		if b != nil {
			return b.Type
		}
	case types.VideoBlock:
		return b.Type
	case *types.VideoBlock:
		if b != nil {
			return b.Type
		}
	case types.DocumentBlock:
		return b.Type
	case *types.DocumentBlock:
		if b != nil {
			return b.Type
		}
	case types.ToolUseBlock:
		return b.Type
	case *types.ToolUseBlock:
		if b != nil {
			return b.Type
		}
	case types.ToolResultBlock:
		return b.Type
	case *types.ToolResultBlock:
		if b != nil {
			return b.Type
		}
	case types.ThinkingBlock:
		return b.Type
	case *types.ThinkingBlock:
		if b != nil {
			return b.Type
		}
	case types.ServerToolUseBlock:
		return b.Type
	case *types.ServerToolUseBlock:
		if b != nil {
			return b.Type
		}
	case types.WebSearchToolResultBlock:
		return b.Type
	case *types.WebSearchToolResultBlock:
		if b != nil {
			return b.Type
		}
	}
	return ""
}

func newIssue(param, code, message string) core.CompatibilityIssue {
	return core.CompatibilityIssue{
		Severity: "error",
		Param:    param,
		Code:     code,
		Message:  message,
	}
}
