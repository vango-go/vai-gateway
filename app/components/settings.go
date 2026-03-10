package components

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
	"github.com/vango-go/vango/setup"
)

type SettingsPageProps struct {
	Actor services.UserIdentity
}

type settingsOverviewData struct {
	Org     *services.Organization
	Balance int64
}

type billingPageData struct {
	Balance int64
	Ledger  []services.WalletEntry
	Usage   []services.UsageEntry
}

type ObservabilityPageProps struct {
	Actor services.UserIdentity
}

type observabilityData struct {
	Logs      []services.GatewayRequestListEntry
	Keys      []services.APIKeyRecord
	Detail    *services.GatewayRequestDetail
	Managed   *services.ManagedObservabilitySnapshot
	RunDetail *services.ManagedObservabilityRunDetail
}

type createAPIKeyFormData struct {
	Name string `form:"name" validate:"required,min=2,max=100"`
}

type storeProviderSecretFormData struct {
	Provider    string `form:"provider" validate:"required"`
	SecretValue string `form:"secret_value" validate:"required,min=8,max=4096"`
}

type billingTopupFormData struct {
	AmountUSD string `form:"amount_usd" validate:"required"`
}

func SettingsOverviewPage(p SettingsPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[SettingsPageProps]) vango.RenderFn {
		props := s.Props()
		data := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) (*settingsOverviewData, error) {
				current := props.Peek().Actor
				org, err := ResolveOrganization(ctx, current)
				if err != nil {
					return nil, err
				}
				balance, err := appruntime.Get().Services.CurrentBalance(ctx, current.OrgID)
				if err != nil {
					return nil, err
				}
				return &settingsOverviewData{Org: org, Balance: balance}, nil
			},
		)

		return func() *vango.VNode {
			return data.Match(
				vango.OnLoadingOrPending[*settingsOverviewData](func() *vango.VNode {
					return LoadingPanel("Loading workspace settings...")
				}),
				vango.OnError[*settingsOverviewData](func(err error) *vango.VNode {
					return PageErrorPanel(err)
				}),
				vango.OnReady(func(model *settingsOverviewData) *vango.VNode {
					return Section(
						Class("settings-panel"),
						H1(Text("Workspace settings")),
						P(Textf("Workspace: %s", model.Org.Name)),
						P(Textf("Default model: %s", model.Org.DefaultModel)),
						P(Textf("VAI credits balance: %s", centsLabel(model.Balance))),
						Ul(
							Li(Textf("VAI-hosted mode enabled: %t", model.Org.HostedUsageEnabled)),
							Li(Textf("Browser BYOK enabled: %t", model.Org.AllowBYOKOverride)),
						),
						Div(
							Class("stack"),
							H2(Text("Execution modes")),
							P(Text("VAI credits are consumed only when requests use VAI-hosted capacity. Workspace provider keys and browser-local BYOK stay customer-managed and never consume VAI credits.")),
							A(Href("/settings/access"), Class("btn btn-secondary"), Text("View hosted access details")),
						),
					)
				}),
			)
		}
	})
}

func AccessPage(p SettingsPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[SettingsPageProps]) vango.RenderFn {
		models := appruntime.Get().Services.Pricing.HostedModels()
		return func() *vango.VNode {
			return Section(
				Class("settings-panel"),
				H1(Text("VAI-hosted access")),
				P(Text("VAI credits apply only when a request uses VAI-managed upstream provider accounts. Customer-owned provider keys, whether stored in Workspace Keys or supplied from the browser, are always BYOK and never consume VAI credits.")),
				Ul(
					Li(Text("Gateway API key without provider headers: VAI-hosted and billable")),
					Li(Text("Gateway API key with provider headers: customer BYOK and non-billable")),
					Li(Text("Chat with workspace key: customer BYOK and non-billable")),
					Li(Text("Chat with browser key: customer BYOK and non-billable")),
				),
				H2(Text("Hosted retail catalog")),
				Div(
					Class("table-stack"),
					RangeKeyed(models,
						func(item services.HostedModelPrice) any { return item.Model },
						func(item services.HostedModelPrice) *vango.VNode {
							return Article(
								Class("card-row"),
								Div(
									Strong(Text(item.DisplayName)),
									P(Textf("%s • %s", item.Provider, item.Model)),
									P(Text(hostedPricingSummary(item))),
								),
								Span(Text(appruntime.Get().Services.Pricing.Version)),
							)
						},
					),
				),
			)
		}
	})
}

