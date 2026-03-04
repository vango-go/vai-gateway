package gem

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/genai"

	"github.com/vango-go/vai-lite/pkg/core"
	"github.com/vango-go/vai-lite/pkg/core/types"
)

const (
	defaultInlineMediaMaxBytes      int64 = 20 * 1024 * 1024
	defaultInlineMediaTotalMaxBytes int64 = 20 * 1024 * 1024
	thoughtSignatureKey                   = "__thought_signature"
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

var imageMIMETypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/heic": {},
	"image/heif": {},
}

var audioMIMETypes = map[string]struct{}{
	"audio/x-aac": {},
	"audio/flac":  {},
	"audio/mp3":   {},
	"audio/m4a":   {},
	"audio/mpeg":  {},
	"audio/mpga":  {},
	"audio/mp4":   {},
	"audio/ogg":   {},
	"audio/pcm":   {},
	"audio/wav":   {},
	"audio/webm":  {},
}

var videoMIMETypes = map[string]struct{}{
	"video/mp4":   {},
	"video/mpeg":  {},
	"video/mov":   {},
	"video/avi":   {},
	"video/x-flv": {},
	"video/mpg":   {},
	"video/webm":  {},
	"video/wmv":   {},
	"video/3gpp":  {},
}

var documentMIMETypes = map[string]struct{}{
	"application/pdf": {},
}

type safetySettingExtension struct {
	Category  string
	Threshold string
	Method    string
}

type gemExtensions struct {
	VertexProject  string
	VertexLocation string

	StreamFunctionCallArguments *bool
	CandidateCount              int

	HasIncludeThoughts bool
	IncludeThoughts    bool
	ThinkingBudget     *int32
	ThinkingLevel      genai.ThinkingLevel

	MediaResolution *genai.MediaResolution
	InferMediaType  bool

	InlineMediaMaxBytes      int64
	InlineMediaTotalMaxBytes int64

	Grounding      map[string]any
	SafetySettings []safetySettingExtension
	SpeechConfig   *genai.SpeechConfig
}

type requestBuild struct {
	contents []*genai.Content
	config   *genai.GenerateContentConfig
	ext      gemExtensions
	warnings []string

	streamArgsEnabled  bool
	streamArgsExplicit bool
	streamArgsAuto     bool
}

type buildRequestOptions struct {
	isStream               bool
	autoStreamArgsOverride *bool
}

type streamArgsDecision struct {
	enabled  bool
	explicit bool
	auto     bool
}

func defaultBuildRequestOptions() buildRequestOptions {
	return buildRequestOptions{}
}

