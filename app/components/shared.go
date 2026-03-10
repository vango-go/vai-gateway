package components

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
)

func RootLayout(ctx vango.Ctx, children vango.Slot) *vango.VNode {
	scripts := RuntimeScripts(ctx)
	if appruntime.Get().Config.DevMode {
		scripts = RuntimeScripts(ctx, WithDebug())
	}
	return Html(
		Head(
			Meta(Charset("utf-8")),
			Meta(Name("viewport"), Content("width=device-width, initial-scale=1")),
			TitleEl(Text("vai-lite gateway")),
			LinkEl(Rel("stylesheet"), Href(ctx.Asset("app.css"))),
		),
		Body(
			Class("beta-app"),
			children,
			scripts,
		),
	)
}

func NotFoundPage() *vango.VNode {
	return Div(
		Class("missing-page"),
		H1(Text("Page not found")),
		P(Text("The route you requested does not exist.")),
		A(Href("/"), Text("Back to home")),
	)
}

func LandingPage() *vango.VNode {
	authEnabled := appruntime.Get().Config.WorkOSEnabled
	return Div(
		Class("landing"),
		Div(
			Class("landing-copy"),
			H1(Text("vai-lite gateway")),
			P(Text("Use VAI-hosted gateway access with wallet-backed credits, or bring your own provider keys through shared Workspace Keys and browser-local BYOK without consuming VAI credits.")),
			Div(
				Class("cta-row"),
				If(authEnabled,
					A(Href("/auth/signup"), Class("btn btn-primary"), Text("Create workspace")),
				),
				If(authEnabled,
					A(Href("/auth/signin"), Class("btn btn-secondary"), Text("Sign in")),
				),
				Unless(authEnabled,
					Span(Class("badge"), Text("WorkOS auth is not configured")),
				),
			),
		),
		Div(
			Class("landing-panel"),
			H2(Text("Private beta scope")),
			Ul(
				Li(Text("Shared organization BYOK provider keys in WorkOS Vault")),
				Li(Text("VAI credits via Stripe one-time top-ups")),
				Li(Text("Stable `/v1/*` gateway for backend clients")),
				Li(Text("Chat UI with markdown streaming island and attachment uploads")),
			),
		),
	)
}

func AppShell(ctx vango.Ctx, actor services.UserIdentity, content any) *vango.VNode {
	return Div(
		Class("page-shell"),
		Header(
			Class("app-header"),
			Div(
				Class("brand"),
				A(Href("/"), Text("vai-lite gateway")),
				If(actor.Name != "",
					Span(Class("brand-subtitle"), Text(actor.Name)),
				),
			),
			Nav(
				Class("top-nav"),
				A(Href("/settings"), Text("Settings")),
				A(Href("/settings/access"), Text("VAI credits")),
				A(Href("/settings/developers"), Text("Developers")),
				A(Href("/settings/keys"), Text("Keys")),
				A(Href("/settings/billing"), Text("Billing")),
				A(Href("/settings/observability"), Text("Observability")),
				If(appruntime.Get().Config.WorkOSEnabled,
					Form(
						Method("POST"),
						Action("/auth/logout"),
						CSRFField(ctx),
						Button(Type("submit"), Class("btn btn-ghost"), Text("Sign out")),
					),
				),
			),
		),
		content,
	)
}

func SettingsLayout(ctx vango.Ctx, actor services.UserIdentity, active string, children vango.Slot) *vango.VNode {
	return AppShell(ctx, actor,
		Div(
			Class("settings-grid"),
			SettingsNav(active),
			children,
		),
	)
}

func SettingsNav(active string) *vango.VNode {
	link := func(href, label string) *vango.VNode {
		className := "settings-link"
		if href == active {
			className += " settings-link-active"
		}
		return A(Href(href), Class(className), Text(label))
	}
	return Nav(
		Class("settings-nav"),
		link("/settings", "Overview"),
		link("/settings/access", "VAI-hosted"),
		link("/settings/developers", "Developers"),
		link("/settings/keys", "Workspace Keys"),
		link("/settings/billing", "Billing"),
		link("/settings/observability", "Observability"),
	)
}

