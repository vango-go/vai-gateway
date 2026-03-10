package chains

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vai-lite/pkg/gateway/tools/servertools"
)

type scriptedChainProvider struct {
	mu        sync.Mutex
	responses []*types.MessageResponse
	requests  []*types.MessageRequest
}

func (p *scriptedChainProvider) Name() string { return "anthropic" }

func (p *scriptedChainProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{Tools: true}
}

func (p *scriptedChainProvider) CreateMessage(ctx context.Context, req *types.MessageRequest) (*types.MessageResponse, error) {
	return nil, io.ErrUnexpectedEOF
}

func (p *scriptedChainProvider) StreamMessage(ctx context.Context, req *types.MessageRequest) (core.EventStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := len(p.requests)
	p.requests = append(p.requests, cloneMessageRequest(req))
	if index >= len(p.responses) {
		return nil, io.ErrUnexpectedEOF
	}
	return &scriptedChainEventStream{events: chainEventsForResponse(p.responses[index])}, nil
}

func cloneMessageRequest(req *types.MessageRequest) *types.MessageRequest {
	if req == nil {
		return nil
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return req
	}
	var out types.MessageRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		return req
	}
	return &out
}

type scriptedChainEventStream struct {
	events []types.StreamEvent
	index  int
}

func (s *scriptedChainEventStream) Next() (types.StreamEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *scriptedChainEventStream) Close() error { return nil }

func chainEventsForResponse(resp *types.MessageResponse) []types.StreamEvent {
	if resp == nil {
		return nil
	}
	events := []types.StreamEvent{
		types.MessageStartEvent{
			Type:    "message_start",
			Message: types.MessageResponse{Type: resp.Type, Role: resp.Role, Model: resp.Model},
		},
	}
	for i, block := range resp.Content {
		switch typed := block.(type) {
		case types.TextBlock:
			events = append(events, types.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: types.TextBlock{Type: "text", Text: ""},
			})
			if typed.Text != "" {
				events = append(events, types.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: i,
					Delta: types.TextDelta{Type: "text_delta", Text: typed.Text},
				})
			}
		default:
			events = append(events, types.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: block,
			})
		}
	}
	delta := types.MessageDeltaEvent{Type: "message_delta"}
	delta.Delta.StopReason = resp.StopReason
	events = append(events, delta)
	return events
}

type fakeGatewayTool struct {
	name    string
	content []types.ContentBlock
}

func (f fakeGatewayTool) Name() string { return f.name }

func (f fakeGatewayTool) Definition() types.Tool {
	additionalProps := false
	return types.Tool{
		Type:        types.ToolTypeFunction,
		Name:        f.name,
		Description: "fake gateway tool",
		InputSchema: &types.JSONSchema{
			Type:                 "object",
			AdditionalProperties: &additionalProps,
		},
	}
}

func (f fakeGatewayTool) Execute(ctx context.Context, input map[string]any) ([]types.ContentBlock, *types.Error) {
	return cloneContentBlocks(f.content), nil
}

