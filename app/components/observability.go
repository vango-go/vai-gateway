package components

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
)

func RenderObservabilityDetail(detail *services.GatewayRequestDetail) *vango.VNode {
	if detail == nil {
		return nil
	}
	metaLines := []string{
		fmt.Sprintf("Request ID: %s", detail.RequestID),
		fmt.Sprintf("Endpoint: %s (%s)", detail.EndpointKind, detail.Path),
		fmt.Sprintf("Model: %s", detail.Model),
		fmt.Sprintf("Provider: %s", firstNonEmpty(detail.Provider, "n/a")),
		fmt.Sprintf("Access credential: %s", accessCredentialLabel(detail.AccessCredential)),
		fmt.Sprintf("Key source: %s", keySourceLabel(detail.KeySource)),
		fmt.Sprintf("Status: %s", observabilityStatusLabel(detail.StatusCode, detail.ErrorSummary != "")),
		fmt.Sprintf("Duration: %d ms", detail.DurationMS),
		fmt.Sprintf("API key: %s (%s)", firstNonEmpty(detail.GatewayAPIKeyName, detail.GatewayAPIKeyPrefix), detail.GatewayAPIKeyPrefix),
	}
	if detail.SessionID != "" {
		metaLines = append(metaLines, fmt.Sprintf("Session: %s", detail.SessionID))
	}
	metaLines = append(metaLines, fmt.Sprintf("Chain: %s", detail.ChainID))
	if detail.ParentRequestID != "" {
		metaLines = append(metaLines, fmt.Sprintf("Parent request: %s", detail.ParentRequestID))
	}
	if detail.InputContextFingerprint != "" {
		metaLines = append(metaLines, fmt.Sprintf("Input fingerprint: %s", detail.InputContextFingerprint))
	}
	if detail.OutputContextFingerprint != "" {
		metaLines = append(metaLines, fmt.Sprintf("Output fingerprint: %s", detail.OutputContextFingerprint))
	}
	if detail.ErrorSummary != "" {
		metaLines = append(metaLines, fmt.Sprintf("Error: %s", detail.ErrorSummary))
	}

	return Div(
		Class("observability-detail"),
		H2(Text("Request detail")),
		Div(
			Class("detail-grid"),
			Article(
				Class("settings-panel inset"),
				H3(Text("Correlation")),
				Pre(Class("code-block"), Text(strings.Join(metaLines, "\n"))),
			),
			Article(
				Class("settings-panel inset"),
				H3(Text("Request summary")),
				Pre(Class("code-block"), Text(detail.RequestSummary)),
			),
			Article(
				Class("settings-panel inset"),
				H3(Text("Response summary")),
				Pre(Class("code-block"), Text(detail.ResponseSummary)),
			),
			Article(
				Class("settings-panel inset"),
				H3(Text("Request body")),
				Pre(Class("code-block code-block-tall"), Text(detail.RequestBody)),
			),
			Article(
				Class("settings-panel inset"),
				H3(Text("Response body")),
				Pre(Class("code-block code-block-tall"), Text(detail.ResponseBody)),
			),
			If(detail.ErrorJSON != "" && detail.ErrorJSON != "{}",
				Article(
					Class("settings-panel inset"),
					H3(Text("Error JSON")),
					Pre(Class("code-block"), Text(detail.ErrorJSON)),
				),
			),
		),
		If(detail.RunTrace != nil,
			Div(
				Class("stack"),
				H2(Text("Run trace")),
				Article(
					Class("settings-panel inset"),
					H3(Text("Run summary")),
					Pre(Class("code-block"), Text(fmt.Sprintf("Stop reason: %s\nTurns: %d\nTool calls: %d\nRun config: %s\nUsage: %s",
						detail.RunTrace.StopReason,
						detail.RunTrace.TurnCount,
						detail.RunTrace.ToolCallCount,
						detail.RunTrace.RunConfig,
						detail.RunTrace.Usage,
					))),
				),
				Div(
					Class("table-stack"),
					RangeKeyed(detail.RunTrace.Steps,
						func(item services.GatewayRunStepDetail) any { return item.ID },
						func(item services.GatewayRunStepDetail) *vango.VNode {
							return Article(
								Class("settings-panel inset"),
								H3(Textf("Step %d", item.StepIndex)),
								P(Textf("Duration: %d ms", item.DurationMS)),
								Pre(Class("code-block"), Text(item.ResponseSummary)),
								If(item.ResponseBody != "",
									Pre(Class("code-block code-block-tall"), Text(item.ResponseBody)),
								),
								If(len(item.ToolCalls) > 0,
									Div(
										Class("stack"),
										H4(Text("Tool calls")),
										RangeKeyed(item.ToolCalls,
											func(tool services.GatewayRunToolCall) any { return tool.ID },
											func(tool services.GatewayRunToolCall) *vango.VNode {
												return Article(
													Class("card-row"),
													Div(
														Strong(Text(tool.Name)),
														P(Textf("Tool call: %s", tool.ToolCallID)),
														If(tool.ErrorSummary != "",
															P(Class("error-copy"), Text(tool.ErrorSummary)),
														),
													),
													Div(
														Class("stack observability-tool-payloads"),
														Pre(Class("code-block"), Text(tool.InputJSON)),
														If(tool.ResultBody != "",
															Pre(Class("code-block"), Text(tool.ResultBody)),
														),
													),
												)
											},
										),
									),
								),
							)
						},
					),
				),
			),
		),
	)
}

