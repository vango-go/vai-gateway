package chatruntime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/vango-go/vai-lite/internal/services"
	"github.com/vango-go/vai-lite/sdk"
)

const (
	headerInternalOrgID         = "X-VAI-Internal-Org-ID"
	headerInternalPrincipalID   = "X-VAI-Internal-Principal-ID"
	headerInternalPrincipalType = "X-VAI-Internal-Principal-Type"
)

type InProcessGateway struct {
	server *httptest.Server
}

func NewInProcessGateway(handler http.Handler) (*InProcessGateway, error) {
	if handler == nil {
		return nil, errors.New("gateway handler is not configured")
	}
	return &InProcessGateway{server: httptest.NewServer(handler)}, nil
}

func (g *InProcessGateway) Close() {
	if g == nil || g.server == nil {
		return
	}
	g.server.Close()
}

func (g *InProcessGateway) BaseURL() string {
	if g == nil || g.server == nil {
		return ""
	}
	return g.server.URL
}

func (g *InProcessGateway) Client(actor services.UserIdentity, headers http.Header) (*vai.Client, error) {
	if g == nil || g.server == nil {
		return nil, errors.New("gateway handler is not configured")
	}
	opts := []vai.ClientOption{
		vai.WithBaseURL(g.server.URL),
		vai.WithHTTPClient(g.server.Client()),
	}
	if orgID := strings.TrimSpace(actor.OrgID); orgID != "" {
		opts = append(opts, vai.WithHeader(headerInternalOrgID, orgID))
	}
	if userID := strings.TrimSpace(actor.UserID); userID != "" {
		opts = append(opts, vai.WithHeader(headerInternalPrincipalID, userID))
	}
	opts = append(opts, vai.WithHeader(headerInternalPrincipalType, "app_user"))
	for key, values := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value := ""
		for _, candidate := range values {
			if strings.TrimSpace(candidate) != "" {
				value = strings.TrimSpace(candidate)
				break
			}
		}
		if value == "" {
			continue
		}
		opts = append(opts, vai.WithHeader(key, value))
	}
	return vai.NewClient(opts...), nil
}