func Sidebar(actor services.UserIdentity, conversations []services.Conversation, activeConversationID string, creating bool, onNewConversation func()) *vango.VNode {
	return Aside(
		Class("chat-sidebar"),
		Button(
			Type("button"),
			Class("btn btn-primary sidebar-create"),
			Disabled(creating),
			OnClick(onNewConversation),
			Text("New chat"),
		),
		Div(
			Class("conversation-list"),
			RangeKeyed(conversations,
				func(item services.Conversation) any { return item.ID },
				func(item services.Conversation) *vango.VNode {
					className := "conversation-link"
					if item.ID == activeConversationID {
						className += " conversation-link-active"
					}
					return A(
						Href("/chat/"+item.ID),
						Class(className),
						Strong(Text(item.Title)),
						Span(Text(item.Model)),
					)
				},
			),
		),
		Div(
			Class("sidebar-footer"),
			Span(Text(actor.Email)),
		),
	)
}

func PageErrorPanel(err error) *vango.VNode {
	return Section(
		Class("settings-panel"),
		H1(Text("Something failed")),
		P(Text(err.Error())),
	)
}

func LoadingPanel(copy string) *vango.VNode {
	return Section(
		Class("settings-panel"),
		H1(Text(copy)),
	)
}

func EmptyState(title, body string, actions vango.Slot) *vango.VNode {
	hasActions := len(actions.ChildrenNodes()) > 0
	return Div(
		Class("empty-state"),
		H1(Text(title)),
		P(Text(body)),
		If(hasActions,
			Div(
				Class("cta-row"),
				actions,
			),
		),
	)
}

func PostForm(ctx vango.Ctx, action string, children vango.Slot) *vango.VNode {
	return Form(
		Method("POST"),
		Action(action),
		CSRFField(ctx),
		children,
	)
}

func PostFormClass(ctx vango.Ctx, action, className string, children vango.Slot) *vango.VNode {
	return Form(
		Method("POST"),
		Action(action),
		Class(className),
		CSRFField(ctx),
		children,
	)
}

func centsLabel(amount int64) string {
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	return fmt.Sprintf("%s$%.2f", sign, float64(amount)/100)
}

func compactStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	var prev string
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || item == prev {
			continue
		}
		out = append(out, item)
		prev = item
	}
	return out
}

func keySourceLabel(source services.KeySource) string {
	switch source {
	case services.KeySourcePlatformHosted:
		return "VAI-hosted"
	case services.KeySourceCustomerBYOKBrowser:
		return "Browser BYOK"
	case services.KeySourceCustomerBYOKVault:
		return "Workspace key"
	case services.KeySourceCustomerBYOKExternal:
		return "External BYOK"
	default:
		return string(source)
	}
}

func accessCredentialLabel(credential services.AccessCredential) string {
	switch credential {
	case services.AccessCredentialGatewayAPIKey:
		return "gateway API key"
	case services.AccessCredentialSessionAuth:
		return "session auth"
	default:
		return string(credential)
	}
}

func modelOptions(current string) []map[string]any {
	models := make([]string, 0, len(appruntime.Get().Services.Pricing.ByModel)+1)
	for model := range appruntime.Get().Services.Pricing.ByModel {
		models = append(models, model)
	}
	if current != "" {
		models = append(models, current)
	}
	sort.Strings(models)
	models = compactStrings(models)
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		_, hostedAvailable := appruntime.Get().Services.Pricing.HostedModel(model)
		out = append(out, map[string]any{
			"id":              model,
			"label":           model,
			"selected":        model == current,
			"hostedAvailable": hostedAvailable,
		})
	}
	return out
}

