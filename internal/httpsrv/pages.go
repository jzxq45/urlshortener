package httpsrv

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"urlshortener/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pageDefs parses layout.html + the page file per set so content blocks are
// page-local (no cross-page accumulation under one template name).
var pageDefs = map[string]string{
	"index": "index.html",
	"link":  "link.html",
	"links": "links.html",
	"stats": "stats.html",
	"404":   "404.html",
}

var funcMap = template.FuncMap{
	"humanTime": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	"humanDay":  func(t time.Time) string { return t.Format("01-02") },
	"humanHour": func(t time.Time) string { return t.Format("01-02 15h") },
	"jsonObj":   func(i any) template.JS { b, _ := json.Marshal(i); return template.JS(b) },
	"add":       func(a, b int) int { return a + b },
	"sub":       func(a, b int) int { return a - b },
}

// buildPages parses all template sets once at startup. The row fragment set
// is parsed without layout (it's an htmx partial).
func buildPages() (map[string]*template.Template, error) {
	sets := make(map[string]*template.Template, len(pageDefs))
	for name, file := range pageDefs {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFS(templateFS, "templates/layout.html", "templates/"+file)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		sets[name] = t
	}
	frag, err := template.New("row.html").Funcs(funcMap).ParseFS(templateFS, "templates/row.html")
	if err != nil {
		return nil, fmt.Errorf("parse row fragment: %w", err)
	}
	sets["row"] = frag
	return sets, nil
}

// render writes the named page.
func (s *Server) render(w http.ResponseWriter, name string, status int, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("template unavailable", "page", name)
		writeErr(w, http.StatusInternalServerError, "template unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		s.log.Error("render failed", "page", name, "err", err)
	}
}

// renderFragment writes a small htmx partial (execute the define directly).
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	t, ok := s.pages[name]
	if !ok {
		writeErr(w, http.StatusInternalServerError, "template unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = t.ExecuteTemplate(w, "row", data)
}

// handleIndex answers GET / — form plus the 20 newest links.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	links, err := s.st.ListLinks(r.Context(), 20, 0)
	if err != nil {
		s.log.Error("index list failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.render(w, "index", http.StatusOK, map[string]any{
		"Title": "Shorten",
		"Links": s.linkJSONs(links),
	})
}