func RenderManagedObservability(data *services.ManagedObservabilitySnapshot, filter services.GatewayRequestFilter, selectedRunID string) *vango.VNode {
	if data == nil {
		return nil
	}
	return Div(
		Class("stack"),
		If(len(data.Sessions) > 0,
			Div(
				Class("stack"),
				H3(Text("Sessions")),
				RangeKeyed(data.Sessions,
					func(item services.ManagedObservabilitySession) any { return item.Session.ID },
					func(item services.ManagedObservabilitySession) *vango.VNode {
						meta := []string{
							fmt.Sprintf("Session: %s", firstNonEmpty(item.Session.ExternalSessionID, item.Session.ID)),
							fmt.Sprintf("Chains: %d", item.ChainCount),
							fmt.Sprintf("Runs: %d", item.RunCount),
							fmt.Sprintf("Latest activity: %s", item.LatestActivity.Format(time.RFC822)),
						}
						return Article(
							Class("settings-panel inset stack"),
							H4(Text(firstNonEmpty(item.Session.ExternalSessionID, item.Session.ID))),
							Pre(Class("code-block"), Text(strings.Join(meta, "\n"))),
							RangeKeyed(item.Chains,
								func(chain services.ManagedObservabilityChain) any { return chain.Chain.ID },
								func(chain services.ManagedObservabilityChain) *vango.VNode {
									return renderManagedChainCard(chain, filter, selectedRunID)
								},
							),
						)
					},
				),
			),
		),
		If(len(data.UnsessionedChains) > 0,
			Div(
				Class("stack"),
				H3(Text("Unsessioned chains")),
				RangeKeyed(data.UnsessionedChains,
					func(item services.ManagedObservabilityChain) any { return item.Chain.ID },
					func(item services.ManagedObservabilityChain) *vango.VNode {
						return renderManagedChainCard(item, filter, selectedRunID)
					},
				),
			),
		),
	)
}

func RenderManagedRunDetail(detail *services.ManagedObservabilityRunDetail) *vango.VNode {
	if detail == nil || detail.Record == nil {
		return nil
	}
	metaLines := []string{
		fmt.Sprintf("Run ID: %s", detail.Run.ID),
		fmt.Sprintf("Chain: %s", detail.Run.ChainID),
		fmt.Sprintf("Session: %s", firstNonEmpty(detail.Run.ExternalSessionID, detail.Run.SessionID, "n/a")),
		fmt.Sprintf("Model: %s", detail.Run.Model),
		fmt.Sprintf("Provider: %s", firstNonEmpty(detail.Run.Provider, "n/a")),
		fmt.Sprintf("Transport: %s", firstNonEmpty(detail.Run.Transport, "unknown")),
		fmt.Sprintf("Endpoint: %s", firstNonEmpty(detail.Run.EndpointKind, "n/a")),
		fmt.Sprintf("Key source: %s", keySourceLabel(detail.Run.KeySource)),
		fmt.Sprintf("Access credential: %s", accessCredentialLabel(detail.Run.AccessCredential)),
		fmt.Sprintf("Status: %s", managedRunStatusLabel(detail.Run.Status)),
		fmt.Sprintf("Duration: %d ms", detail.Run.DurationMS),
	}
	if detail.Run.GatewayAPIKeyID != "" {
		metaLines = append(metaLines, fmt.Sprintf("API key: %s (%s)", firstNonEmpty(detail.Run.GatewayAPIKeyName, detail.Run.GatewayAPIKeyPref), detail.Run.GatewayAPIKeyPref))
	}
	if detail.Run.ErrorSummary != "" {
		metaLines = append(metaLines, fmt.Sprintf("Error: %s", detail.Run.ErrorSummary))
	}

	return Div(
		Class("observability-detail"),
		H2(Text("Run detail")),
		Div(
			Class("detail-grid"),
			Article(
				Class("settings-panel inset"),
				H3(Text("Correlation")),
				Pre(Class("code-block"), Text(strings.Join(metaLines, "\n"))),
			),
			If(detail.EffectiveRequest != nil,
				Article(
					Class("settings-panel inset"),
					H3(Text("Effective request")),
					Pre(Class("code-block code-block-tall"), Text(mustPrettyJSON(detail.EffectiveRequest))),
				),
			),
			Article(
				Class("settings-panel inset"),
				H3(Text("Timeline")),
				Div(
					Class("stack"),
					RangeKeyed(detail.Timeline,
						func(item types.RunTimelineItem) any { return item.ID },
						func(item types.RunTimelineItem) *vango.VNode {
							return Article(
								Class("card-row"),
								Div(
									Strong(Text(strings.ToUpper(item.Kind))),
									If(item.Tool != nil,
										P(Textf("%s %s", item.Tool.Name, mustPrettyJSON(item.Tool.Args))),
									),
									If(len(item.Content) > 0,
										Pre(Class("code-block"), Text(mustPrettyJSON(item.Content))),
									),
								),
								Div(
									Class("stack observability-row-meta"),
									Span(Textf("step %d", item.StepIndex)),
									Span(Text(item.CreatedAt.Format(time.RFC822))),
								),
							)
						},
					),
				),
			),
		),
	)
}

