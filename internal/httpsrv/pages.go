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
	ChartDaily  []chartPoint
	ChartHourly []chartPoint
}

// chartPoint feeds Chart.js labels/datasets.
type chartPoint struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
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
		s.log.Error("link page lookup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().UTC()
	stats, err := s.pageStats(r.Context(), l.ID, now)
	if err != nil {
		s.log.Error("link page stats failed", "err", err)
	}
	s.render(w, "link", http.StatusOK, map[string]any{
		"Title":   "Link " + l.Code,
		"Link":    s.toLinkJSON(l, s.cfg.BaseURL),
		"Stats":   stats,
		"Expired": l.Expired(now),
	})
}

// pageStats aggregates the windows the details page shows.
func (s *Server) pageStats(ctx context.Context, linkID int64, now time.Time) (linkStats, error) {
	st := s.st
	out := linkStats{
		TopReferers: []store.RefCount{},
		Recent:      []store.ClickRow{},
		ChartDaily:  []chartPoint{},
		ChartHourly: []chartPoint{},
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
	var daily []store.DayCount
	if daily, err = st.ClicksByDay(ctx, linkID, now.Add(-30*24*time.Hour), now.Add(time.Hour)); err != nil {
		return out, err
	}
	out.ChartDaily = toChartDaily(daily)
	var hourly []store.HourCount
	if hourly, err = st.ClicksByHour(ctx, linkID, now.Add(-24*time.Hour)); err != nil {
		return out, err
	}
	out.ChartHourly = toChartHourly(hourly)
	return out, nil
}

// toChartDaily densifies the 30-day series so the chart shows zero days too.
func toChartDaily(days []store.DayCount) []chartPoint {
	byDay := make(map[int64]int64, len(days))
	for _, d := range days {
		byDay[d.Day.Unix()] = d.Count
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]chartPoint, 0, len(days)+1)
	for i := 29; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		out = append(out, chartPoint{Label: day.Format("01-02"), Count: byDay[day.Unix()]})
	}
	return out
}

// errIsNotFound hides the store sentinel from page logic.
func errIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// toChartHourly densifies the 24-hour series the same way.
func toChartHourly(hours []store.HourCount) []chartPoint {
	byHour := make(map[int64]int64, len(hours))
	for _, h := range hours {
		byHour[h.Hour.Unix()] = h.Count
	}
	top := time.Now().UTC().Truncate(time.Hour)
	out := make([]chartPoint, 0, len(hours)+1)
	for i := 23; i >= 0; i-- {
		hour := top.Add(-time.Duration(i) * time.Hour)
		out = append(out, chartPoint{Label: hour.Format("01-02 15h"), Count: byHour[hour.Unix()]})
	}
	return out
}

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
	if daily, err := s.st.ClicksByDayGlobal(ctx, now.Add(-30*24*time.Hour), now.Add(time.Hour)); err != nil {
		s.log.Error("stats: daily", "err", err)
	} else {
		d["ChartDaily30"] = toChartDaily(daily)
	}
	if top, err := s.st.ListLinks(ctx, 5, 0); err == nil {
		d["TopLinks"] = s.linkJSONs(top)
	}
	s.render(w, "stats", http.StatusOK, d)
}