func TestRunBlocking_PreservesGatewayToolResultsAcrossTurnsAndTimeline(t *testing.T) {
	provider := &scriptedChainProvider{
		responses: []*types.MessageResponse{
			{
				Type:       "message",
				Role:       "assistant",
				Model:      "anthropic/test",
				StopReason: types.StopReasonToolUse,
				Content: []types.ContentBlock{
					types.ToolUseBlock{
						Type:  "tool_use",
						ID:    "call_1",
						Name:  servertools.ToolWebSearch,
						Input: map[string]any{"query": "Iran"},
					},
				},
			},
			{
				Type:       "message",
				Role:       "assistant",
				Model:      "anthropic/test",
				StopReason: types.StopReasonEndTurn,
				Content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: "Here is the answer."},
				},
			},
		},
	}
	manager := NewManager(nil, DefaultManagerConfig())
	env := RuntimeEnvironment{
		Principal: Principal{
			OrgID:       "org_1",
			PrincipalID: "principal_1",
		},
		Mode:     types.AttachmentModeStatefulSSE,
		Protocol: "sse",
		Scope: CredentialScope{
			ProviderKeys: map[string]string{
				"anthropic": "sk-test",
			},
			AllowedGatewayTools: map[string]struct{}{
				servertools.ToolWebSearch: {},
			},
		},
		NewProvider: func(providerName, apiKey string) (core.Provider, error) {
			return provider, nil
		},
		BuildGatewayTools: func(enabled []string, rawConfig map[string]any) (*servertools.Registry, error) {
			return servertools.NewRegistry(fakeGatewayTool{
				name: servertools.ToolWebSearch,
				content: []types.ContentBlock{
					types.TextBlock{Type: "text", Text: `{"provider":"tavily","results":[{"title":"Result","url":"https://example.com","snippet":"Snippet"}]}`},
				},
			}), nil
		},
	}

	started, _, err := manager.StartChain(context.Background(), env.Principal, env, types.ChainStartPayload{
		Defaults: types.ChainDefaults{
			Model:        "anthropic/test",
			GatewayTools: []string{servertools.ToolWebSearch},
		},
	}, "chain_start_tool_result")
	if err != nil {
		t.Fatalf("StartChain() error = %v", err)
	}

	run, result, err := manager.RunBlocking(context.Background(), started.ChainID, env, types.RunStartPayload{
		Input: []types.Message{
			{Role: "user", Content: []types.ContentBlock{types.TextBlock{Type: "text", Text: "What is recent news in Iran?"}}},
		},
	}, "run_start_tool_result")
	if err != nil {
		t.Fatalf("RunBlocking() error = %v", err)
	}
	if result == nil || result.Response == nil {
		t.Fatalf("RunBlocking() result = %#v", result)
	}
	if got := result.Response.TextContent(); got != "Here is the answer." {
		t.Fatalf("response text = %q, want %q", got, "Here is the answer.")
	}

	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	secondRequest := provider.requests[1]
	if len(secondRequest.Messages) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(secondRequest.Messages))
	}
	toolResultMsg := secondRequest.Messages[2]
	blocks := toolResultMsg.ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("tool result block count = %d, want 1", len(blocks))
	}
	toolResult, ok := blocks[0].(types.ToolResultBlock)
	if !ok {
		t.Fatalf("tool result block type = %T, want ToolResultBlock", blocks[0])
	}
	if len(toolResult.Content) != 1 {
		t.Fatalf("tool result content len = %d, want 1", len(toolResult.Content))
	}
	text, ok := toolResult.Content[0].(types.TextBlock)
	if !ok {
		t.Fatalf("tool result content type = %T, want TextBlock", toolResult.Content[0])
	}
	if text.Text == "" {
		t.Fatal("tool result text should be preserved")
	}

	timeline, err := manager.GetRunTimeline(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRunTimeline() error = %v", err)
	}
	foundToolResult := false
	for _, item := range timeline.Items {
		if item.Kind != "tool_result" {
			continue
		}
		if len(item.Content) != 1 {
			t.Fatalf("timeline tool_result content len = %d, want 1", len(item.Content))
		}
		tb, ok := item.Content[0].(types.TextBlock)
		if !ok {
			t.Fatalf("timeline tool_result content type = %T, want TextBlock", item.Content[0])
		}
		if tb.Text == "" {
			t.Fatal("timeline tool_result text should be preserved")
		}
		foundToolResult = true
	}
	if !foundToolResult {
		t.Fatal("expected tool_result item in timeline")
	}
}

func TestNormalizeToolResultContent_ConvertsEmptyResultIntoExplicitEmptyTextBlock(t *testing.T) {
	content := normalizeToolResultContent(nil)
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	tb, ok := content[0].(types.TextBlock)
	if !ok {
		t.Fatalf("content[0] type = %T, want TextBlock", content[0])
	}
	if tb.Text != "" {
		t.Fatalf("content[0].Text = %q, want empty string", tb.Text)
	}
}
