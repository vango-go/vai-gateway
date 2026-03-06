package types

import (
	"strings"
	"testing"
)

func TestUnmarshalServerToolExecuteRequestStrict_Basic(t *testing.T) {
	req, err := UnmarshalServerToolExecuteRequestStrict([]byte(`{
		"tool":"vai_web_search",
		"input":{"query":"latest"},
		"server_tool_config":{"vai_web_search":{"provider":"tavily"}}
	}`))
	if err != nil {
		t.Fatalf("UnmarshalServerToolExecuteRequestStrict() error = %v", err)
	}
	if req.Tool != "vai_web_search" {
		t.Fatalf("tool=%q", req.Tool)
	}
	if req.Input["query"] != "latest" {
		t.Fatalf("input.query=%v", req.Input["query"])
	}
	if len(req.ServerToolConfig) != 1 {
		t.Fatalf("len(server_tool_config)=%d", len(req.ServerToolConfig))
	}
}

func TestUnmarshalServerToolExecuteRequestStrict_UnknownField(t *testing.T) {
	_, err := UnmarshalServerToolExecuteRequestStrict([]byte(`{"tool":"vai_web_search","input":{"query":"x"},"extra":true}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	se, ok := err.(*StrictDecodeError)
	if !ok {
		t.Fatalf("error type=%T", err)
	}
	if se.Param != "extra" {
		t.Fatalf("param=%q", se.Param)
	}
}

func TestUnmarshalServerToolExecuteRequestStrict_InputValidation(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{name: "missing tool", body: `{"input":{}}`, param: "tool"},
		{name: "empty tool", body: `{"tool":"  ","input":{}}`, param: "tool"},
		{name: "missing input", body: `{"tool":"vai_web_search"}`, param: "input"},
		{name: "input non-object", body: `{"tool":"vai_web_search","input":"nope"}`, param: "input"},
		{name: "config non-object", body: `{"tool":"vai_web_search","input":{},"server_tool_config":"nope"}`, param: "server_tool_config"},
		{name: "config entry non-object", body: `{"tool":"vai_web_search","input":{},"server_tool_config":{"vai_web_search":"nope"}}`, param: "server_tool_config.vai_web_search"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnmarshalServerToolExecuteRequestStrict([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error")
			}
			se, ok := err.(*StrictDecodeError)
			if !ok {
				t.Fatalf("error type=%T", err)
			}
			if se.Param != tc.param {
				t.Fatalf("param=%q, want %q", se.Param, tc.param)
			}
		})
	}
}

func TestUnmarshalServerToolExecuteResponse_DecodesContentBlocks(t *testing.T) {
	resp, err := UnmarshalServerToolExecuteResponse([]byte(`{
		"content":[{"type":"text","text":"ok"}]
	}`))
	if err != nil {
		t.Fatalf("UnmarshalServerToolExecuteResponse() error = %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("len(content)=%d", len(resp.Content))
	}
	if resp.Content[0].BlockType() != "text" {
		t.Fatalf("block type=%q", resp.Content[0].BlockType())
	}
}

func TestUnmarshalServerToolExecuteResponse_UnknownContentType(t *testing.T) {
	resp, err := UnmarshalServerToolExecuteResponse([]byte(`{"content":[{"type":"not_real","x":1}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("len(content)=%d", len(resp.Content))
	}
	if got := resp.Content[0].BlockType(); got != "not_real" {
		t.Fatalf("block type=%q", got)
	}
	if !strings.Contains(string(resp.Content[0].(UnknownContentBlock).Raw), `"x":1`) {
		t.Fatalf("raw block=%s", string(resp.Content[0].(UnknownContentBlock).Raw))
	}
}