func (p *Provider) buildGenerateContentRequest(req *types.MessageRequest, opts buildRequestOptions) (*requestBuild, error) {
	ext, err := parseGemExtensions(req)
	if err != nil {
		return nil, err
	}

	cfg := &genai.GenerateContentConfig{}
	warnings := make([]string, 0, 4)

	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(req.MaxTokens)
	}
	if req.Temperature != nil {
		t := float32(*req.Temperature)
		cfg.Temperature = &t
	}
	if req.TopP != nil {
		t := float32(*req.TopP)
		cfg.TopP = &t
	}
	if req.TopK != nil {
		t := float32(*req.TopK)
		cfg.TopK = &t
	}
	if len(req.StopSequences) > 0 {
		cfg.StopSequences = append([]string(nil), req.StopSequences...)
	}

	if ext.CandidateCount > 0 {
		cfg.CandidateCount = int32(ext.CandidateCount)
	}

	if req.System != nil {
		systemText, err := systemInstructionText(req.System)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(systemText) != "" {
			cfg.SystemInstruction = genai.NewContentFromText(systemText, genai.RoleUser)
		}
	}

	if req.Output != nil {
		if err := applyOutputConfig(cfg, req.Output); err != nil {
			return nil, err
		}
	}
	if req.OutputFormat != nil {
		if err := applyOutputFormat(cfg, req.OutputFormat); err != nil {
			return nil, err
		}
	}

	if ext.MediaResolution != nil {
		cfg.MediaResolution = *ext.MediaResolution
	}
	if ext.SpeechConfig != nil {
		cfg.SpeechConfig = ext.SpeechConfig
	}

	if ext.HasIncludeThoughts || ext.ThinkingBudget != nil || ext.ThinkingLevel != "" {
		thinking := &genai.ThinkingConfig{IncludeThoughts: ext.IncludeThoughts}
		if ext.ThinkingBudget != nil {
			thinking.ThinkingBudget = ext.ThinkingBudget
		}
		if ext.ThinkingLevel != "" {
			thinking.ThinkingLevel = ext.ThinkingLevel
		}
		cfg.ThinkingConfig = thinking
	}

	if len(ext.SafetySettings) > 0 {
		cfg.SafetySettings = make([]*genai.SafetySetting, 0, len(ext.SafetySettings))
		for _, s := range ext.SafetySettings {
			set := &genai.SafetySetting{
				Category:  genai.HarmCategory(s.Category),
				Threshold: genai.HarmBlockThreshold(s.Threshold),
			}
			if p.mode == backendModeVertex && s.Method != "" {
				set.Method = genai.HarmBlockMethod(s.Method)
			}
			cfg.SafetySettings = append(cfg.SafetySettings, set)
		}
	}

	contents := make([]*genai.Content, 0, len(req.Messages))
	toolUseNameByID := map[string]string{}
	var totalInlineBytes int64

	for i := range req.Messages {
		msg := req.Messages[i]
		role, err := mapRole(msg.Role)
		if err != nil {
			return nil, core.NewInvalidRequestErrorWithParam(err.Error(), fmt.Sprintf("messages[%d].role", i))
		}

		blocks := msg.ContentBlocks()
		parts := make([]*genai.Part, 0, len(blocks))
		for j := range blocks {
			b := blocks[j]
			part, extraWarnings, err := p.translateInputBlock(b, ext, &totalInlineBytes, toolUseNameByID)
			if err != nil {
				param := fmt.Sprintf("messages[%d].content[%d]", i, j)
				if ce, ok := err.(*core.Error); ok {
					if ce.Param == "" {
						ce.Param = param
					}
					return nil, ce
				}
				return nil, core.NewInvalidRequestErrorWithParam(err.Error(), param)
			}
			warnings = append(warnings, extraWarnings...)
			if part != nil {
				parts = append(parts, part)
			}

			if tu, ok := asToolUseBlock(b); ok {
				toolUseNameByID[tu.ID] = tu.Name
			}
		}

		if len(parts) == 0 {
			continue
		}
		contents = append(contents, genai.NewContentFromParts(parts, role))
	}

	tools, toolCfg, streamArgs, err := p.translateTools(req.Tools, req.ToolChoice, ext, opts)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		cfg.Tools = tools
	}
	if toolCfg != nil {
		cfg.ToolConfig = toolCfg
	}

	if len(contents) == 0 {
		return nil, core.NewInvalidRequestError("messages must contain at least one content block")
	}

	return &requestBuild{
		contents:           contents,
		config:             cfg,
		ext:                ext,
		warnings:           warnings,
		streamArgsEnabled:  streamArgs.enabled,
		streamArgsExplicit: streamArgs.explicit,
		streamArgsAuto:     streamArgs.auto,
	}, nil
}