func DevelopersPage(p SettingsPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[SettingsPageProps]) vango.RenderFn {
		props := s.Props()
		name := setup.Signal(&s, "")
		nameError := setup.Signal(&s, "")
		createdKey := setup.Signal(&s, (*services.CreatedAPIKey)(nil))
		keys := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) ([]services.APIKeyRecord, error) {
				return appruntime.Get().Services.ListAPIKeys(ctx, orgID)
			},
		)
		lastKeys := setup.Signal(&s, []services.APIKeyRecord(nil))
		keys.OnSuccess(func(items []services.APIKeyRecord) {
			lastKeys.Set(cloneAPIKeyRecords(items))
		})
		createKey := setup.Action(&s,
			func(ctx context.Context, input string) (*services.CreatedAPIKey, error) {
				return appruntime.Get().Services.CreateAPIKey(ctx, props.Peek().Actor, input)
			},
			vango.DropWhileRunning(),
			vango.ActionOnStart(func() {
				createdKey.Set(nil)
				nameError.Set("")
			}),
			vango.ActionOnSuccess(func(result any) {
				created, ok := result.(*services.CreatedAPIKey)
				if !ok || created == nil {
					return
				}
				createdKey.Set(created)
				name.Set("")
				lastKeys.Set(upsertAPIKeyRecord(lastKeys.Get(), created.Record))
				keys.Refetch()
			}),
			vango.ActionOnError(func(err error) {
				if err != nil {
					nameError.Set(err.Error())
				}
			}),
		)
		revokeKey := setup.Action(&s,
			func(ctx context.Context, id string) (string, error) {
				return id, appruntime.Get().Services.RevokeAPIKey(ctx, props.Peek().Actor, id)
			},
			vango.DropWhileRunning(),
			vango.ActionOnSuccess(func(result any) {
				id, _ := result.(string)
				if strings.TrimSpace(id) != "" {
					lastKeys.Set(markAPIKeyRevoked(lastKeys.Get(), id, time.Now().UTC()))
				}
				keys.Refetch()
			}),
		)

		return func() *vango.VNode {
			items, loading, err := resolveAPIKeysData(keys.State(), keys.Data(), keys.Error(), lastKeys.Get())
			if err != nil {
				return PageErrorPanel(fmt.Errorf("load api keys: %w", err))
			}
			if loading {
				return LoadingPanel("Loading API keys...")
			}
			keyResult := createdKey.Get()
			haveCreatedKey := keyResult != nil
			return Section(
				Class("settings-panel"),
				H1(Text("Developer access")),
				P(Text("Create organization-scoped gateway API keys for `/v1/*` clients. Requests without provider headers use billable VAI-hosted access. Requests with provider headers are treated as customer BYOK and remain non-billable.")),
				Form(
					Method("POST"),
					Action("/settings/developers"),
					OnSubmit(vango.PreventDefault(func() {
						nextName := strings.TrimSpace(name.Get())
						switch {
						case nextName == "":
							nameError.Set("API key name is required")
						case len(nextName) < 2:
							nameError.Set("API key name must be at least 2 characters")
						case len(nextName) > 100:
							nameError.Set("API key name must be 100 characters or fewer")
						default:
							createKey.Run(nextName)
						}
					})),
					Class("stack"),
					TestID("developer-key-form"),
					Label(Text("Key name")),
					Input(
						Type("text"),
						Placeholder("Production backend"),
						Value(name.Get()),
						OnInput(func(next string) {
							name.Set(next)
							if nameError.Get() != "" {
								nameError.Set("")
							}
						}),
						TestID("developer-key-name"),
					),
					If(nameError.Get() != "",
						P(Class("error-copy"), Text(nameError.Get())),
					),
					Button(
						Type("submit"),
						Class("btn btn-primary"),
						Disabled(createKey.IsRunning()),
						TestID("developer-key-create"),
						Text("Create API key"),
					),
				),
				If(createKey.IsRunning(),
					P(Text("Creating API key...")),
				),
				func() *vango.VNode {
					if !createKey.IsError() || nameError.Get() != "" || createKey.Error() == nil {
						return nil
					}
					return P(Class("error-copy"), Text(createKey.Error().Error()))
				}(),
				func() *vango.VNode {
					if !haveCreatedKey {
						return nil
					}
					return Article(
						Class("settings-panel inset"),
						H2(Text("API key created")),
						P(Text("Copy this key now. It will not be shown again.")),
						Pre(Class("code-block"), Text(keyResult.Token)),
						P(Textf("Prefix: %s", keyResult.Record.TokenPrefix)),
					)
				}(),
				Div(
					Class("table-stack"),
					RangeKeyed(items,
						func(item services.APIKeyRecord) any { return item.ID },
						func(item services.APIKeyRecord) *vango.VNode {
							status := "active"
							if item.RevokedAt != nil {
								status = "revoked"
							}
							return Article(
								Class("card-row"),
								Div(
									Strong(Text(item.Name)),
									P(Textf("Prefix: %s", item.TokenPrefix)),
									P(Textf("Created: %s", item.CreatedAt.Format(time.RFC822))),
									P(Textf("Status: %s", status)),
								),
								If(item.RevokedAt == nil,
									Button(
										Type("button"),
										Class("btn btn-danger"),
										Disabled(revokeKey.IsRunning()),
										TestID("developer-key-revoke-"+item.ID),
										OnClick(func() {
											revokeKey.Run(item.ID)
										}),
										Text("Revoke"),
									),
								),
							)
						},
					),
				),
				func() *vango.VNode {
					if !revokeKey.IsError() || revokeKey.Error() == nil {
						return nil
					}
					return P(Class("error-copy"), Text(revokeKey.Error().Error()))
				}(),
			)
		}
	})
}

