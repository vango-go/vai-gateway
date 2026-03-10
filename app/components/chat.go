package components

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/chatruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/sdk"
	"github.com/vango-go/vai-lite/pkg/core/types"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
	"github.com/vango-go/vango/setup"
)

type ChatPageProps struct {
	Actor          services.UserIdentity
	ConversationID string
}

type chatPageData struct {
	Org             *services.Organization
	Conversations   []services.Conversation
	Detail          *services.ManagedConversationDetail
	ProviderSecrets []services.ProviderSecretRecord
	CurrentBalance  chatBalanceState
	Messages        []map[string]any
}

type chatStaticData struct {
	Org             *services.Organization
	ProviderSecrets []services.ProviderSecretRecord
}

type chatConversationData struct {
	Detail   *services.ManagedConversationDetail
	Messages []map[string]any
}

type chatSubmitResult struct {
	KeySource services.KeySource
	ChainID   string
	Transport string
}

type chatBalanceState struct {
	Status              string
	CurrentBalanceCents int64
}

const (
	chatBalanceStatusLoading     = "loading"
	chatBalanceStatusReady       = "ready"
	chatBalanceStatusUnavailable = "unavailable"
)

func ChatPage(p ChatPageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[ChatPageProps]) vango.RenderFn {
		pageCtx := s.Ctx()
		props := s.Props()
		pendingConversationID := setup.Signal(&s, "")

		staticData := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) (*chatStaticData, error) {
				current := props.Peek()
				org, err := ResolveOrganization(ctx, current.Actor)
				if err != nil {
					return nil, fmt.Errorf("load org: %w", err)
				}
				providerSecrets, err := appruntime.Get().Services.ListProviderSecrets(ctx, orgID)
				if err != nil {
					return nil, fmt.Errorf("load provider secrets: %w", err)
				}
				return &chatStaticData{
					Org:             org,
					ProviderSecrets: providerSecrets,
				}, nil
			},
		)

		conversations := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) ([]services.Conversation, error) {
				items, err := appruntime.Get().Services.ListManagedConversations(ctx, orgID)
				if err != nil {
					return nil, fmt.Errorf("load conversations: %w", err)
				}
				return items, nil
			},
		)
		lastConversations := setup.Signal(&s, []services.Conversation(nil))
		conversations.OnSuccess(func(items []services.Conversation) {
			lastConversations.Set(append([]services.Conversation{}, items...))
		})

		conversationData := setup.ResourceKeyed(&s,
			func() string {
				current := props.Get()
				return current.Actor.OrgID + "|" + current.ConversationID
			},
			func(ctx context.Context, _ string) (*chatConversationData, error) {
				current := props.Peek()
				detail, err := appruntime.Get().Services.ManagedConversation(ctx, current.Actor.OrgID, current.ConversationID)
				if err != nil {
					return nil, fmt.Errorf("load conversation: %w", err)
				}
				messageViews, err := chatMessagesViewManaged(ctx, current.Actor.OrgID, detail)
				if err != nil {
					return nil, fmt.Errorf("prepare messages: %w", err)
				}
				return &chatConversationData{
					Detail:   detail,
					Messages: messageViews,
				}, nil
			},
		)
		lastConversationData := setup.Signal(&s, (*chatConversationData)(nil))
		conversationData.OnSuccess(func(data *chatConversationData) {
			lastConversationData.Set(data)
		})

		balanceData := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) (chatBalanceState, error) {
				balanceState := chatBalanceState{Status: chatBalanceStatusReady}
				balance, err := appruntime.Get().Services.CurrentBalance(ctx, orgID)
				if err != nil {
					balanceState.Status = chatBalanceStatusUnavailable
					return balanceState, nil
				}
				balanceState.CurrentBalanceCents = balance
				return balanceState, nil
			},
		)
		lastBalanceData := setup.Signal(&s, chatBalanceState{Status: chatBalanceStatusLoading})
		balanceData.OnSuccess(func(state chatBalanceState) {
			lastBalanceData.Set(state)
		})

		s.Effect(func() vango.Cleanup {
			if conversationID := pendingConversationID.Get(); conversationID != "" {
				if ctx := vango.UseCtx(); ctx != nil {
					pendingConversationID.Set("")
					ctx.Navigate("/chat/" + conversationID)
				}
			}
			return nil
		})

		var activeRunMu sync.Mutex
		var activeRunRequestID string
		var activeRunCancel func()
		var gatewayMu sync.Mutex
		var gatewayProxy *chatruntime.InProcessGateway
		var wsChain *vai.Chain
		var wsChainConversationID string
		var wsCredentialSignature string
		var trackedChainID string
		var trackedResumeToken string

		ensureGatewayProxy := func() (*chatruntime.InProcessGateway, error) {
			gatewayMu.Lock()
			defer gatewayMu.Unlock()
			if gatewayProxy != nil {
				return gatewayProxy, nil
			}
			proxy, err := chatruntime.NewInProcessGateway(appruntime.Get().Gateway)
			if err != nil {
				return nil, err
			}
			gatewayProxy = proxy
			return gatewayProxy, nil
		}
		closeWSChain := func() {
			gatewayMu.Lock()
			chain := wsChain
			wsChain = nil
			wsChainConversationID = ""
			wsCredentialSignature = ""
			gatewayMu.Unlock()
			if chain != nil {
				_ = chain.Close()
			}
		}
		setTrackedChain := func(chainID, resumeToken string) {
			gatewayMu.Lock()
			trackedChainID = strings.TrimSpace(chainID)
			if trimmed := strings.TrimSpace(resumeToken); trimmed != "" {
				trackedResumeToken = trimmed
			} else if trackedChainID == "" {
				trackedResumeToken = ""
			}
			gatewayMu.Unlock()
		}
		currentTrackedChain := func() (string, string) {
			gatewayMu.Lock()
			defer gatewayMu.Unlock()
			return trackedChainID, trackedResumeToken
		}
		s.Effect(func() vango.Cleanup {
			return func() {
				closeWSChain()
				gatewayMu.Lock()
				proxy := gatewayProxy
				gatewayProxy = nil
				gatewayMu.Unlock()
				if proxy != nil {
					proxy.Close()
				}
			}
		})

		setActiveRun := func(requestID string, cancel func()) {
			activeRunMu.Lock()
			activeRunRequestID = requestID
			activeRunCancel = cancel
			activeRunMu.Unlock()
		}
		clearActiveRun := func(requestID string) {
			activeRunMu.Lock()
			if activeRunRequestID == requestID {
				activeRunRequestID = ""
				activeRunCancel = nil
			}
			activeRunMu.Unlock()
		}
		stopActiveRun := func(requestID string) bool {
			activeRunMu.Lock()
			cancel := activeRunCancel
			activeRequestID := activeRunRequestID
			if cancel == nil {
				activeRunMu.Unlock()
				return false
			}
			if requestID != "" && activeRequestID != requestID {
				activeRunMu.Unlock()
				return false
			}
			activeRunCancel = nil
			activeRunRequestID = ""
			activeRunMu.Unlock()
			cancel()
			return true
		}

		uploadIntent := setup.Action(&s,
			func(ctx context.Context, in chatUploadIntentCommand) (struct{}, error) {
				proxy, err := ensureGatewayProxy()
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_intent_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				client, err := proxy.Client(props.Peek().Actor, nil)
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_intent_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				intent, err := client.Assets.CreateUploadIntent(ctx, &types.AssetUploadIntentRequest{
					Filename:    in.Filename,
					ContentType: in.ContentType,
					SizeBytes:   in.SizeBytes,
					Metadata: map[string]any{
						"external_session_id": props.Peek().ConversationID,
						"source":              "platform_chat",
					},
				})
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_intent_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
					Type:      "upload_intent_ready",
					RequestID: in.RequestID,
					Intent:    intent,
				})
				return struct{}{}, nil
			},
			vango.Queue(8),
		)

		uploadClaim := setup.Action(&s,
			func(ctx context.Context, in chatUploadClaimCommand) (struct{}, error) {
				proxy, err := ensureGatewayProxy()
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_claim_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				client, err := proxy.Client(props.Peek().Actor, nil)
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_claim_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				asset, err := client.Assets.Claim(ctx, &types.AssetClaimRequest{
					IntentToken: in.IntentToken,
					Filename:    in.Filename,
					ContentType: in.ContentType,
					Metadata: map[string]any{
						"external_session_id": props.Peek().ConversationID,
						"source":              "platform_chat",
					},
				})
				if err != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_claim_error",
						RequestID: in.RequestID,
						Error:     err.Error(),
					})
					return struct{}{}, err
				}
				signed, signErr := client.Assets.Sign(ctx, asset.ID)
				if signErr != nil {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_claim_error",
						RequestID: in.RequestID,
						Error:     signErr.Error(),
					})
					return struct{}{}, signErr
				}
				dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "upload_claim_ready",
						RequestID: in.RequestID,
						Attachment: &chatAttachmentPayload{
							ID:          asset.ID,
							Filename:    firstNonEmpty(in.Filename, asset.ID),
							ContentType: firstNonEmpty(asset.MediaType, in.ContentType),
							SizeBytes:   asset.SizeBytes,
							URL:         signed.URL,
						},
				})
				return struct{}{}, nil
			},
			vango.Queue(8),
		)

		submitChat := setup.Action(&s,
			func(ctx context.Context, in chatSubmitCommand) (chatSubmitResult, error) {
				actor := props.Peek().Actor
				fail := func(err error) (chatSubmitResult, error) {
					if err == nil {
						err = errors.New("chat request failed")
					}
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "chat_error",
						RequestID: in.RequestID,
						Error:     strings.TrimSpace(err.Error()),
					})
					return chatSubmitResult{}, err
				}
				if in.RequestID == "" {
					return fail(errors.New("chat request id is required"))
				}

				org, err := appruntime.Get().Services.Org(ctx, actor.OrgID)
				if err != nil {
					return fail(err)
				}

				model := strings.TrimSpace(in.Model)
				if model == "" {
					model = org.DefaultModel
				}

				resolvedHeaders, keySource, err := appruntime.Get().Services.ResolveExecutionHeaders(
					ctx,
					actor.OrgID,
					buildBrowserBYOKHeaders(in.BrowserKeys),
					in.KeySource,
					services.AccessCredentialSessionAuth,
				)
				if err != nil {
					return fail(err)
				}
				if keySource == services.KeySourcePlatformHosted {
					if err := appruntime.Get().Services.ReservePlatformHostedUsage(ctx, actor.OrgID, model); err != nil {
						return fail(err)
					}
				}

				detail, err := appruntime.Get().Services.ManagedConversation(ctx, actor.OrgID, props.Peek().ConversationID)
				if err != nil {
					return fail(err)
				}
				runPlan, err := buildManagedRunPlan(detail, in.Message, in.AttachmentIDs, in.Regenerate, in.EditMessageID)
				if err != nil {
					return fail(err)
				}
				if len(runPlan.Input) == 0 {
					return fail(errors.New("chat input is required"))
				}

				proxy, err := ensureGatewayProxy()
				if err != nil {
					return fail(err)
				}
				client, err := proxy.Client(actor, resolvedHeaders)
				if err != nil {
					return fail(err)
				}

				selectedTransport := normalizeChatTransport(in.Transport)
				credentialSignature := chatCredentialSignature(resolvedHeaders, keySource)
				gatewayTools, gatewayToolConfig := chatruntime.ConversationServerTools(resolvedHeaders)
				runMetadata := chatRunMetadata(keySource, selectedTransport)
				chainMetadata := chatChainMetadata(keySource, selectedTransport, props.Peek().ConversationID)

				currentChainID, currentResumeToken := currentTrackedChain()
				if strings.TrimSpace(currentChainID) == "" && detail != nil && detail.Chain != nil {
					currentChainID = strings.TrimSpace(detail.Chain.ID)
				}

				var chain *vai.Chain
				if runPlan.Forked {
					closeWSChain()
				} else {
					switch selectedTransport {
					case chatTransportSSE:
						closeWSChain()
						if currentChainID != "" {
							chain, err = client.Chains.Attach(ctx, &vai.ChainAttachRequest{
								ChainID:     currentChainID,
								ResumeToken: currentResumeToken,
								Transport:   vai.TransportSSE,
							})
							if err != nil {
								return fail(err)
							}
						}
					case chatTransportWebsocket:
						gatewayMu.Lock()
						existingWS := wsChain
						existingConversationID := wsChainConversationID
						existingSignature := wsCredentialSignature
						gatewayMu.Unlock()
						if existingWS != nil &&
							existingConversationID == props.Peek().ConversationID &&
							existingSignature == credentialSignature &&
							existingWS.ID() == currentChainID {
							chain = existingWS
						} else {
							if existingWS != nil {
								closeWSChain()
							}
							if currentChainID != "" && strings.TrimSpace(currentResumeToken) != "" {
								chain, err = client.Chains.Attach(ctx, &vai.ChainAttachRequest{
									ChainID:     currentChainID,
									ResumeToken: currentResumeToken,
									Takeover:    true,
									Transport:   vai.TransportWebSocket,
								})
								if err != nil {
									return fail(err)
								}
								gatewayMu.Lock()
								wsChain = chain
								wsChainConversationID = props.Peek().ConversationID
								wsCredentialSignature = credentialSignature
								gatewayMu.Unlock()
							}
						}
					}
				}

				if chain == nil {
					connectTransport := vai.TransportSSE
					if selectedTransport == chatTransportWebsocket {
						connectTransport = vai.TransportWebSocket
					}
					chain, err = client.Chains.Connect(ctx, &vai.ChainRequest{
						ExternalSessionID: props.Peek().ConversationID,
						Model:             model,
						Messages:          runPlan.SeedHistory,
						GatewayTools:      gatewayTools,
						GatewayToolConfig: gatewayToolConfig,
						Metadata:          chainMetadata,
						Transport:         connectTransport,
					})
					if err != nil {
						return fail(err)
					}
					if selectedTransport == chatTransportWebsocket {
						gatewayMu.Lock()
						wsChain = chain
						wsChainConversationID = props.Peek().ConversationID
						wsCredentialSignature = credentialSignature
						gatewayMu.Unlock()
					}
				}
				setTrackedChain(chain.ID(), chain.ResumeToken())

				dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
					Type:      "chat_started",
					RequestID: in.RequestID,
				})

				runCtx, cancelRun := context.WithCancel(ctx)
				stream, err := chain.RunStream(runCtx, &vai.ChainRunRequest{
					Input:             runPlan.Input,
					Model:             model,
					GatewayTools:      gatewayTools,
					GatewayToolConfig: gatewayToolConfig,
					Metadata:          runMetadata,
				})
				if err != nil {
					cancelRun()
					return fail(err)
				}
				setActiveRun(in.RequestID, func() {
					cancelRun()
					_ = stream.Cancel()
				})
				defer clearActiveRun(in.RequestID)

				_, err = stream.Process(vai.StreamCallbacks{
					OnTextDelta: func(text string) {
						dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
							Type:      "chat_delta",
							RequestID: in.RequestID,
							Delta:     text,
						})
					},
					OnToolCallStart: func(id, name string, input map[string]any) {
						dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
							Type:      "chat_tool",
							RequestID: in.RequestID,
							Tool: map[string]any{
								"type":  "tool_call_start",
								"id":    id,
								"name":  name,
								"input": input,
							},
						})
					},
					OnToolResult: func(id, name string, content []types.ContentBlock, toolErr error) {
						payload := map[string]any{
							"type":    "tool_result",
							"id":      id,
							"name":    name,
							"content": content,
						}
						if toolErr != nil {
							payload["error"] = toolErr.Error()
						}
						dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
							Type:      "chat_tool",
							RequestID: in.RequestID,
							Tool:      payload,
						})
					},
				})
				cancelRun()
				if err != nil {
					if errors.Is(err, context.Canceled) {
						dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
							Type:      "chat_stopped",
							RequestID: in.RequestID,
						})
						return chatSubmitResult{}, err
					}
					return fail(err)
				}

				result := stream.Result()
				if result != nil && result.StopReason == types.RunStopReasonCancelled {
					dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
						Type:      "chat_stopped",
						RequestID: in.RequestID,
					})
					return chatSubmitResult{}, context.Canceled
				}
				if result == nil || result.Response == nil {
					return fail(errors.New("conversation completed without a response"))
				}
				setTrackedChain(chain.ID(), chain.ResumeToken())
				if err := appruntime.Get().Services.RecordUsage(
					ctx,
					actor.OrgID,
					props.Peek().ConversationID,
					"",
					"chat_stream",
					model,
					keySource,
					services.AccessCredentialSessionAuth,
					result.Usage,
					map[string]any{
						"via":        "chat_island",
						"chain_id":   chain.ID(),
						"transport":  selectedTransport,
						"regenerate": in.Regenerate,
						"edited":     in.EditMessageID != "",
					},
				); err != nil {
					appruntime.Get().Logger.Error("record usage failed", "error", err)
				}

				dispatchChatIsland(pageCtx, in.HID, chatServerMessage{
					Type:      "chat_complete",
					RequestID: in.RequestID,
					Assistant: buildChatAssistantPayload(result, keySource),
				})
				return chatSubmitResult{
					KeySource: keySource,
					ChainID:   chain.ID(),
					Transport: selectedTransport,
				}, nil
			},
			vango.DropWhileRunning(),
			vango.ActionOnSuccess(func(result any) {
				conversationData.Refetch()
				conversations.Refetch()
				if submitResult, ok := result.(chatSubmitResult); ok {
					setTrackedChain(submitResult.ChainID, "")
					if submitResult.KeySource == services.KeySourcePlatformHosted {
						balanceData.Refetch()
					}
				}
			}),
		)

		handleIslandMessage := func(msg vango.IslandMessage) {
			payload, err := vango.DecodeIslandPayload[chatIslandClientMessage](msg)
			if err != nil {
				appruntime.Get().Logger.Warn("chat island payload rejected", "hid", msg.HID, "error", err)
				return
			}

			switch strings.TrimSpace(payload.Type) {
			case "upload_intent":
				if !uploadIntent.Run(chatUploadIntentCommand{
					HID:         msg.HID,
					RequestID:   payload.RequestID,
					Filename:    payload.Filename,
					ContentType: payload.ContentType,
					SizeBytes:   payload.SizeBytes,
				}) {
					dispatchChatIsland(pageCtx, msg.HID, chatServerMessage{
						Type:      "upload_intent_error",
						RequestID: payload.RequestID,
						Error:     "upload request rejected while another upload is still being processed",
					})
				}
			case "upload_claim":
				if !uploadClaim.Run(chatUploadClaimCommand{
					HID:         msg.HID,
					RequestID:   payload.RequestID,
					Filename:    payload.Filename,
					ContentType: payload.ContentType,
					SizeBytes:   payload.SizeBytes,
					IntentToken: payload.IntentToken,
				}) {
					dispatchChatIsland(pageCtx, msg.HID, chatServerMessage{
						Type:      "upload_claim_error",
						RequestID: payload.RequestID,
						Error:     "upload claim rejected while another upload is still being processed",
					})
				}
			case "submit":
				requestedMode := services.KeySource(strings.TrimSpace(payload.KeySource))
				if requestedMode == "" {
					requestedMode = services.KeySourcePlatformHosted
				}
				if !submitChat.Run(chatSubmitCommand{
					HID:           msg.HID,
					RequestID:     payload.RequestID,
					Message:       payload.Message,
					Model:         payload.Model,
					KeySource:     requestedMode,
					Transport:     normalizeChatTransport(payload.Transport),
					AttachmentIDs: append([]string(nil), payload.AttachmentIDs...),
					Regenerate:    payload.Regenerate,
					EditMessageID: payload.EditMessageID,
					BrowserKeys:   payload.BrowserKeys,
				}) {
					dispatchChatIsland(pageCtx, msg.HID, chatServerMessage{
						Type:      "chat_error",
						RequestID: payload.RequestID,
						Error:     "a chat run is already in progress",
					})
				}
			case "stop":
				if !stopActiveRun(payload.RequestID) {
					dispatchChatIsland(pageCtx, msg.HID, chatServerMessage{
						Type:      "chat_stopped",
						RequestID: payload.RequestID,
					})
				}
			default:
				dispatchChatIsland(pageCtx, msg.HID, chatServerMessage{
					Type:      "chat_error",
					RequestID: payload.RequestID,
					Error:     "unsupported chat island event",
				})
			}
		}

		return func() *vango.VNode {
			actor := props.Get().Actor
			ctx := vango.UseCtx()
			staticSnapshot, loading, err := chatResolveStaticData(staticData.State(), staticData.Data(), staticData.Error())
			if err != nil {
				return AppShell(ctx, actor, PageErrorPanel(err))
			}
			if loading {
				return AppShell(ctx, actor, LoadingPanel("Loading chat..."))
			}

			conversationSnapshot, loading, err := chatResolveConversationData(
				props.Get().ConversationID,
				conversationData.State(),
				conversationData.Data(),
				conversationData.Error(),
				lastConversationData.Get(),
			)
			if err != nil {
				return AppShell(ctx, actor, PageErrorPanel(err))
			}
			if loading {
				return AppShell(ctx, actor, LoadingPanel("Loading chat..."))
			}

			conversationList, loading, err := chatResolveConversationsData(
				conversations.State(),
				conversations.Data(),
				conversations.Error(),
				lastConversations.Get(),
			)
			if err != nil {
				return AppShell(ctx, actor, PageErrorPanel(err))
			}
			if loading {
				return AppShell(ctx, actor, LoadingPanel("Loading chat..."))
			}

			data := chatBuildPageData(
				staticSnapshot,
				conversationList,
				conversationSnapshot,
				chatResolveBalanceData(balanceData.State(), balanceData.Data(), lastBalanceData.Get()),
			)
			return AppShell(ctx, actor,
				Div(
					Class("chat-page"),
					Sidebar(actor, data.Conversations, data.Detail.Conversation.ID, false, func() {
						pendingConversationID.Set(newDraftConversationID())
					}),
					Main(
						Class("chat-main"),
						Header(
							Class("chat-header"),
							Div(
								H1(Text(data.Detail.Conversation.Title)),
								P(Textf("Model: %s", data.Detail.Conversation.Model)),
							),
							Div(
								Class("chat-header-actions"),
								A(Href("/settings/keys"), Class("btn btn-secondary"), Text("Keys")),
								A(Href("/settings/billing"), Class("btn btn-secondary"), Text("Billing")),
							),
						),
						chatIslandBoundary(chatIslandProps(data, data.CurrentBalance), handleIslandMessage),
					),
				),
			)
		}
	})
}

