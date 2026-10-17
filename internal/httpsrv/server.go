// Package httpsrv wires the HTTP surface: routes, middleware, async clicks.
package httpsrv

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"urlshortener/internal/config"
	"urlshortener/internal/metrics"
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
	return &Server{
		cfg:     cfg,
		st:      st,
		sh:      sh,
		log:     log,
		m:       m,
		limiter: newTokenLimiter(cfg.RateLimitPerMin),
		clicks:  make(chan clickRec, 512),
	}
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("urlshortener\n"))
	})

	return s.middlewares(mux)
}

// Workers drains the click queue until ctx is done, inserting clicks and
// bumping the link counter. On shutdown it drains non-blocking first so the
// redirect path never blocks on a closed DB.
func (s *Server) Workers(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case c := <-s.clicks:
					// detached: DB close happens after errgroup.Wait()
					s.saveClick(context.Background(), c)
				default:
					return
				}
			}
		case c := <-s.clicks:
			s.saveClick(ctx, c)
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
