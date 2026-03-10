package components

import (
	"context"
	"fmt"

	"github.com/vango-go/vai-lite/internal/appruntime"
	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
	"github.com/vango-go/vango/setup"
)

type HomePageProps struct {
	Actor services.UserIdentity
}

func HomePage(p HomePageProps) vango.Component {
	return vango.Setup(p, func(s vango.SetupCtx[HomePageProps]) vango.RenderFn {
		props := s.Props()
		redirected := setup.Signal(&s, false)
		pendingConversationID := setup.Signal(&s, "")
		conversations := setup.ResourceKeyed(&s,
			func() string { return props.Get().Actor.OrgID },
			func(ctx context.Context, orgID string) ([]services.Conversation, error) {
				return appruntime.Get().Services.ListManagedConversations(ctx, orgID)
			},
		)

		s.Effect(func() vango.Cleanup {
			if conversationID := pendingConversationID.Get(); conversationID != "" {
				if ctx := vango.UseCtx(); ctx != nil {
					pendingConversationID.Set("")
					ctx.Navigate("/chat/" + conversationID)
				}
			}
			return nil
		})

		s.Effect(func() vango.Cleanup {
			if redirected.Get() || !conversations.IsReady() {
				return nil
			}
			items := conversations.Data()
			if len(items) == 0 {
				return nil
			}
			redirected.Set(true)
			if ctx := vango.UseCtx(); ctx != nil {
				ctx.Navigate("/chat/"+items[0].ID, vango.WithReplace())
			}
			return nil
		})

		return func() *vango.VNode {
			actor := props.Get().Actor
			renderCtx := vango.UseCtx()
			return conversations.Match(
				vango.OnLoadingOrPending[[]services.Conversation](func() *vango.VNode {
					return AppShell(renderCtx, actor, LoadingPanel("Opening your workspace..."))
				}),
				vango.OnError[[]services.Conversation](func(err error) *vango.VNode {
					return AppShell(renderCtx, actor, PageErrorPanel(fmt.Errorf("load conversations: %w", err)))
				}),
				vango.OnReady(func(items []services.Conversation) *vango.VNode {
					if len(items) > 0 {
						return AppShell(renderCtx, actor, LoadingPanel("Opening your workspace..."))
					}
					return AppShell(renderCtx, actor,
						EmptyState(
							"Your workspace is ready",
							"Start a new chat, store shared workspace BYOK keys in WorkOS Vault, or add VAI credits for platform-hosted usage.",
							vango.Children(
								Button(
									Type("button"),
									Class("btn btn-primary"),
									OnClick(func() {
										redirected.Set(true)
										pendingConversationID.Set(services.NewExternalSessionID("chat"))
									}),
									Text("Start a chat"),
								),
								A(Href("/settings/keys"), Class("btn btn-secondary"), Text("Manage keys")),
								A(Href("/settings/billing"), Class("btn btn-secondary"), Text("Billing")),
							),
						),
					)
				}),
			)
		}
	})
}