func parseGemExtensions(req *types.MessageRequest) (gemExtensions, error) {
	ext := gemExtensions{
		InlineMediaMaxBytes:      defaultInlineMediaMaxBytes,
		InlineMediaTotalMaxBytes: defaultInlineMediaTotalMaxBytes,
	}
	if req == nil || req.Extensions == nil {
		return ext, nil
	}
	raw, ok := req.Extensions["gem"]
	if !ok || raw == nil {
		return ext, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ext, core.NewInvalidRequestErrorWithParam("extensions.gem must be an object", "extensions.gem")
	}

	for k, v := range m {
		switch k {
		case "vertex_project":
			s, ok := v.(string)
			if !ok {
				return ext, invalidExtType(k, "string")
			}
			ext.VertexProject = strings.TrimSpace(s)
		case "vertex_location":
			s, ok := v.(string)
			if !ok {
				return ext, invalidExtType(k, "string")
			}
			ext.VertexLocation = strings.TrimSpace(s)
		case "stream_function_call_arguments":
			b, ok := v.(bool)
			if !ok {
				return ext, invalidExtType(k, "bool")
			}
			ext.StreamFunctionCallArguments = &b
		case "candidate_count":
			n, ok := anyToInt(v)
			if !ok {
				return ext, invalidExtType(k, "int")
			}
			ext.CandidateCount = n
		case "include_thoughts":
			b, ok := v.(bool)
			if !ok {
				return ext, invalidExtType(k, "bool")
			}
			ext.HasIncludeThoughts = true
			ext.IncludeThoughts = b
		case "thinking_budget":
			n, ok := anyToInt(v)
			if !ok {
				return ext, invalidExtType(k, "int")
			}
			t := int32(n)
			ext.ThinkingBudget = &t
		case "thinking_level":
			s, ok := v.(string)
			if !ok {
				return ext, invalidExtType(k, "string")
			}
			level, err := parseThinkingLevel(s)
			if err != nil {
				return ext, err
			}
			ext.ThinkingLevel = level
		case "media_resolution":
			s, ok := v.(string)
			if !ok {
				return ext, invalidExtType(k, "string")
			}
			mr, err := parseMediaResolution(s)
			if err != nil {
				return ext, err
			}
			ext.MediaResolution = &mr
		case "infer_media_type":
			b, ok := v.(bool)
			if !ok {
				return ext, invalidExtType(k, "bool")
			}
			ext.InferMediaType = b
		case "inline_media_max_bytes":
			n, ok := anyToInt(v)
			if !ok {
				return ext, invalidExtType(k, "int")
			}
			if n <= 0 {
				return ext, core.NewInvalidRequestErrorWithParam("gem.inline_media_max_bytes must be > 0", "extensions.gem.inline_media_max_bytes")
			}
			ext.InlineMediaMaxBytes = int64(n)
		case "inline_media_total_max_bytes":
			n, ok := anyToInt(v)
			if !ok {
				return ext, invalidExtType(k, "int")
			}
			if n <= 0 {
				return ext, core.NewInvalidRequestErrorWithParam("gem.inline_media_total_max_bytes must be > 0", "extensions.gem.inline_media_total_max_bytes")
			}
			ext.InlineMediaTotalMaxBytes = int64(n)
		case "grounding":
			gm, ok := v.(map[string]any)
			if !ok {
				return ext, invalidExtType(k, "object")
			}
			if _, err := json.Marshal(gm); err != nil {
				return ext, core.NewInvalidRequestErrorWithParam("gem.grounding must be JSON-serializable", "extensions.gem.grounding")
			}
			ext.Grounding = gm
		case "safety_settings":
			arr, ok := v.([]any)
			if !ok {
				return ext, core.NewInvalidRequestErrorWithParam("gem.safety_settings must be an array of objects", "extensions.gem.safety_settings")
			}
			settings := make([]safetySettingExtension, 0, len(arr))
			for i := range arr {
				obj, ok := arr[i].(map[string]any)
				if !ok {
					return ext, core.NewInvalidRequestErrorWithParam("gem.safety_settings must be an array of objects", "extensions.gem.safety_settings")
				}
				cat, ok := obj["category"].(string)
				if !ok || strings.TrimSpace(cat) == "" {
					return ext, core.NewInvalidRequestErrorWithParam("gem.safety_settings[].category must be string", fmt.Sprintf("extensions.gem.safety_settings[%d].category", i))
				}
				thr, ok := obj["threshold"].(string)
				if !ok || strings.TrimSpace(thr) == "" {
					return ext, core.NewInvalidRequestErrorWithParam("gem.safety_settings[].threshold must be string", fmt.Sprintf("extensions.gem.safety_settings[%d].threshold", i))
				}
				set := safetySettingExtension{Category: strings.TrimSpace(cat), Threshold: strings.TrimSpace(thr)}
				if rawMethod, ok := obj["method"]; ok {
					m, ok := rawMethod.(string)
					if !ok {
						return ext, core.NewInvalidRequestErrorWithParam("gem.safety_settings[].method must be string", fmt.Sprintf("extensions.gem.safety_settings[%d].method", i))
					}
					set.Method = strings.TrimSpace(m)
				}
				settings = append(settings, set)
			}
			ext.SafetySettings = settings
		case "speech_config":
			scMap, ok := v.(map[string]any)
			if !ok {
				return ext, invalidExtType(k, "object")
			}
			sc := &genai.SpeechConfig{}
			if rawLang, ok := scMap["language_code"]; ok {
				lang, ok := rawLang.(string)
				if !ok {
					return ext, core.NewInvalidRequestErrorWithParam("gem.speech_config.language_code must be string", "extensions.gem.speech_config.language_code")
				}
				sc.LanguageCode = strings.TrimSpace(lang)
			}
			if rawVoice, ok := scMap["voice_name"]; ok {
				voice, ok := rawVoice.(string)
				if !ok {
					return ext, core.NewInvalidRequestErrorWithParam("gem.speech_config.voice_name must be string", "extensions.gem.speech_config.voice_name")
				}
				sc.VoiceConfig = &genai.VoiceConfig{PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: strings.TrimSpace(voice)}}
			}
			ext.SpeechConfig = sc
		default:
			// Unknown extension keys are ignored by design.
		}
	}

	return ext, nil
}

func invalidExtType(key, want string) error {
	return core.NewInvalidRequestErrorWithParam(
		fmt.Sprintf("gem.%s must be %s", key, want),
		fmt.Sprintf("extensions.gem.%s", key),
	)
}

func anyToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		if math.Trunc(n) != n {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func parseThinkingLevel(raw string) (genai.ThinkingLevel, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	switch s {
	case "LOW":
		return genai.ThinkingLevelLow, nil
	case "MEDIUM":
		return genai.ThinkingLevelMedium, nil
	case "HIGH":
		return genai.ThinkingLevelHigh, nil
	case "MINIMAL":
		return genai.ThinkingLevelMinimal, nil
	default:
		return "", core.NewInvalidRequestErrorWithParam("gem.thinking_level must be one of LOW|MEDIUM|HIGH|MINIMAL", "extensions.gem.thinking_level")
	}
}

func parseMediaResolution(raw string) (genai.MediaResolution, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	switch s {
	case "LOW", "MEDIA_RESOLUTION_LOW":
		return genai.MediaResolutionLow, nil
	case "MEDIUM", "MEDIA_RESOLUTION_MEDIUM":
		return genai.MediaResolutionMedium, nil
	case "HIGH", "MEDIA_RESOLUTION_HIGH":
		return genai.MediaResolutionHigh, nil
	default:
		return "", core.NewInvalidRequestErrorWithParam("gem.media_resolution must be one of LOW|MEDIUM|HIGH", "extensions.gem.media_resolution")
	}
}

func mapRole(role string) (genai.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return genai.RoleUser, nil
	case "assistant":
		return genai.RoleModel, nil
	default:
		return "", fmt.Errorf("unsupported role %q", role)
	}
}

