package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ServerToolExecuteRequest is the request body for /v1/server-tools:execute.
type ServerToolExecuteRequest struct {
	Tool             string         `json:"tool"`
	Input            map[string]any `json:"input"`
	ServerToolConfig map[string]any `json:"server_tool_config,omitempty"`
}

// ServerToolExecuteResponse is the response body for /v1/server-tools:execute.
type ServerToolExecuteResponse struct {
	Content []ContentBlock `json:"content"`
}

// UnmarshalServerToolExecuteRequestStrict strictly decodes a ServerToolExecuteRequest.
func UnmarshalServerToolExecuteRequestStrict(data []byte) (*ServerToolExecuteRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}

	for k := range top {
		switch k {
		case "tool", "input", "server_tool_config":
		default:
			return nil, strictErr(k, fmt.Sprintf("unknown field %q", k))
		}
	}

	toolRaw, ok := top["tool"]
	if !ok || isNullOrEmptyJSON(toolRaw) {
		return nil, strictErr("tool", "tool is required")
	}
	var tool string
	if err := json.Unmarshal(toolRaw, &tool); err != nil {
		return nil, strictErr("tool", "tool must be a string")
	}
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return nil, strictErr("tool", "tool must be non-empty")
	}

	inputRaw, ok := top["input"]
	if !ok || isNullOrEmptyJSON(inputRaw) {
		return nil, strictErr("input", "input is required")
	}
	var input map[string]any
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return nil, strictErr("input", "input must be an object")
	}
	if input == nil {
		return nil, strictErr("input", "input must be an object")
	}

	out := &ServerToolExecuteRequest{
		Tool:  tool,
		Input: input,
	}

	if cfgRaw, ok := top["server_tool_config"]; ok && !isNullOrEmptyJSON(cfgRaw) {
		var rawCfg map[string]json.RawMessage
		if err := json.Unmarshal(cfgRaw, &rawCfg); err != nil {
			return nil, strictErr("server_tool_config", "server_tool_config must be an object")
		}
		out.ServerToolConfig = make(map[string]any, len(rawCfg))
		for name, value := range rawCfg {
			toolName := strings.TrimSpace(name)
			if toolName == "" {
				return nil, strictErr("server_tool_config", "server_tool_config keys must be non-empty")
			}
			if isNullOrEmptyJSON(value) {
				return nil, strictErr("server_tool_config."+toolName, "server_tool_config entries must be objects")
			}
			var obj map[string]any
			if err := json.Unmarshal(value, &obj); err != nil {
				return nil, strictErr("server_tool_config."+toolName, "server_tool_config entries must be objects")
			}
			if obj == nil {
				return nil, strictErr("server_tool_config."+toolName, "server_tool_config entries must be objects")
			}
			out.ServerToolConfig[toolName] = obj
		}
	}

	return out, nil
}

// UnmarshalServerToolExecuteResponse decodes a server-tool execute response.
func UnmarshalServerToolExecuteResponse(data []byte) (*ServerToolExecuteResponse, error) {
	var raw struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	content, err := unmarshalContentBlocks(raw.Content)
	if err != nil {
		return nil, err
	}
	return &ServerToolExecuteResponse{Content: content}, nil
}
