// Package httpsrv wires the HTTP surface: routes, middleware, async clicks.
package httpsrv

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"time"

	"urlshortener/internal/analytics"
	"urlshortener/internal/config"
	"urlshortener/internal/metrics"
	"urlshortener/internal/qr"
	"urlshortener/internal/shortener"
	"urlshortener/internal/store"
)

// Server couples the HTTP handlers to the domain services.
type Server struct {
	cfg     config.Config
	st      *store.Store
	sh      *shortener.Shortener
	log     *slog.Logger
	m       *metrics.Registry
	limiter *tokenLimiter
	pages   map[string]*template.Template
	an      *analytics.Service
	qr      *qr.Renderer
	trusted []net.IPNet
}

// New builds the Server with an analytics-backed async click pipeline.
// Templates are static assets: a broken parse fails startup instead of
// silently serving a web UI that returns 500 on every page.
func New(cfg config.Config, st *store.Store, sh *shortener.Shortener, log *slog.Logger, m *metrics.Registry) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		st:      st,
		sh:      sh,
		log:     log,
		m:       m,
		limiter: newTokenLimiter(cfg.RateLimitPerMin),
	}
	sets, err := buildPages()
	if err != nil {
		return nil, fmt.Errorf("httpsrv: load templates: %w", err)
	}
	s.pages = sets
	s.an = analytics.New(st, time.Duration(cfg.RetentionDays)*24*time.Hour, log)
	s.qr = qr.New(cfg.BaseURL)
	s.trusted = parseTrustedProxies(cfg.TrustedProxies)
	return s, nil
}

// Handler returns the fully wrapped router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /api/shorten", s.handleShorten)
	mux.HandleFunc("GET /api/links", s.handleListLinks)
	mux.HandleFunc("GET /api/links/{code}", s.handleGetLink)
	mux.HandleFunc("DELETE /api/links/{code}", s.handleDeleteLink)
	mux.HandleFunc("GET /r/{code}", s.handleRedirect)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("GET /api/links/{code}/stats", s.handleLinkStats)
	mux.HandleFunc("GET /api/links/{code}/export.csv", s.handleExportCSV)
	// dot suffix after a wildcard panics ServeMux registration, so the .png
	// suffix is matched here instead
	mux.HandleFunc("GET /qr/{code}", s.handleQR)

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /links", s.handleLinksPage)
	mux.HandleFunc("GET /stats", s.handleStatsPage)
	mux.HandleFunc("GET /{code}", s.handleLinkPage)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusTemporaryRedirect)
	})

	return s.middlewares(mux)
}

// linkJSONs converts a page of links for templates.
func (s *Server) linkJSONs(links []store.Link) []linkJSON {
	now := time.Now()
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		// pages are public: creator IPs must not leak into the HTML
		out = append(out, s.toLinkJSON(l, s.cfg.BaseURL, "", now))
	}
	return out
}

// IngestLoop runs the async click pipeline until ctx is done; on shutdown
// (scheduled after the HTTP drain) it flushes every accepted click to the
// database before returning.
func (s *Server) IngestLoop(ctx context.Context) error {
	return s.an.Run(ctx)
}