func resolveAPIKeysData(state vango.ResourceState, current []services.APIKeyRecord, currentErr error, last []services.APIKeyRecord) ([]services.APIKeyRecord, bool, error) {
	switch state {
	case vango.Ready:
		return current, false, nil
	case vango.Error:
		if last != nil {
			return cloneAPIKeyRecords(last), false, nil
		}
		return nil, false, currentErr
	default:
		if last != nil {
			return cloneAPIKeyRecords(last), false, nil
		}
		return nil, true, nil
	}
}

func cloneAPIKeyRecords(items []services.APIKeyRecord) []services.APIKeyRecord {
	if items == nil {
		return nil
	}
	out := make([]services.APIKeyRecord, len(items))
	for i := range items {
		out[i] = cloneAPIKeyRecord(items[i])
	}
	return out
}

func cloneAPIKeyRecord(item services.APIKeyRecord) services.APIKeyRecord {
	cloned := item
	if item.LastUsedAt != nil {
		lastUsedAt := *item.LastUsedAt
		cloned.LastUsedAt = &lastUsedAt
	}
	if item.RevokedAt != nil {
		revokedAt := *item.RevokedAt
		cloned.RevokedAt = &revokedAt
	}
	return cloned
}

func upsertAPIKeyRecord(items []services.APIKeyRecord, item services.APIKeyRecord) []services.APIKeyRecord {
	out := make([]services.APIKeyRecord, 0, len(items)+1)
	found := false
	for _, existing := range items {
		if existing.ID == item.ID {
			out = append(out, cloneAPIKeyRecord(item))
			found = true
			continue
		}
		out = append(out, cloneAPIKeyRecord(existing))
	}
	if !found {
		out = append(out, cloneAPIKeyRecord(item))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func markAPIKeyRevoked(items []services.APIKeyRecord, id string, revokedAt time.Time) []services.APIKeyRecord {
	if items == nil {
		return nil
	}
	out := cloneAPIKeyRecords(items)
	for i := range out {
		if out[i].ID != id {
			continue
		}
		ts := revokedAt
		out[i].RevokedAt = &ts
		break
	}
	return out
}

func KeysPage(p SettingsPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[SettingsPageProps]) vango.RenderFn {
		props := s.Props()
		provider := setup.Signal(&s, "")
		secretValue := setup.Signal(&s, "")
		formError := setup.Signal(&s, "")
		secrets := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) ([]services.ProviderSecretRecord, error) {
				return appruntime.Get().Services.ListProviderSecrets(ctx, orgID)
			},
		)
		storeSecret := setup.Action(&s,
			func(ctx context.Context, in storeProviderSecretFormData) (*services.ProviderSecretRecord, error) {
				return appruntime.Get().Services.StoreProviderSecret(ctx, props.Peek().Actor, in.Provider, in.SecretValue)
			},
			vango.DropWhileRunning(),
			vango.ActionOnStart(func() {
				formError.Set("")
			}),
			vango.ActionOnSuccess(func(any) {
				provider.Set("")
				secretValue.Set("")
				secrets.Refetch()
			}),
			vango.ActionOnError(func(err error) {
				if err != nil {
					formError.Set(err.Error())
				}
			}),
		)
		deleteSecret := setup.Action(&s,
			func(ctx context.Context, provider string) (struct{}, error) {
				return struct{}{}, appruntime.Get().Services.DeleteProviderSecret(ctx, props.Peek().Actor, provider)
			},
			vango.DropWhileRunning(),
			vango.ActionOnSuccess(func(any) {
				secrets.Refetch()
			}),
		)

		return func() *vango.VNode {
			return secrets.Match(
				vango.OnLoadingOrPending[[]services.ProviderSecretRecord](func() *vango.VNode {
					return LoadingPanel("Loading workspace keys...")
				}),
				vango.OnError[[]services.ProviderSecretRecord](func(err error) *vango.VNode {
					return PageErrorPanel(fmt.Errorf("load provider secrets: %w", err))
				}),
				vango.OnReady(func(items []services.ProviderSecretRecord) *vango.VNode {
					return Section(
						Class("settings-panel"),
						H1(Text("Workspace provider keys")),
						P(Text("Store organization-shared provider keys in WorkOS Vault for shared BYOK usage. These customer-managed keys can be used from the chat app and gateway flows without consuming VAI credits. Browser BYOK stays local to the chat app and is never persisted server-side.")),
						Form(
							Method("POST"),
							Action("/settings/keys"),
							OnSubmit(vango.PreventDefault(func() {
								nextProvider := strings.TrimSpace(provider.Get())
								nextSecret := strings.TrimSpace(secretValue.Get())
								switch {
								case nextProvider == "":
									formError.Set("provider is required")
								case nextSecret == "":
									formError.Set("secret value is required")
								case len(nextSecret) < 8:
									formError.Set("secret value must be at least 8 characters")
								case len(nextSecret) > 4096:
									formError.Set("secret value must be 4096 characters or fewer")
								default:
									storeSecret.Run(storeProviderSecretFormData{
										Provider:    nextProvider,
										SecretValue: nextSecret,
									})
								}
							})),
							Class("stack"),
							TestID("provider-secret-form"),
							Label(Text("Provider")),
							Select(
								Value(provider.Get()),
								OnChange(func(next string) {
									provider.Set(next)
									if formError.Get() != "" {
										formError.Set("")
									}
								}),
								Option(Value(""), Text("Select a provider")),
								Option(Value("anthropic"), Text("Anthropic")),
								Option(Value("openai"), Text("OpenAI")),
								Option(Value("gem-dev"), Text("Gemini Dev")),
								Option(Value("groq"), Text("Groq")),
								Option(Value("tavily"), Text("Tavily")),
								Option(Value("exa"), Text("Exa")),
								Option(Value("firecrawl"), Text("Firecrawl")),
							),
							Label(Text("Secret value")),
							Input(
								Type("password"),
								Placeholder("sk-..."),
								Value(secretValue.Get()),
								OnInput(func(next string) {
									secretValue.Set(next)
									if formError.Get() != "" {
										formError.Set("")
									}
								}),
								TestID("provider-secret-value"),
							),
							If(formError.Get() != "",
								P(Class("error-copy"), Text(formError.Get())),
							),
							Button(
								Type("submit"),
								Class("btn btn-primary"),
								Disabled(storeSecret.IsRunning()),
								TestID("provider-secret-store"),
								Text("Store or update"),
							),
						),
						If(storeSecret.IsRunning(),
							P(Text("Saving workspace key...")),
						),
						func() *vango.VNode {
							if !storeSecret.IsError() || formError.Get() != "" || storeSecret.Error() == nil {
								return nil
							}
							return P(Class("error-copy"), Text(storeSecret.Error().Error()))
						}(),
						Div(
							Class("table-stack"),
							RangeKeyed(items,
								func(item services.ProviderSecretRecord) any { return item.ID },
								func(item services.ProviderSecretRecord) *vango.VNode {
									return Article(
										Class("card-row"),
										Div(
											Strong(Text(item.Provider)),
											P(Text(item.ObjectName)),
											P(Textf("Updated: %s", item.UpdatedAt.Format(time.RFC822))),
										),
										Button(
											Type("button"),
											Class("btn btn-danger"),
											Disabled(deleteSecret.IsRunning()),
											TestID("provider-secret-delete-"+item.Provider),
											OnClick(func() {
												deleteSecret.Run(item.Provider)
											}),
											Text("Delete"),
										),
									)
								},
							),
						),
						func() *vango.VNode {
							if !deleteSecret.IsError() || deleteSecret.Error() == nil {
								return nil
							}
							return P(Class("error-copy"), Text(deleteSecret.Error().Error()))
						}(),
					)
				}),
			)
		}
	})
}