func renderManagedChainCard(chain services.ManagedObservabilityChain, filter services.GatewayRequestFilter, selectedRunID string) *vango.VNode {
	meta := []string{
		fmt.Sprintf("Chain: %s", chain.Chain.ID),
		fmt.Sprintf("Runs: %d", chain.RunCount),
		fmt.Sprintf("Latest activity: %s", chain.LatestActivity.Format(time.RFC822)),
	}
	if len(chain.Models) > 0 {
		meta = append(meta, fmt.Sprintf("Models: %s", strings.Join(chain.Models, ", ")))
	}
	if len(chain.Transports) > 0 {
		meta = append(meta, fmt.Sprintf("Transports: %s", strings.Join(chain.Transports, ", ")))
	}
	return Article(
		Class("settings-panel inset stack"),
		H5(Text(chain.Chain.ID)),
		Pre(Class("code-block"), Text(strings.Join(meta, "\n"))),
		Div(
			Class("table-stack observability-list"),
			RangeKeyed(chain.Runs,
				func(item services.ManagedObservabilityRun) any { return item.ID },
				func(item services.ManagedObservabilityRun) *vango.VNode {
					return Article(
						Class(managedRunRowClass(item, selectedRunID)),
						Div(
							Class("stack"),
							Strong(Text(item.Model)),
							P(Textf("%s • %s • %d ms", firstNonEmpty(item.EndpointKind, item.Transport), keySourceLabel(item.KeySource), item.DurationMS)),
							P(Textf("Run %s • %s", item.ID, firstNonEmpty(item.Transport, "unknown"))),
							If(item.ExternalSessionID != "",
								P(Textf("Session %s • Chain %s", item.ExternalSessionID, item.ChainID)),
							),
							If(item.ExternalSessionID == "",
								P(Textf("Chain %s", item.ChainID)),
							),
							If(item.ErrorSummary != "",
								P(Class("error-copy"), Text(item.ErrorSummary)),
							),
						),
						Div(
							Class("stack observability-row-meta"),
							Span(Class(managedRunStatusBadgeClass(item.Status)), Text(managedRunStatusLabel(item.Status))),
							Span(Text(item.StartedAt.Format(time.RFC822))),
							A(Href(observabilityRunDetailHref(filter, item.ID)), Class("btn btn-secondary"), Text("Inspect")),
						),
					)
				},
			),
		),
	)
}

func managedRunRowClass(item services.ManagedObservabilityRun, selectedRunID string) string {
	className := "card-row"
	if item.ID == selectedRunID {
		className += " observability-row-active"
	}
	return className
}

func managedRunStatusLabel(status types.RunStatus) string {
	if strings.TrimSpace(string(status)) == "" {
		return "unknown"
	}
	return strings.ReplaceAll(string(status), "_", " ")
}

func managedRunStatusBadgeClass(status types.RunStatus) string {
	className := "status-badge"
	switch status {
	case types.RunStatusCompleted:
		return className + " status-badge-ok"
	default:
		return className + " status-badge-error"
	}
}

func observabilityRunDetailHref(filter services.GatewayRequestFilter, runID string) string {
	values := buildObservabilityQuery(filter)
	values.Del("request_id")
	values.Set("run_id", runID)
	encoded := values.Encode()
	if encoded == "" {
		return "/settings/observability"
	}
	return "/settings/observability?" + encoded
}

func mustPrettyJSON(value any) string {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(raw)
}