func chatResolveStaticData(state vango.ResourceState, current *chatStaticData, currentErr error) (*chatStaticData, bool, error) {
	switch state {
	case vango.Ready:
		return current, false, nil
	case vango.Error:
		return nil, false, currentErr
	default:
		return nil, true, nil
	}
}

func chatResolveConversationsData(state vango.ResourceState, current []services.Conversation, currentErr error, last []services.Conversation) ([]services.Conversation, bool, error) {
	switch state {
	case vango.Ready:
		return current, false, nil
	case vango.Error:
		if last != nil {
			return last, false, nil
		}
		return nil, false, currentErr
	default:
		if last != nil {
			return last, false, nil
		}
		return nil, true, nil
	}
}

func chatResolveConversationData(conversationID string, state vango.ResourceState, current *chatConversationData, currentErr error, last *chatConversationData) (*chatConversationData, bool, error) {
	switch state {
	case vango.Ready:
		return current, false, nil
	case vango.Error:
		if chatConversationSnapshotMatches(last, conversationID) {
			return last, false, nil
		}
		return nil, false, currentErr
	default:
		if chatConversationSnapshotMatches(last, conversationID) {
			return last, false, nil
		}
		return nil, true, nil
	}
}

func chatResolveBalanceData(state vango.ResourceState, current, last chatBalanceState) chatBalanceState {
	if state == vango.Ready {
		return current
	}
	if last.Status == chatBalanceStatusReady || last.Status == chatBalanceStatusUnavailable {
		return last
	}
	return chatBalanceState{Status: chatBalanceStatusLoading}
}

