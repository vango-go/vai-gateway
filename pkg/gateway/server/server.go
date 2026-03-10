package server

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	assetsvc "github.com/vango-go/vai-lite/pkg/gateway/assets"
	chainrt "github.com/vango-go/vai-lite/pkg/gateway/chains"
	"github.com/vango-go/vai-lite/pkg/gateway/config"
	"github.com/vango-go/vai-lite/pkg/gateway/handlers"
	"github.com/vango-go/vai-lite/pkg/gateway/lifecycle"
	"github.com/vango-go/vai-lite/pkg/gateway/mw"
	"github.com/vango-go/vai-lite/pkg/gateway/ratelimit"
	"github.com/vango-go/vai-lite/pkg/gateway/upstream"
)

type Server struct {
	cfg    config.Config
	logger *slog.Logger
	mux    *http.ServeMux

	upstreams  upstream.Factory
	httpClient *http.Client
	limiter    *ratelimit.Limiter
	lifecycle  *lifecycle.Lifecycle
	chains     *chainrt.Manager
	assets     *assetsvc.Service
}

type Options struct {
	ChainStore   chainrt.Store
	AssetService *assetsvc.Service
}

func New(cfg config.Config, logger *slog.Logger) *Server {
	return NewWithOptions(cfg, logger, Options{})
}

func NewWithOptions(cfg config.Config, logger *slog.Logger, opts Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout: cfg.UpstreamConnectTimeout,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: cfg.UpstreamResponseHeaderTimeout,
		},
	}

	s := &Server{
		cfg:    cfg,
		logger: logger,
		mux:    http.NewServeMux(),
		upstreams: upstream.Factory{
			HTTPClient: httpClient,
		},
		httpClient: httpClient,
		limiter: ratelimit.New(ratelimit.Config{
			RPS:                     cfg.LimitRPS,
			Burst:                   cfg.LimitBurst,
			MaxConcurrentRequests:   cfg.LimitMaxConcurrentRequests,
			MaxConcurrentStreams:    cfg.LimitMaxConcurrentStreams,
			MaxConcurrentWSSessions: cfg.WSMaxSessionsPerPrincipal,
		}),
		lifecycle: &lifecycle.Lifecycle{},
		chains:    chainrt.NewManager(opts.ChainStore, chainrt.DefaultManagerConfig()),
		assets:    opts.AssetService,
	}

	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.Handle("/healthz", handlers.HealthHandler{})
	s.mux.Handle("/readyz", handlers.ReadyHandler{Config: s.cfg, Lifecycle: s.lifecycle})

	s.mux.Handle("/v1/messages", handlers.MessagesHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/runs", handlers.RunsHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Stream:     false,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/runs:stream", handlers.RunsHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Stream:     true,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/live", handlers.LiveHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Chains:     s.chains,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/chains/ws", handlers.ChainWSHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Chains:     s.chains,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/chains", handlers.ChainsHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Chains:     s.chains,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/chains/", handlers.ChainsHandler{
		Config:     s.cfg,
		Upstreams:  s.upstreams,
		HTTPClient: s.httpClient,
		Logger:     s.logger,
		Limiter:    s.limiter,
		Lifecycle:  s.lifecycle,
		Chains:     s.chains,
		Assets:     s.assets,
	})
	s.mux.Handle("/v1/assets:upload-intent", handlers.AssetsHandler{Config: s.cfg, Logger: s.logger, Assets: s.assets})
	s.mux.Handle("/v1/assets:claim", handlers.AssetsHandler{Config: s.cfg, Logger: s.logger, Assets: s.assets})
	s.mux.Handle("/v1/assets/", handlers.AssetsHandler{Config: s.cfg, Logger: s.logger, Assets: s.assets})
	s.mux.Handle("/v1/sessions", handlers.SessionsHandler{Config: s.cfg, Chains: s.chains})
	s.mux.Handle("/v1/sessions/", handlers.SessionsHandler{Config: s.cfg, Chains: s.chains})
	s.mux.Handle("/v1/runs/", handlers.ChainRunsReadHandler{Config: s.cfg, Chains: s.chains})
	s.mux.Handle("/v1/server-tools:execute", handlers.ServerToolsHandler{
		Config:     s.cfg,
		HTTPClient: s.httpClient,
	})
	s.mux.Handle("/v1/models", handlers.ModelsHandler{Config: s.cfg})

	// Catch-all JSON 404s for unknown paths.
	s.mux.Handle("/", handlers.NotFoundHandler{})
}

func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = mw.RateLimit(s.cfg, s.limiter, h)
	h = mw.Auth(s.cfg, h)
	h = mw.APIVersion(h)
	h = mw.CORS(s.cfg, h)
	h = mw.Recover(s.logger, h)
	h = mw.AccessLog(s.logger, h)
	h = mw.RequestID(h)
	return h
}

func (s *Server) SetDraining() {
	if s == nil || s.lifecycle == nil {
		return
	}
	s.lifecycle.SetDraining(true)
}