func systemInstructionText(system any) (string, error) {
	switch s := system.(type) {
	case string:
		return s, nil
	case []types.ContentBlock:
		parts := make([]string, 0, len(s))
		for i := range s {
			text, ok := textBlockText(s[i])
			if !ok {
				return "", core.NewInvalidRequestErrorWithParam("system content blocks must be text only", fmt.Sprintf("system[%d]", i))
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	case []any:
		parts := make([]string, 0, len(s))
		for i := range s {
			cb, ok := s[i].(types.ContentBlock)
			if !ok {
				return "", core.NewInvalidRequestErrorWithParam("system content blocks must be text only", fmt.Sprintf("system[%d]", i))
			}
			text, ok := textBlockText(cb)
			if !ok {
				return "", core.NewInvalidRequestErrorWithParam("system content blocks must be text only", fmt.Sprintf("system[%d]", i))
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", core.NewInvalidRequestErrorWithParam("system must be string or text blocks", "system")
	}
}

func textBlockText(block types.ContentBlock) (string, bool) {
	switch b := block.(type) {
	case types.TextBlock:
		return b.Text, true
	case *types.TextBlock:
		if b == nil {
			return "", false
		}
		return b.Text, true
	default:
		return "", false
	}
}

func applyOutputConfig(cfg *genai.GenerateContentConfig, out *types.OutputConfig) error {
	if out == nil {
		return nil
	}
	if len(out.Modalities) > 0 {
		modes := make([]string, 0, len(out.Modalities))
		for i := range out.Modalities {
			switch strings.ToLower(strings.TrimSpace(out.Modalities[i])) {
			case "text":
				modes = append(modes, "TEXT")
			case "image":
				modes = append(modes, "IMAGE")
			case "audio":
				modes = append(modes, "AUDIO")
			case "video":
				return core.NewInvalidRequestErrorWithParam("video output is not implemented for Gemini providers in this version", "output.modalities")
			default:
				return core.NewInvalidRequestErrorWithParam(fmt.Sprintf("unsupported output modality %q", out.Modalities[i]), "output.modalities")
			}
		}
		cfg.ResponseModalities = modes
	}
	if out.Image != nil && strings.TrimSpace(out.Image.Size) != "" {
		size := strings.ToUpper(strings.TrimSpace(out.Image.Size))
		switch size {
		case "1024X1024", "1K":
			size = "1K"
		case "1536X1536", "2K":
			size = "2K"
		case "2048X2048", "4K":
			size = "4K"
		default:
			return core.NewInvalidRequestErrorWithParam("unsupported output.image.size; supported values are 1K, 2K, 4K", "output.image.size")
		}
		cfg.ImageConfig = &genai.ImageConfig{ImageSize: size}
	}
	return nil
}

func applyOutputFormat(cfg *genai.GenerateContentConfig, out *types.OutputFormat) error {
	if out == nil {
		return nil
	}
	if strings.TrimSpace(out.Type) == "" {
		return nil
	}
	if strings.TrimSpace(out.Type) != "json_schema" {
		return core.NewInvalidRequestErrorWithParam("unsupported output_format.type", "output_format.type")
	}
	if out.JSONSchema == nil {
		return core.NewInvalidRequestErrorWithParam("output_format.json_schema is required for json_schema output", "output_format.json_schema")
	}
	cfg.ResponseMIMEType = "application/json"
	cfg.ResponseJsonSchema = jsonSchemaToMap(out.JSONSchema)
	return nil
}

func jsonSchemaToMap(s *types.JSONSchema) map[string]any {
	if s == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	m := map[string]any{}
	if s.Type != "" {
		m["type"] = s.Type
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for k, v := range s.Properties {
			vv := v
			props[k] = jsonSchemaToMap(&vv)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = append([]string(nil), s.Required...)
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		m["enum"] = append([]string(nil), s.Enum...)
	}
	if s.Items != nil {
		m["items"] = jsonSchemaToMap(s.Items)
	}
	if s.AdditionalProperties != nil {
		m["additionalProperties"] = *s.AdditionalProperties
	}
	if len(m) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if m["type"] == "object" {
		if _, ok := m["properties"]; !ok {
			m["properties"] = map[string]any{}
		}
	}
	return m
}

func (p *Provider) translateInputBlock(block types.ContentBlock, ext gemExtensions, totalInlineBytes *int64, toolUseNameByID map[string]string) (*genai.Part, []string, error) {
	warnings := make([]string, 0, 1)
	switch b := block.(type) {
	case types.TextBlock:
		return genai.NewPartFromText(b.Text), warnings, nil
	case *types.TextBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return genai.NewPartFromText(b.Text), warnings, nil
	case types.ThinkingBlock:
		part := genai.NewPartFromText(b.Thinking)
		part.Thought = true
		return part, warnings, nil
	case *types.ThinkingBlock:
		if b == nil {
			return nil, warnings, nil
		}
		part := genai.NewPartFromText(b.Thinking)
		part.Thought = true
		return part, warnings, nil
	case types.ToolUseBlock:
		return encodeToolUseBlock(b)
	case *types.ToolUseBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return encodeToolUseBlock(*b)
	case types.ToolResultBlock:
		return encodeToolResultBlock(b, toolUseNameByID)
	case *types.ToolResultBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return encodeToolResultBlock(*b, toolUseNameByID)
	case types.ImageBlock:
		return p.encodeImageBlock(b, ext, totalInlineBytes)
	case *types.ImageBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return p.encodeImageBlock(*b, ext, totalInlineBytes)
	case types.AudioBlock:
		return encodeAudioBlock(b, ext, totalInlineBytes)
	case *types.AudioBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return encodeAudioBlock(*b, ext, totalInlineBytes)
	case types.VideoBlock:
		return encodeVideoBlock(b, ext, totalInlineBytes)
	case *types.VideoBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return encodeVideoBlock(*b, ext, totalInlineBytes)
	case types.DocumentBlock:
		return encodeDocumentBlock(b, ext, totalInlineBytes)
	case *types.DocumentBlock:
		if b == nil {
			return nil, warnings, nil
		}
		return encodeDocumentBlock(*b, ext, totalInlineBytes)
	default:
		return nil, warnings, core.NewInvalidRequestError("unsupported content block for Gemini provider")
	}
}

func encodeToolUseBlock(b types.ToolUseBlock) (*genai.Part, []string, error) {
	args := copyMap(b.Input)
	if args == nil {
		args = map[string]any{}
	}
	var sigBytes []byte
	if rawSig, ok := args[thoughtSignatureKey]; ok {
		if s, ok := rawSig.(string); ok {
			decoded, err := base64.StdEncoding.DecodeString(s)
			if err == nil {
				sigBytes = decoded
			}
		}
		delete(args, thoughtSignatureKey)
	}
	part := genai.NewPartFromFunctionCall(b.Name, args)
	part.FunctionCall.ID = b.ID
	if len(sigBytes) > 0 {
		part.ThoughtSignature = sigBytes
	}
	return part, nil, nil
}

func encodeToolResultBlock(b types.ToolResultBlock, names map[string]string) (*genai.Part, []string, error) {
	name := names[b.ToolUseID]
	if strings.TrimSpace(name) == "" {
		return nil, nil, core.NewInvalidRequestErrorWithParam("tool_result.tool_use_id did not match a prior tool_use block", "tool_result.tool_use_id")
	}

	blocks := make([]any, 0, len(b.Content))
	textParts := make([]string, 0, len(b.Content))
	for i := range b.Content {
		raw, err := contentBlockToJSON(b.Content[i])
		if err != nil {
			return nil, nil, core.NewInvalidRequestErrorWithParam(err.Error(), fmt.Sprintf("tool_result.content[%d]", i))
		}
		blocks = append(blocks, raw)
		if t, ok := textBlockText(b.Content[i]); ok && strings.TrimSpace(t) != "" {
			textParts = append(textParts, t)
		}
	}
	payload := map[string]any{"blocks": blocks, "text": strings.Join(textParts, "\n")}
	response := map[string]any{"output": payload}
	if b.IsError {
		response["error"] = payload
	}
	part := genai.NewPartFromFunctionResponse(name, response)
	part.FunctionResponse.ID = b.ToolUseID
	return part, nil, nil
}

func contentBlockToJSON(block types.ContentBlock) (any, error) {
	b, err := json.Marshal(block)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Provider) encodeImageBlock(b types.ImageBlock, ext gemExtensions, totalInlineBytes *int64) (*genai.Part, []string, error) {
	warnings := make([]string, 0, 1)
	src := b.Source
	switch strings.ToLower(strings.TrimSpace(src.Type)) {
	case "url":
		mt := strings.TrimSpace(src.MediaType)
		if mt == "" && ext.InferMediaType {
			mt = inferImageMIMETypeFromURL(src.URL)
		}
		if mt == "" {
			return nil, warnings, core.NewInvalidRequestErrorWithParam("image URL input requires source.media_type (or set extensions.gem.infer_media_type=true)", "image.source.media_type")
		}
		mt = strings.ToLower(mt)
		if _, ok := imageMIMETypes[mt]; !ok {
			warnings = append(warnings, fmt.Sprintf("unsupported image MIME type %q passed through to backend", mt))
		}
		if strings.TrimSpace(src.URL) == "" {
			return nil, warnings, core.NewInvalidRequestErrorWithParam("image URL source.url must be set", "image.source.url")
		}
		return genai.NewPartFromURI(src.URL, mt), warnings, nil
	case "base64":
		mt := strings.ToLower(strings.TrimSpace(src.MediaType))
		if mt == "" {
			return nil, warnings, core.NewInvalidRequestErrorWithParam("image base64 input requires source.media_type", "image.source.media_type")
		}
		if _, ok := imageMIMETypes[mt]; !ok {
			warnings = append(warnings, fmt.Sprintf("unsupported image MIME type %q passed through to backend", mt))
		}
		data, err := decodeInlineData(src.Data, mt, "image", ext, totalInlineBytes)
		if err != nil {
			return nil, warnings, err
		}
		return genai.NewPartFromBytes(data, mt), warnings, nil
	default:
		return nil, warnings, core.NewInvalidRequestErrorWithParam("image.source.type must be url or base64", "image.source.type")
	}
}

func encodeAudioBlock(b types.AudioBlock, ext gemExtensions, totalInlineBytes *int64) (*genai.Part, []string, error) {
	warnings := make([]string, 0, 1)
	src := b.Source
	if strings.ToLower(strings.TrimSpace(src.Type)) != "base64" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("audio.source.type must be base64", "audio.source.type")
	}
	mt := strings.ToLower(strings.TrimSpace(src.MediaType))
	if mt == "" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("audio input requires source.media_type", "audio.source.media_type")
	}
	if _, ok := audioMIMETypes[mt]; !ok {
		warnings = append(warnings, fmt.Sprintf("unsupported audio MIME type %q passed through to backend", mt))
	}
	data, err := decodeInlineData(src.Data, mt, "audio", ext, totalInlineBytes)
	if err != nil {
		return nil, warnings, err
	}
	return genai.NewPartFromBytes(data, mt), warnings, nil
}

func encodeVideoBlock(b types.VideoBlock, ext gemExtensions, totalInlineBytes *int64) (*genai.Part, []string, error) {
	warnings := make([]string, 0, 1)
	src := b.Source
	if strings.ToLower(strings.TrimSpace(src.Type)) != "base64" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("video.source.type must be base64", "video.source.type")
	}
	mt := strings.ToLower(strings.TrimSpace(src.MediaType))
	if mt == "" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("video input requires source.media_type", "video.source.media_type")
	}
	if _, ok := videoMIMETypes[mt]; !ok {
		warnings = append(warnings, fmt.Sprintf("unsupported video MIME type %q passed through to backend", mt))
	}
	data, err := decodeInlineData(src.Data, mt, "video", ext, totalInlineBytes)
	if err != nil {
		return nil, warnings, err
	}
	return genai.NewPartFromBytes(data, mt), warnings, nil
}

func encodeDocumentBlock(b types.DocumentBlock, ext gemExtensions, totalInlineBytes *int64) (*genai.Part, []string, error) {
	warnings := make([]string, 0, 1)
	src := b.Source
	if strings.ToLower(strings.TrimSpace(src.Type)) != "base64" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("document.source.type must be base64", "document.source.type")
	}
	mt := strings.ToLower(strings.TrimSpace(src.MediaType))
	if mt == "" {
		return nil, warnings, core.NewInvalidRequestErrorWithParam("document input requires source.media_type", "document.source.media_type")
	}
	if _, ok := documentMIMETypes[mt]; !ok {
		warnings = append(warnings, fmt.Sprintf("unsupported document MIME type %q passed through to backend", mt))
	}
	data, err := decodeInlineData(src.Data, mt, "document", ext, totalInlineBytes)
	if err != nil {
		return nil, warnings, err
	}
	return genai.NewPartFromBytes(data, mt), warnings, nil
}