func chatConversationSnapshotMatches(data *chatConversationData, conversationID string) bool {
	return data != nil && data.Detail != nil && data.Detail.Conversation.ID == conversationID
}

func chatBuildPageData(staticData *chatStaticData, conversations []services.Conversation, conversationData *chatConversationData, balance chatBalanceState) *chatPageData {
	return &chatPageData{
		Org:             staticData.Org,
		Conversations:   conversations,
		Detail:          conversationData.Detail,
		ProviderSecrets: staticData.ProviderSecrets,
		CurrentBalance:  balance,
		Messages:        conversationData.Messages,
	}
}

func chatIslandProps(data *chatPageData, balance chatBalanceState) map[string]any {
	props := map[string]any{
		"conversationId":        data.Detail.Conversation.ID,
		"model":                 data.Detail.Conversation.Model,
		"messages":              data.Messages,
		"modelOptions":          modelOptions(data.Detail.Conversation.Model),
		"allowBrowserBYOK":      data.Org.AllowBYOKOverride,
		"platformHostedEnabled": data.Org.HostedUsageEnabled,
		"hasWorkspaceProviders": len(data.ProviderSecrets) > 0,
		"initialKeySource":      string(data.Detail.Conversation.KeySource),
		"conversationTitle":     data.Detail.Conversation.Title,
		"providerHints":         providerHints(),
		"settingsKeysURL":       "/settings/keys",
		"settingsBillingURL":    "/settings/billing",
		"settingsAccessURL":     "/settings/access",
		"currentBalanceStatus":  balance.Status,
		"hostedModels":          hostedModelOptions(data.Detail.Conversation.Model),
		"initialTransport":      normalizeChatTransport(data.Detail.PreferredTransport),
	}
	if balance.Status == chatBalanceStatusReady {
		props["currentBalanceCents"] = balance.CurrentBalanceCents
	}
	return props
}