func hostedModelOptions(current string) []map[string]any {
	models := appruntime.Get().Services.Pricing.HostedModels()
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		out = append(out, map[string]any{
			"id":                                   model.Model,
			"displayName":                          model.DisplayName,
			"provider":                             model.Provider,
			"retailInputMicrosUSDPer1M":            model.RetailInputMicrosUSDPer1M,
			"retailLongInputMicrosUSDPer1M":        model.RetailLongInputMicrosUSDPer1M,
			"retailAudioInputMicrosUSDPer1M":       model.RetailAudioInputMicrosUSDPer1M,
			"retailCachedInputMicrosUSDPer1M":      model.RetailCachedInputMicrosUSDPer1M,
			"retailLongCachedInputMicrosUSDPer1M":  model.RetailLongCachedInputMicrosUSDPer1M,
			"retailAudioCachedInputMicrosUSDPer1M": model.RetailAudioCachedInputMicrosUSDPer1M,
			"retailCacheWrite5mMicrosUSDPer1M":     model.RetailCacheWrite5mMicrosUSDPer1M,
			"retailCacheWrite1hMicrosUSDPer1M":     model.RetailCacheWrite1hMicrosUSDPer1M,
			"retailCacheStorageMicrosUSDPer1MHour": model.RetailCacheStorageMicrosUSDPer1MHour,
			"retailOutputMicrosUSDPer1M":           model.RetailOutputMicrosUSDPer1M,
			"retailLongOutputMicrosUSDPer1M":       model.RetailLongOutputMicrosUSDPer1M,
			"retailImageOutputMicrosUSDPer1M":      model.RetailImageOutputMicrosUSDPer1M,
			"promptTierThresholdTokens":            model.PromptTierThresholdTokens,
			"minimumChargeCents":                   model.MinimumChargeCents,
			"selected":                             model.Model == current,
		})
	}
	return out
}

func providerHints() []map[string]string {
	return []map[string]string{
		{"provider": "anthropic", "header": "X-Provider-Key-Anthropic"},
		{"provider": "openai", "header": "X-Provider-Key-OpenAI"},
		{"provider": "gem-dev", "header": "X-Provider-Key-Gemini"},
		{"provider": "gem-vert", "header": "X-Provider-Key-VertexAI"},
		{"provider": "groq", "header": "X-Provider-Key-Groq"},
		{"provider": "cerebras", "header": "X-Provider-Key-Cerebras"},
		{"provider": "openrouter", "header": "X-Provider-Key-OpenRouter"},
		{"provider": "cartesia", "header": "X-Provider-Key-Cartesia"},
		{"provider": "elevenlabs", "header": "X-Provider-Key-ElevenLabs"},
		{"provider": "tavily", "header": "X-Provider-Key-Tavily"},
		{"provider": "exa", "header": "X-Provider-Key-Exa"},
		{"provider": "firecrawl", "header": "X-Provider-Key-Firecrawl"},
	}
}

func hostedInputRateLabel(model services.HostedModelPrice) string {
	parts := []string{fmt.Sprintf("Input %s / 1M", microsUSDPer1MLabel(model.RetailInputMicrosUSDPer1M))}
	if model.PromptTierThresholdTokens != nil && model.RetailLongInputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("> %s prompt %s / 1M", compactTokenCount(*model.PromptTierThresholdTokens), microsUSDPer1MLabel(*model.RetailLongInputMicrosUSDPer1M)))
	}
	if model.RetailAudioInputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Audio input %s / 1M", microsUSDPer1MLabel(*model.RetailAudioInputMicrosUSDPer1M)))
	}
	if model.RetailCachedInputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Cached %s / 1M", microsUSDPer1MLabel(*model.RetailCachedInputMicrosUSDPer1M)))
	}
	if model.PromptTierThresholdTokens != nil && model.RetailLongCachedInputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("> %s cached %s / 1M", compactTokenCount(*model.PromptTierThresholdTokens), microsUSDPer1MLabel(*model.RetailLongCachedInputMicrosUSDPer1M)))
	}
	if model.RetailAudioCachedInputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Audio cached %s / 1M", microsUSDPer1MLabel(*model.RetailAudioCachedInputMicrosUSDPer1M)))
	}
	return strings.Join(parts, " • ")
}

func hostedCacheWriteLabel(model services.HostedModelPrice) string {
	parts := make([]string, 0, 3)
	if model.RetailCacheWrite5mMicrosUSDPer1M != nil && model.RetailCacheWrite1hMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Cache write 5m %s / 1M", microsUSDPer1MLabel(*model.RetailCacheWrite5mMicrosUSDPer1M)))
		parts = append(parts, fmt.Sprintf("Cache write 1h %s / 1M", microsUSDPer1MLabel(*model.RetailCacheWrite1hMicrosUSDPer1M)))
	} else if model.RetailCacheWrite5mMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Cache write %s / 1M", microsUSDPer1MLabel(*model.RetailCacheWrite5mMicrosUSDPer1M)))
	} else if model.RetailCacheWrite1hMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Cache write 1h %s / 1M", microsUSDPer1MLabel(*model.RetailCacheWrite1hMicrosUSDPer1M)))
	}
	if model.RetailCacheStorageMicrosUSDPer1MHour != nil {
		parts = append(parts, fmt.Sprintf("Cache storage %s / 1M / hour", microsUSDPer1MLabel(*model.RetailCacheStorageMicrosUSDPer1MHour)))
	}
	if len(parts) == 0 {
		return "No cache tier"
	}
	return strings.Join(parts, " • ")
}