// handleLinksPage answers GET /links with pagination (?page=).
func (s *Server) handleLinksPage(w http.ResponseWriter, r *http.Request) {
	const perPage = 20
	page := clampInt(parseQueryInt(r, "page", 1), 1, 1_000_000)
	total, err := s.st.CountLinks(r.Context())
	if err != nil {
		s.log.Error("links count failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	pages := int((total + perPage - 1) / perPage)
	if pages < 1 {
		pages = 1
	}
	links, err := s.st.ListLinks(r.Context(), perPage, (page-1)*perPage)
	if err != nil {
		s.log.Error("links list failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.render(w, "links", http.StatusOK, map[string]any{
		"Title":   "All links",
		"Links":   s.linkJSONs(links),
		"Page":    page,
		"Pages":   pages,
		"HasPrev": page > 1,
		"HasNext": page < pages,
		"PageURL": func(p int) string { return "/links?page=" + strconv.Itoa(p) },
	})
}

// linkStats is the per-code analytics payload for the details page.
type linkStats struct {
	Clicks24h   int64
	Clicks7d    int64
	Clicks30d   int64
	ClicksTotal int64
	UniqueIPs   int64
	TopReferers []store.RefCount
	Recent      []store.ClickRow
	Days7       []store.DayCount
}

// handleLinkPage answers GET /{code} with the link details page (404 fallback).
func (s *Server) handleLinkPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.st.GetLink(r.Context(), code)
	if err != nil {
		if errIsNotFound(err) {
			s.render(w, "404", http.StatusNotFound, map[string]any{"Title": "Not found", "Code": code})
			return
		}
		if clientMissing(r, err) {
			return
		}
		s.log.Error("link page lookup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().UTC()
	isCreator := clientIP(r) == l.CreatorIP
	stats, err := s.pageStats(r.Context(), l.ID, now, isCreator)
	if err != nil {
		s.log.Error("link page stats failed", "err", err)
	}
	s.render(w, "link", http.StatusOK, map[string]any{
		"Title":   "Link " + l.Code,
		"Link":    s.toLinkJSON(l, s.cfg.BaseURL, clientIP(r), now),
		"Stats":   stats,
		"Expired": l.Expired(now),
	})
}

// pageStats aggregates the windows the details page shows. Unless the viewer
// is the link's creator, recent click IPs are masked (no PII to strangers).
//
// The stats block runs several small windows back-to-back. They are all
// read-only and ride the 20-connection query_only pool while the writer stays
// free, so fusing them into one CTE would complicate the SQL for no practical
// gain at this scale.
func (s *Server) pageStats(ctx context.Context, linkID int64, now time.Time, isCreator bool) (linkStats, error) {
	st := s.st
	out := linkStats{
		TopReferers: []store.RefCount{},
		Recent:      []store.ClickRow{},
		Days7:       []store.DayCount{},
	}
	var err error
	if out.Clicks24h, err = st.ClicksInWindow(ctx, linkID, now.Add(-24*time.Hour)); err != nil {
		return out, err
	}
	if out.Clicks7d, err = st.ClicksInWindow(ctx, linkID, now.Add(-7*24*time.Hour)); err != nil {
		return out, err
	}
	if out.Clicks30d, err = st.ClicksInWindow(ctx, linkID, now.Add(-30*24*time.Hour)); err != nil {
		return out, err
	}
	if out.ClicksTotal, err = st.TotalClicksForLink(ctx, linkID); err != nil {
		return out, err
	}
	if out.UniqueIPs, err = st.UniqueIPs(ctx, linkID, time.Time{}); err != nil {
		return out, err
	}
	if out.TopReferers, err = st.TopReferers(ctx, linkID, now.Add(-30*24*time.Hour), 10); err != nil {
		return out, err
	}
	if out.Recent, err = st.RecentClicks(ctx, linkID, 15); err != nil {
		return out, err
	}
	if !isCreator {
		for i := range out.Recent {
			out.Recent[i].IP = maskIP(out.Recent[i].IP)
		}
	}
	// 7-day daily table for the page (the 30d/24h hourly series stay in the
	// JSON API); a densified 30-point chart needs JS, the table does not
	if out.Days7, err = st.ClicksByDay(ctx, linkID, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil {
		return out, err
	}
	return out, nil
}

// errIsNotFound hides the store sentinel from page logic.
func errIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// handleStatsPage answers GET /stats with global analytics.
func (s *Server) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	d := map[string]any{"Title": "Stats"}
	var err error
	if d["TotalLinks"], err = s.st.CountLinks(ctx); err != nil {
		s.log.Error("stats: links", "err", err)
	}
	if d["ActiveLinks"], err = s.st.CountActiveLinks(ctx, now); err != nil {
		s.log.Error("stats: active", "err", err)
	}
	if d["TotalClicks"], err = s.st.ClicksCountGlobal(ctx, time.Time{}); err != nil {
		s.log.Error("stats: total clicks", "err", err)
	}
	if d["Clicks24h"], err = s.st.ClicksCountGlobal(ctx, now.Add(-24*time.Hour)); err != nil {
		s.log.Error("stats: clicks24h", "err", err)
	}
	if d["Clicks7d"], err = s.st.ClicksCountGlobal(ctx, now.Add(-7*24*time.Hour)); err != nil {
		s.log.Error("stats: clicks7d", "err", err)
	}
	if d["Clicks30d"], err = s.st.ClicksCountGlobal(ctx, now.Add(-30*24*time.Hour)); err != nil {
		s.log.Error("stats: clicks30d", "err", err)
	}
	if d["TopReferers"], err = s.st.TopReferersGlobal(ctx, now.Add(-30*24*time.Hour), 10); err != nil {
		s.log.Error("stats: referers", "err", err)
	}
	// last-7-days table for the HTML page (the 30d series stays in the API)
	if daily, err := s.st.ClicksByDayGlobal(ctx, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil {
		s.log.Error("stats: daily", "err", err)
	} else {
		d["Days7"] = daily
	}
	if top, err := s.st.ListLinks(ctx, 5, 0); err == nil {
		d["TopLinks"] = s.linkJSONs(top)
	}
	s.render(w, "stats", http.StatusOK, d)
}