func decodeInlineData(b64 string, mediaType string, kind string, ext gemExtensions, totalInlineBytes *int64) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, core.NewInvalidRequestErrorWithParam(fmt.Sprintf("invalid base64 %s data", kind), fmt.Sprintf("%s.source.data", kind))
	}
	if int64(len(decoded)) > ext.InlineMediaMaxBytes {
		return nil, core.NewInvalidRequestErrorWithParam(
			fmt.Sprintf("%s inline media exceeds max decoded bytes (%d > %d). Use image URL input for images, or external file URI/upload flow for larger media.", kind, len(decoded), ext.InlineMediaMaxBytes),
			fmt.Sprintf("%s.source.data", kind),
		)
	}
	if totalInlineBytes != nil {
		next := *totalInlineBytes + int64(len(decoded))
		if next > ext.InlineMediaTotalMaxBytes {
			return nil, core.NewInvalidRequestErrorWithParam(
				fmt.Sprintf("total inline media exceeds max decoded bytes (%d > %d). Use image URL input for images, or external file URI/upload flow for larger media.", next, ext.InlineMediaTotalMaxBytes),
				"messages",
			)
		}
		*totalInlineBytes = next
	}
	_ = mediaType
	return decoded, nil
}

func inferImageMIMETypeFromURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	default:
		return ""
	}
}