func BillingPage(p SettingsPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[SettingsPageProps]) vango.RenderFn {
		props := s.Props()
		amountUSD := setup.Signal(&s, "")
		amountError := setup.Signal(&s, "")
		pendingCheckoutURL := setup.Signal(&s, "")
		topup := setup.Action(&s,
			func(ctx context.Context, amountCents int64) (*services.TopupIntent, error) {
				return appruntime.Get().Services.CreateTopupCheckout(ctx, props.Peek().Actor, amountCents)
			},
			vango.DropWhileRunning(),
			vango.ActionOnStart(func() {
				amountError.Set("")
			}),
			vango.ActionOnSuccess(func(result any) {
				intent, ok := result.(*services.TopupIntent)
				if !ok || intent == nil {
					return
				}
				pendingCheckoutURL.Set(intent.CheckoutURL)
			}),
			vango.ActionOnError(func(err error) {
				if err != nil {
					amountError.Set(err.Error())
				}
			}),
		)
		s.Effect(func() vango.Cleanup {
			if checkoutURL := strings.TrimSpace(pendingCheckoutURL.Get()); checkoutURL != "" {
				if ctx := vango.UseCtx(); ctx != nil {
					pendingCheckoutURL.Set("")
					ctx.RedirectExternal(checkoutURL, http.StatusSeeOther)
				}
			}
			return nil
		})
		data := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) (*billingPageData, error) {
				balance, err := appruntime.Get().Services.CurrentBalance(ctx, orgID)
				if err != nil {
					return nil, fmt.Errorf("load balance: %w", err)
				}
				ledger, err := appruntime.Get().Services.Ledger(ctx, orgID)
				if err != nil {
					return nil, fmt.Errorf("load ledger: %w", err)
				}
				usage, err := appruntime.Get().Services.Usage(ctx, orgID)
				if err != nil {
					return nil, fmt.Errorf("load usage: %w", err)
				}
				return &billingPageData{Balance: balance, Ledger: ledger, Usage: usage}, nil
			},
		)

		return func() *vango.VNode {
			return data.Match(
				vango.OnLoadingOrPending[*billingPageData](func() *vango.VNode {
					return LoadingPanel("Loading billing...")
				}),
				vango.OnError[*billingPageData](func(err error) *vango.VNode {
					return PageErrorPanel(err)
				}),
				vango.OnReady(func(model *billingPageData) *vango.VNode {
					return Section(
						Class("settings-panel"),
						H1(Text("VAI credits")),
						P(Textf("Current balance: %s", centsLabel(model.Balance))),
						P(Text("Credits are consumed only for VAI-hosted requests that run on VAI-managed upstream accounts. Workspace keys and browser-local BYOK remain non-billable.")),
						Form(
							Method("POST"),
							Action("/settings/billing"),
							OnSubmit(vango.PreventDefault(func() {
								nextAmount := strings.TrimSpace(amountUSD.Get())
								if nextAmount == "" {
									amountError.Set("enter an amount")
									return
								}
								amountCents, err := parseUSDCentsValue(nextAmount)
								if err != nil {
									amountError.Set(err.Error())
									return
								}
								topup.Run(amountCents)
							})),
							Class("topup-card"),
							TestID("billing-topup-form"),
							Div(
								Class("stack"),
								Label(Text("Add credits (USD)")),
								Input(
									Type("number"),
									Value(amountUSD.Get()),
									OnInput(func(next string) {
										amountUSD.Set(next)
										if amountError.Get() != "" {
											amountError.Set("")
										}
									}),
									Placeholder("25.00"),
									Attr("min", "1.00"),
									Attr("max", "10000.00"),
									Attr("step", "0.01"),
									Attr("inputmode", "decimal"),
									TestID("billing-topup-amount"),
								),
								If(amountError.Get() != "",
									P(Class("error-copy"), Text(amountError.Get())),
								),
								P(Text("Enter any amount between $1.00 and $10,000.00.")),
								Button(
									Type("submit"),
									Class("btn btn-primary"),
									Disabled(topup.IsRunning()),
									TestID("billing-topup-submit"),
									Text("Checkout with Stripe"),
								),
							),
						),
						If(topup.IsRunning(),
							P(Text("Creating Stripe checkout...")),
						),
						func() *vango.VNode {
							if !topup.IsError() || amountError.Get() != "" || topup.Error() == nil {
								return nil
							}
							return P(Class("error-copy"), Text(topup.Error().Error()))
						}(),
						H2(Text("Suggested amounts")),
						Div(
							Class("topup-grid"),
							RangeKeyed(appruntime.Get().Config.TopupOptions,
								func(item int64) any { return fmt.Sprintf("%d", item) },
								func(item int64) *vango.VNode {
									return Button(
										Type("button"),
										Class("topup-card"),
										Disabled(topup.IsRunning()),
										TestID("billing-topup-quick-"+fmt.Sprintf("%d", item)),
										OnClick(func() {
											topup.Run(item)
										}),
										Strong(Text(centsLabel(item))),
										P(Text("Quick add")),
										Span(Class("btn btn-primary"), Text("Add credits")),
									)
								},
							),
						),
						H2(Text("Wallet ledger")),
						Div(
							Class("table-stack"),
							RangeKeyed(model.Ledger,
								func(item services.WalletEntry) any { return item.ID },
								func(item services.WalletEntry) *vango.VNode {
									return Article(
										Class("card-row"),
										Div(
											Strong(Text(item.Description)),
											P(Textf("%s • %s", item.Kind, centsLabel(item.AmountCents))),
										),
										Span(Text(item.CreatedAt.Format(time.RFC822))),
									)
								},
							),
						),
						H2(Text("Usage events")),
						Div(
							Class("table-stack"),
							RangeKeyed(model.Usage,
								func(item services.UsageEntry) any { return item.ID },
								func(item services.UsageEntry) *vango.VNode {
									return Article(
										Class("card-row"),
										Div(
											Strong(Text(item.Model)),
											P(Textf("%s via %s • input %d • output %d • total %d", keySourceLabel(item.KeySource), accessCredentialLabel(item.AccessCredential), item.InputTokens, item.OutputTokens, item.TotalTokens)),
											P(Textf("Estimated cost: %s", centsLabel(item.EstimatedCostCents))),
										),
										Span(Text(item.CreatedAt.Format(time.RFC822))),
									)
								},
							),
						),
					)
				}),
			)
		}
	})
}

