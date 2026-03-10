package types

import (
	"bytes"
	"encoding/json"
	"time"
)

type rawChainDefaults struct {
	Model             string          `json:"model,omitempty"`
	System            json.RawMessage `json:"system,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	GatewayTools      []string        `json:"gateway_tools,omitempty"`
	GatewayToolConfig map[string]any  `json:"gateway_tool_config,omitempty"`
	ToolChoice        *ToolChoice     `json:"tool_choice,omitempty"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	STTModel          string          `json:"stt_model,omitempty"`
	TTSModel          string          `json:"tts_model,omitempty"`
	OutputFormat      *OutputFormat   `json:"output_format,omitempty"`
	Output            *OutputConfig   `json:"output,omitempty"`
	Voice             *VoiceConfig    `json:"voice,omitempty"`
	Extensions        map[string]any  `json:"extensions,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
}

func (d *ChainDefaults) UnmarshalJSON(data []byte) error {
	if isNullRaw(data) {
		*d = ChainDefaults{}
		return nil
	}
	var raw rawChainDefaults
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := ChainDefaults{
		Model:             raw.Model,
		Tools:             raw.Tools,
		GatewayTools:      raw.GatewayTools,
		GatewayToolConfig: raw.GatewayToolConfig,
		ToolChoice:        raw.ToolChoice,
		MaxTokens:         raw.MaxTokens,
		Temperature:       raw.Temperature,
		TopP:              raw.TopP,
		TopK:              raw.TopK,
		StopSequences:     raw.StopSequences,
		STTModel:          raw.STTModel,
		TTSModel:          raw.TTSModel,
		OutputFormat:      raw.OutputFormat,
		Output:            raw.Output,
		Voice:             raw.Voice,
		Extensions:        raw.Extensions,
		Metadata:          raw.Metadata,
	}
	if system, err := decodeFlexibleContentJSON(raw.System); err != nil {
		return err
	} else {
		out.System = system
	}
	*d = out
	return nil
}

type rawRunTimelineItem struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	StepIndex       int               `json:"step_index,omitempty"`
	StepID          string            `json:"step_id,omitempty"`
	ExecutionID     string            `json:"execution_id,omitempty"`
	SequenceInRun   int               `json:"sequence_in_run,omitempty"`
	SequenceInChain int               `json:"sequence_in_chain,omitempty"`
	Content         []json.RawMessage `json:"content,omitempty"`
	Tool            *RunTimelineTool  `json:"tool,omitempty"`
	AssetID         string            `json:"asset_id,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
}

func (i *RunTimelineItem) UnmarshalJSON(data []byte) error {
	if isNullRaw(data) {
		*i = RunTimelineItem{}
		return nil
	}
	var raw rawRunTimelineItem
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	content, err := unmarshalContentBlocks(raw.Content)
	if err != nil {
		return err
	}
	*i = RunTimelineItem{
		ID:              raw.ID,
		Kind:            raw.Kind,
		StepIndex:       raw.StepIndex,
		StepID:          raw.StepID,
		ExecutionID:     raw.ExecutionID,
		SequenceInRun:   raw.SequenceInRun,
		SequenceInChain: raw.SequenceInChain,
		Content:         content,
		Tool:            raw.Tool,
		AssetID:         raw.AssetID,
		CreatedAt:       raw.CreatedAt,
	}
	return nil
}

func decodeFlexibleContentJSON(data []byte) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}
	if blocks, err := UnmarshalContentBlocks(trimmed); err == nil {
		return blocks, nil
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, err
	}
	return value, nil
}
