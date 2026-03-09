package components

import (
	"github.com/vango-go/vango"
	. "github.com/vango-go/vango/el"
	vserver "github.com/vango-go/vango/pkg/server"
)

func csrfToken(ctx vango.Ctx) string {
	if ctx == nil {
		return ""
	}
	return vserver.CSRFCtxToken(ctx)
}

func CSRFField(ctx vango.Ctx) *vango.VNode {
	token := csrfToken(ctx)
	if token == "" {
		return nil
	}
	return Input(Type("hidden"), Name("csrf"), Value(token))
}

//test comment