func microsUSDPer1MLabel(micros int64) string {
	return fmt.Sprintf("$%.6g", float64(micros)/1_000_000)
}

func hostedOutputRateLabel(model services.HostedModelPrice) string {
	parts := []string{fmt.Sprintf("Output %s / 1M", microsUSDPer1MLabel(model.RetailOutputMicrosUSDPer1M))}
	if model.PromptTierThresholdTokens != nil && model.RetailLongOutputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("> %s output %s / 1M", compactTokenCount(*model.PromptTierThresholdTokens), microsUSDPer1MLabel(*model.RetailLongOutputMicrosUSDPer1M)))
	}
	if model.RetailImageOutputMicrosUSDPer1M != nil {
		parts = append(parts, fmt.Sprintf("Image output %s / 1M", microsUSDPer1MLabel(*model.RetailImageOutputMicrosUSDPer1M)))
	}
	return strings.Join(parts, " • ")
}

func hostedPricingSummary(model services.HostedModelPrice) string {
	return fmt.Sprintf("%s • %s • %s • Minimum %s", hostedInputRateLabel(model), hostedCacheWriteLabel(model), hostedOutputRateLabel(model), centsLabel(model.MinimumChargeCents))
}

func compactTokenCount(value int) string {
	if value >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	}
	if value >= 1_000 {
		return fmt.Sprintf("%.0fK", float64(value)/1_000)
	}
	return fmt.Sprintf("%d", value)
}

func apiKeyOptions(keys []services.APIKeyRecord, selected string) []any {
	options := make([]any, 0, len(keys))
	for _, item := range keys {
		label := item.Name
		if label == "" {
			label = item.TokenPrefix
		}
		if item.TokenPrefix != "" {
			label += " (" + item.TokenPrefix + ")"
		}
		options = append(options, Option(Value(item.ID), selectedIf(item.ID == selected), Text(label)))
	}
	return options
}

func observabilityRowClass(item services.GatewayRequestListEntry, selectedRequestID string) string {
	className := "card-row"
	if item.RequestID == selectedRequestID {
		className += " observability-row-active"
	}
	return className
}

func observabilityStatusLabel(statusCode int, hasError bool) string {
	if hasError || statusCode >= http.StatusBadRequest {
		return fmt.Sprintf("error (%d)", statusCode)
	}
	return fmt.Sprintf("success (%d)", statusCode)
}

func statusBadgeClass(statusCode int, hasError bool) string {
	className := "status-badge"
	if hasError || statusCode >= http.StatusBadRequest {
		return className + " status-badge-error"
	}
	return className + " status-badge-ok"
}

func observabilityDetailHref(filter services.GatewayRequestFilter, requestID string) string {
	values := buildObservabilityQuery(filter)
	values.Del("run_id")
	values.Set("request_id", requestID)
	encoded := values.Encode()
	if encoded == "" {
		return "/settings/observability"
	}
	return "/settings/observability?" + encoded
}

func buildObservabilityQuery(filter services.GatewayRequestFilter) url.Values {
	values := url.Values{}
	if filter.EndpointKind != "" {
		values.Set("endpoint_kind", filter.EndpointKind)
	}
	if filter.Model != "" {
		values.Set("model", filter.Model)
	}
	if filter.Status != "" {
		values.Set("status", filter.Status)
	}
	if filter.APIKeyID != "" {
		values.Set("api_key_id", filter.APIKeyID)
	}
	if filter.SessionID != "" {
		values.Set("session_id", filter.SessionID)
	}
	if filter.ChainID != "" {
		values.Set("chain_id", filter.ChainID)
	}
	if filter.Hours > 0 {
		values.Set("hours", fmt.Sprintf("%d", filter.Hours))
	}
	return values
}

func selectedIf(ok bool) vango.Attr {
	return Selected(ok)
}

func parseQueryInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func toPrettyJSON(value any) string {
	if value == nil {
		return ""
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func timeLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC822)
}
