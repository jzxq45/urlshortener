// Package httpsrv wires the HTTP surface: routes, middleware, async clicks.
package httpsrv

import (
	"context"
	"html/template"
	"log/slog"
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
	clicks  chan clickRec
	pages   map[string]*template.Template
	an      *analytics.Service
	qr      *qr.Renderer
}

// clickRec is one queued redirect event.
type clickRec struct {
	linkID  int64
	ts      time.Time
	ip      string
	ua      string
	referer string
}

// New builds the Server with an in-memory click queue for async inserts.
func New(cfg config.Config, st *store.Store, sh *shortener.Shortener, log *slog.Logger, m *metrics.Registry) *Server {
	s := &Server{
		cfg:     cfg,
		st:      st,
		sh:      sh,
		log:     log,
		m:       m,
		limiter: newTokenLimiter(cfg.RateLimitPerMin),
		clicks:  make(chan clickRec, 512),
	}
	if sets, err := buildPages(); err != nil {
		// templates are static assets; a broken build must be loud but not
		// panic (per project rules)
		log.Error("template parse failed", "err", err)
	} else {
		s.pages = sets
	}
	s.an = analytics.New(st, time.Duration(cfg.RetentionDays)*24*time.Hour, log)
	s.qr = qr.New(cfg.BaseURL)
	return s
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
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, s.toLinkJSON(l, s.cfg.BaseURL))
	}
	return out
}

// Workers drains the click queue until ctx is done, inserting clicks and
// bumping the link counter. Inserts run with a detached context (WithoutCancel)
// so a race between taking an event from the queue and ctx cancellation cannot
// silently drop a click; on shutdown the loop drains non-blocking.
func (s *Server) Workers(ctx context.Context) {
	// the queue is the ownership boundary: once an event is taken it must
	// reach the DB regardless of loop ctx state
	dbCtx := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case c := <-s.clicks:
					// detached: DB close happens after errgroup.Wait()
					s.saveClick(dbCtx, c)
				default:
					return
				}
			}
		case c := <-s.clicks:
			s.saveClick(dbCtx, c)
		}
	}
}

func (s *Server) saveClick(ctx context.Context, c clickRec) {
	s.m.Clicks.Inc(nil)
	if err := s.st.InsertClick(ctx, store.Click{
		LinkID:    c.linkID,
		Ts:        c.ts,
		IP:        c.ip,
		UserAgent: c.ua,
		Referer:   c.referer,
	}); err != nil {
		s.log.Error("click insert failed", "err", err)
		return
	}
	if err := s.st.IncrementClicks(ctx, c.linkID); err != nil {
		s.log.Error("click counter failed", "err", err)
	}
}