func parseUSDCentsValue(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "$")
	raw = strings.ReplaceAll(raw, ",", "")
	if raw == "" {
		return 0, errors.New("enter an amount")
	}
	if strings.HasPrefix(raw, "+") {
		raw = strings.TrimPrefix(raw, "+")
	}
	if strings.HasPrefix(raw, "-") {
		return 0, errors.New("amount must be positive")
	}

	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return 0, errors.New("enter a valid dollar amount")
	}

	dollarsPart := parts[0]
	if dollarsPart == "" {
		dollarsPart = "0"
	}
	for _, ch := range dollarsPart {
		if ch < '0' || ch > '9' {
			return 0, errors.New("enter a valid dollar amount")
		}
	}

	centsPart := "00"
	if len(parts) == 2 {
		switch len(parts[1]) {
		case 0:
			centsPart = "00"
		case 1:
			centsPart = parts[1] + "0"
		case 2:
			centsPart = parts[1]
		default:
			return 0, errors.New("use at most two decimal places")
		}
		for _, ch := range centsPart {
			if ch < '0' || ch > '9' {
				return 0, errors.New("enter a valid dollar amount")
			}
		}
	}

	dollars := int64(0)
	for _, ch := range dollarsPart {
		dollars = dollars*10 + int64(ch-'0')
	}
	cents := int64(0)
	for _, ch := range centsPart {
		cents = cents*10 + int64(ch-'0')
	}
	return dollars*100 + cents, nil
}

