package httpsrv

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"urlshortener/internal/store"
)

// RetentionLoop runs the daily click cleanup; wraps analytics.Service for
// the router layer (usable inside an errgroup).
func (s *Server) RetentionLoop(ctx context.Context) error {
	return s.an.Start(ctx)
}

// apiStats is GET /api/links/{code}/stats response.
type apiStats struct {
	Code        string            `json:"code"`
	TotalClicks int64             `json:"total_clicks"`
	UniqueIPs   int64             `json:"unique_ips"`
	Windows     map[string]int64  `json:"windows"` // clicks per time window
	TopReferers []store.RefCount  `json:"top_referers"`
	Recent      []store.ClickRow  `json:"recent"`
	Daily30     []store.DayCount  `json:"daily_30d"`
	Hourly24    []store.HourCount `json:"hourly_24h"`
}

// handleLinkStats answers GET /api/links/{code}/stats with click aggregates.
func (s *Server) handleLinkStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.PathValue("code")
	l, err := s.st.GetLink(ctx, code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "link not found")
			return
		}
		s.log.Error("stats lookup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().UTC()
	out := apiStats{
		Code:        code,
		TotalClicks: l.Clicks,
		Windows: map[string]int64{
			"24h": 0,
			"7d":  0,
			"30d": 0,
		},
		TopReferers: []store.RefCount{},
		Recent:      []store.ClickRow{},
		Daily30:     []store.DayCount{},
		Hourly24:    []store.HourCount{},
	}
	// l.Clicks is the denormalized counter; the exact number lives in clicks
	if total, err := s.st.TotalClicksForLink(ctx, l.ID); err == nil {
		out.TotalClicks = total
	}
	if n, err := s.st.ClicksInWindow(ctx, l.ID, now.Add(-24*time.Hour)); err == nil {
		out.Windows["24h"] = n
	}
	if n, err := s.st.ClicksInWindow(ctx, l.ID, now.Add(-7*24*time.Hour)); err == nil {
		out.Windows["7d"] = n
	}
	if n, err := s.st.ClicksInWindow(ctx, l.ID, now.Add(-30*24*time.Hour)); err == nil {
		out.Windows["30d"] = n
	}
	if n, err := s.st.UniqueIPs(ctx, l.ID, time.Time{}); err == nil {
		out.UniqueIPs = n
	}
	if refs, err := s.st.TopReferers(ctx, l.ID, now.Add(-30*24*time.Hour), 10); err == nil {
		out.TopReferers = refs
	} else {
		s.log.Error("stats referers failed", "err", err)
	}
	if rec, err := s.st.RecentClicks(ctx, l.ID, 15); err == nil {
		// PII: same rule as the web page — full IPs only for the creator
		if clientIP(r) != l.CreatorIP {
			for i := range rec {
				rec[i].IP = maskIP(rec[i].IP)
			}
		}
		out.Recent = rec
	}
	if daily, err := s.st.ClicksByDay(ctx, l.ID, now.Add(-30*24*time.Hour), now.Add(time.Hour)); err == nil {
		out.Daily30 = daily
	}
	if hourly, err := s.st.ClicksByHour(ctx, l.ID, now.Add(-24*time.Hour)); err == nil {
		out.Hourly24 = hourly
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExportCSV answers GET /api/links/{code}/export.csv with the raw
// click stream; 404 for unknown codes.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if _, err := s.st.GetLink(r.Context(), code); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "link not found")
			return
		}
		s.log.Error("export lookup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+code+`.csv"`)
	if _, err := s.an.ExportCSV(r.Context(), code, w); err != nil {
		// headers are sent; log only
		s.log.Error("export failed", "err", err)
	}
}

// handleQR answers GET /qr/{code}.png with a cached QR PNG of the short URL;
// 404 for unknown or expired links.
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSuffix(r.PathValue("code"), ".png")
	l, err := s.st.GetLink(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "link not found")
			return
		}
		s.log.Error("qr lookup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if l.Expired(time.Now()) {
		writeErr(w, http.StatusGone, "link expired")
		return
	}
	png, err := s.qr.PNG(code)
	if err != nil {
		s.log.Error("qr render failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// immutable while the code exists; TTL in the cache covers content changes
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