func (p *Provider) translateTools(input []types.Tool, choice *types.ToolChoice, ext gemExtensions, opts buildRequestOptions) ([]*genai.Tool, *genai.ToolConfig, streamArgsDecision, error) {
	streamArgs := streamArgsDecision{
		enabled:  false,
		explicit: p.mode == backendModeVertex && opts.isStream && ext.StreamFunctionCallArguments != nil,
		auto:     p.mode == backendModeVertex && opts.isStream && ext.StreamFunctionCallArguments == nil,
	}
	if len(input) == 0 {
		return nil, nil, streamArgs, nil
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(input))
	var hasWebSearch bool
	var hasCodeExec bool
	var webSearchCfg *types.WebSearchConfig

	for i := range input {
		tool := input[i]
		switch tool.Type {
		case types.ToolTypeFunction:
			name := strings.TrimSpace(tool.Name)
			if !toolNamePattern.MatchString(name) {
				return nil, nil, streamArgs, core.NewInvalidRequestErrorWithParam(
					fmt.Sprintf("invalid function tool name %q: must match ^[A-Za-z_][A-Za-z0-9_.-]{0,63}$", name),
					fmt.Sprintf("tools[%d].name", i),
				)
			}
			schema := normalizeToolSchema(tool.InputSchema)
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 name,
				Description:          tool.Description,
				ParametersJsonSchema: schema,
			})
		case types.ToolTypeWebSearch:
			hasWebSearch = true
			cfg := decodeWebSearchConfig(tool.Config)
			if cfg != nil {
				webSearchCfg = cfg
			}
		case types.ToolTypeCodeExecution:
			hasCodeExec = true
		case types.ToolTypeWebFetch, types.ToolTypeFileSearch, types.ToolTypeComputerUse, types.ToolTypeTextEditor:
			return nil, nil, streamArgs, core.NewInvalidRequestErrorWithParam(
				fmt.Sprintf("tool type %q is not supported by Gemini providers", tool.Type),
				fmt.Sprintf("tools[%d].type", i),
			)
		default:
			return nil, nil, streamArgs, core.NewInvalidRequestErrorWithParam(
				fmt.Sprintf("unknown tool type %q", tool.Type),
				fmt.Sprintf("tools[%d].type", i),
			)
		}
	}

	out := make([]*genai.Tool, 0, 3)
	if len(decls) > 0 {
		out = append(out, &genai.Tool{FunctionDeclarations: decls})
	}
	if hasWebSearch {
		searchTool := &genai.Tool{}
		useGoogleSearch := p.mode == backendModeVertex && webSearchCfg != nil && len(webSearchCfg.BlockedDomains) > 0
		if useGoogleSearch {
			searchTool.GoogleSearch = &genai.GoogleSearch{ExcludeDomains: append([]string(nil), webSearchCfg.BlockedDomains...)}
		} else {
			retrieval := &genai.GoogleSearchRetrieval{}
			if ext.Grounding != nil {
				dyn := &genai.DynamicRetrievalConfig{}
				if rawMode, ok := ext.Grounding["mode"]; ok {
					if s, ok := rawMode.(string); ok {
						s := strings.ToUpper(strings.TrimSpace(s))
						if s == "MODE_DYNAMIC" || s == "DYNAMIC" {
							dyn.Mode = genai.DynamicRetrievalConfigModeDynamic
						}
					}
				}
				if rawThreshold, ok := ext.Grounding["dynamic_threshold"]; ok {
					if f, ok := toFloat32(rawThreshold); ok {
						dyn.DynamicThreshold = &f
					}
				}
				if dyn.Mode != "" || dyn.DynamicThreshold != nil {
					retrieval.DynamicRetrievalConfig = dyn
				}
			}
			searchTool.GoogleSearchRetrieval = retrieval
		}
		out = append(out, searchTool)
	}
	if hasCodeExec {
		out = append(out, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
	}

	toolCfg := &genai.ToolConfig{}
	if len(decls) > 0 {
		fc := &genai.FunctionCallingConfig{}
		if choice != nil {
			switch strings.ToLower(strings.TrimSpace(choice.Type)) {
			case "", "auto":
				fc.Mode = genai.FunctionCallingConfigModeAuto
			case "any":
				fc.Mode = genai.FunctionCallingConfigModeAny
			case "none":
				fc.Mode = genai.FunctionCallingConfigModeNone
			case "tool":
				name := strings.TrimSpace(choice.Name)
				if name == "" {
					return nil, nil, streamArgs, core.NewInvalidRequestErrorWithParam("tool_choice.name is required when tool_choice.type=tool", "tool_choice.name")
				}
				fc.Mode = genai.FunctionCallingConfigModeValidated
				fc.AllowedFunctionNames = []string{name}
			default:
				return nil, nil, streamArgs, core.NewInvalidRequestErrorWithParam("unsupported tool_choice.type", "tool_choice.type")
			}
		} else {
			fc.Mode = genai.FunctionCallingConfigModeAuto
		}

		if p.mode == backendModeVertex && opts.isStream {
			enable := true
			if ext.StreamFunctionCallArguments != nil {
				enable = *ext.StreamFunctionCallArguments
			} else if opts.autoStreamArgsOverride != nil {
				enable = *opts.autoStreamArgsOverride
			}
			fc.StreamFunctionCallArguments = &enable
			streamArgs.enabled = enable
		}

		toolCfg.FunctionCallingConfig = fc
	}

	if toolCfg.FunctionCallingConfig == nil {
		toolCfg = nil
	}

	return out, toolCfg, streamArgs, nil
}