type observabilityFilterState struct {
	EndpointKind string
	Model        string
	Status       string
	APIKeyID     string
	SessionID    string
	ChainID      string
	Hours        int
	RequestID    string
	RunID        string
}

func ObservabilityPage(p ObservabilityPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[ObservabilityPageProps]) vango.RenderFn {
		props := s.Props()
		endpointKind := setup.URLParam(&s, "endpoint_kind", "", vango.Replace)
		model := setup.URLParam(&s, "model", "", vango.Replace)
		status := setup.URLParam(&s, "status", "", vango.Replace)
		apiKeyID := setup.URLParam(&s, "api_key_id", "", vango.Replace)
		sessionID := setup.URLParam(&s, "session_id", "", vango.Replace)
		chainID := setup.URLParam(&s, "chain_id", "", vango.Replace)
		hours := setup.URLParam(&s, "hours", 24, vango.Replace)
		requestID := setup.URLParam(&s, "request_id", "", vango.Replace)
		runID := setup.URLParam(&s, "run_id", "", vango.Replace)

		keys := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) ([]services.APIKeyRecord, error) {
				return appruntime.Get().Services.ListAPIKeys(ctx, orgID)
			},
		)
		data := setup.ResourceKeyed(&s,
			func() observabilityFilterState {
				return observabilityFilterState{
					EndpointKind: endpointKind.Get(),
					Model:        model.Get(),
					Status:       status.Get(),
					APIKeyID:     apiKeyID.Get(),
					SessionID:    sessionID.Get(),
					ChainID:      chainID.Get(),
					Hours:        hours.Get(),
					RequestID:    requestID.Get(),
					RunID:        runID.Get(),
				}
			},
			func(ctx context.Context, filterState observabilityFilterState) (*observabilityData, error) {
				filter := services.GatewayRequestFilter{
					EndpointKind: strings.TrimSpace(filterState.EndpointKind),
					Model:        strings.TrimSpace(filterState.Model),
					Status:       strings.TrimSpace(filterState.Status),
					APIKeyID:     strings.TrimSpace(filterState.APIKeyID),
					SessionID:    strings.TrimSpace(filterState.SessionID),
					ChainID:      strings.TrimSpace(filterState.ChainID),
					Hours:        filterState.Hours,
				}
				logs, err := appruntime.Get().Services.ListGatewayRequestLogs(ctx, props.Peek().Actor.OrgID, filter)
				if err != nil {
					return nil, fmt.Errorf("load request logs: %w", err)
				}
				managed, err := appruntime.Get().Services.ListManagedObservability(ctx, props.Peek().Actor.OrgID, filter)
				if err != nil {
					return nil, fmt.Errorf("load managed observability: %w", err)
				}
				var detail *services.GatewayRequestDetail
				if strings.TrimSpace(filterState.RequestID) != "" {
					detail, err = appruntime.Get().Services.GatewayRequestDetail(ctx, props.Peek().Actor.OrgID, filterState.RequestID)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return nil, fmt.Errorf("load request detail: %w", err)
					}
				}
				var runDetail *services.ManagedObservabilityRunDetail
				if strings.TrimSpace(filterState.RunID) != "" {
					runDetail, err = appruntime.Get().Services.ManagedRunDetail(ctx, props.Peek().Actor.OrgID, filterState.RunID)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return nil, fmt.Errorf("load run detail: %w", err)
					}
				}
				keyRows, err := appruntime.Get().Services.ListAPIKeys(ctx, props.Peek().Actor.OrgID)
				if err != nil {
					return nil, fmt.Errorf("load api keys: %w", err)
				}
				return &observabilityData{
					Logs:      logs,
					Keys:      keyRows,
					Detail:    detail,
					Managed:   managed,
					RunDetail: runDetail,
				}, nil
			},
		)

		return func() *vango.VNode {
			currentFilter := services.GatewayRequestFilter{
				EndpointKind: endpointKind.Get(),
				Model:        model.Get(),
				Status:       status.Get(),
				APIKeyID:     apiKeyID.Get(),
				SessionID:    sessionID.Get(),
				ChainID:      chainID.Get(),
				Hours:        hours.Get(),
			}
			selectedRequestID := requestID.Get()
			selectedRunID := runID.Get()
			return data.Match(
				vango.OnLoadingOrPending[*observabilityData](func() *vango.VNode {
					return LoadingPanel("Loading gateway observability...")
				}),
				vango.OnError[*observabilityData](func(err error) *vango.VNode {
					return PageErrorPanel(err)
				}),
				vango.OnReady(func(modelData *observabilityData) *vango.VNode {
					keyOptions := modelData.Keys
					if len(keyOptions) == 0 && keys.IsReady() {
						keyOptions = keys.Data()
					}
					return Section(
						Class("settings-panel"),
						H1(Text("Gateway observability")),
						P(Text("Inspect single `/v1/messages` request logs alongside managed sessions, chains, and runs. Transport is recorded per run, so stateful HTTP/SSE and chain WebSocket activity show up in the same managed history model.")),
						Form(
							Method("GET"),
							Action("/settings/observability"),
							Class("observability-filters"),
							Div(
								Class("stack"),
								Label(Text("Single request type")),
								Select(
									Name("endpoint_kind"),
									Option(Value(""), Text("All single requests")),
									Option(Value(endpointKindMessages), selectedIf(currentFilter.EndpointKind == endpointKindMessages), Text("Messages")),
									Option(Value(endpointKindMessagesSSE), selectedIf(currentFilter.EndpointKind == endpointKindMessagesSSE), Text("Messages stream")),
								),
							),
							Div(
								Class("stack"),
								Label(Text("Status")),
								Select(
									Name("status"),
									Option(Value(""), Text("All statuses")),
									Option(Value("success"), selectedIf(currentFilter.Status == "success"), Text("Success")),
									Option(Value("error"), selectedIf(currentFilter.Status == "error"), Text("Error")),
								),
							),
							Div(
								Class("stack"),
								Label(Text("API key")),
								func() *vango.VNode {
									options := []any{
										Name("api_key_id"),
										Option(Value(""), Text("All API keys")),
									}
									options = append(options, apiKeyOptions(keyOptions, currentFilter.APIKeyID)...)
									return Select(options...)
								}(),
							),
							Div(
								Class("stack"),
								Label(Text("Hours")),
								Input(Type("number"), Name("hours"), Value(fmt.Sprintf("%d", currentFilter.Hours)), Attr("min", "1"), Attr("max", "168")),
							),
							Div(
								Class("stack"),
								Label(Text("Model")),
								Input(Type("text"), Name("model"), Value(currentFilter.Model), Placeholder("oai-resp/gpt-5-mini")),
							),
							Div(
								Class("stack"),
								Label(Text("Session ID")),
								Input(Type("text"), Name("session_id"), Value(currentFilter.SessionID), Placeholder("sess_...")),
							),
							Div(
								Class("stack"),
								Label(Text("Chain ID")),
								Input(Type("text"), Name("chain_id"), Value(currentFilter.ChainID), Placeholder("chain_...")),
							),
							Div(
								Class("observability-filter-actions"),
								Button(Type("submit"), Class("btn btn-primary"), Text("Apply filters")),
								A(Href("/settings/observability"), Class("btn btn-secondary"), Text("Reset")),
							),
						),
						H2(Text("Single requests")),
						If(len(modelData.Logs) == 0,
							Div(Class("empty-state compact"), P(Text("No matching single-request logs found."))),
						),
						Div(
							Class("table-stack observability-list"),
							RangeKeyed(modelData.Logs,
								func(item services.GatewayRequestListEntry) any { return item.RequestID },
								func(item services.GatewayRequestListEntry) *vango.VNode {
									return Article(
										Class(observabilityRowClass(item, selectedRequestID)),
										Div(
											Class("stack"),
											Strong(Text(item.Model)),
											P(Textf("%s • %s • %d ms", item.EndpointKind, keySourceLabel(item.KeySource), item.DurationMS)),
											P(Textf("API key %s • request %s", firstNonEmpty(item.GatewayAPIKeyName, item.GatewayAPIKeyPref), item.RequestID)),
											If(item.SessionID != "",
												P(Textf("Session %s • Chain %s", item.SessionID, item.ChainID)),
											),
											If(item.SessionID == "",
												P(Textf("Chain %s", item.ChainID)),
											),
											If(item.ParentRequestID != "",
												P(Textf("Parent %s", item.ParentRequestID)),
											),
											If(item.ErrorSummary != "",
												P(Class("error-copy"), Text(item.ErrorSummary)),
											),
										),
										Div(
											Class("stack observability-row-meta"),
											Span(Class(statusBadgeClass(item.StatusCode, item.ErrorSummary != "")), Text(observabilityStatusLabel(item.StatusCode, item.ErrorSummary != ""))),
											Span(Text(item.CompletedAt.Format(time.RFC822))),
											A(Href(observabilityDetailHref(currentFilter, item.RequestID)), Class("btn btn-secondary"), Text("Inspect")),
										),
									)
								},
							),
						),
						H2(Text("Sessions & chains")),
						If(modelData.Managed == nil || (len(modelData.Managed.Sessions) == 0 && len(modelData.Managed.UnsessionedChains) == 0),
							Div(Class("empty-state compact"), P(Text("No matching managed sessions or chains found."))),
						),
						If(modelData.Managed != nil,
							RenderManagedObservability(modelData.Managed, currentFilter, selectedRunID),
						),
						If(modelData.Detail != nil,
							RenderObservabilityDetail(modelData.Detail),
						),
						If(modelData.RunDetail != nil,
							RenderManagedRunDetail(modelData.RunDetail),
						),
					)
				}),
			)
		}
	})
}

const (
	endpointKindMessages    = "messages"
	endpointKindMessagesSSE = "messages_stream"
)