func normalizeToolSchema(schema *types.JSONSchema) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := jsonSchemaToMap(schema)
	if t, _ := out["type"].(string); t == "" {
		out["type"] = "object"
	}
	if strings.ToLower(fmt.Sprint(out["type"])) == "object" {
		if _, ok := out["properties"]; !ok {
			out["properties"] = map[string]any{}
		}
	}
	return out
}

func decodeWebSearchConfig(v any) *types.WebSearchConfig {
	if v == nil {
		return nil
	}
	if cfg, ok := v.(*types.WebSearchConfig); ok && cfg != nil {
		return cfg
	}
	if cfg, ok := v.(types.WebSearchConfig); ok {
		cp := cfg
		return &cp
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var cfg types.WebSearchConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	return &cfg
}

func toFloat32(v any) (float32, bool) {
	switch x := v.(type) {
	case float64:
		return float32(x), true
	case float32:
		return x, true
	case int:
		return float32(x), true
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 32)
		if err != nil {
			return 0, false
		}
		return float32(f), true
	default:
		return 0, false
	}
}

func copyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func asToolUseBlock(block types.ContentBlock) (types.ToolUseBlock, bool) {
	switch b := block.(type) {
	case types.ToolUseBlock:
		return b, true
	case *types.ToolUseBlock:
		if b != nil {
			return *b, true
		}
	}
	return types.ToolUseBlock{}, false
}
