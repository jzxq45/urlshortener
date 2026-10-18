package httpsrv

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"urlshortener/internal/shortener"
	"urlshortener/internal/store"
)

// shortenReq is POST /api/shorten input.
type shortenReq struct {
	URL        string `json:"url"`
	CustomCode string `json:"custom_code"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// maskIP hides the last octet (IPv4) so public pages and APIs never expose
// full client IPs; only the requester sees their own address unmasked.
func maskIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return strings.Join(parts[:3], ".") + ".*"
	}
	return "hidden"
}

// linkJSON is the API link representation.
type linkJSON struct {
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	ShortURL  string     `json:"short_url"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Expired   bool       `json:"expired"`
	Clicks    int64      `json:"clicks"`
	CreatorIP string     `json:"creator_ip"`
	Custom    bool       `json:"custom_code"`
}

// toLinkJSON converts a link for API/pages. `now` is passed in so a listing
// evaluates one clock read instead of one per row.
func (s *Server) toLinkJSON(l store.Link, baseURL, requesterIP string, now time.Time) linkJSON {
	// PII: the creator's full IP is visible only to that creator
	ip := "hidden"
	if l.CreatorIP != "" && l.CreatorIP == requesterIP {
		ip = l.CreatorIP
	}
	return linkJSON{
		Code:      l.Code,
		URL:       l.URL,
		ShortURL:  baseURL + "/r/" + l.Code,
		CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt,
		Expired:   l.Expired(now),
		Clicks:    l.Clicks,
		CreatorIP: ip,
		Custom:    l.CustomCode,
	}
}

// handleShorten validates, rate-limits and stores a new link (201/400/409/429).
func (s *Server) handleShorten(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow(clientIP(r)) {
		s.m.Shorten.Inc(map[string]string{"status": "429"})
		w.Header().Set("Retry-After", "60")
		writeErrEither(w, r, http.StatusTooManyRequests, "rate limit exceeded, try later")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req shortenReq
	if err := dec.Decode(&req); err != nil {
		s.m.Shorten.Inc(map[string]string{"status": "400"})
		writeErrEither(w, r, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if r.Context().Err() != nil {
		return
	}

	var ttl *time.Duration
	if req.TTLSeconds != 0 {
		if req.TTLSeconds < 0 {
			s.m.Shorten.Inc(map[string]string{"status": "400"})
			writeErrEither(w, r, http.StatusBadRequest, "ttl_seconds must be positive or 0")
			return
		}
		// cap TTL at 10y to keep expires_at arithmetic safe
		if req.TTLSeconds > 10*366*24*3600 {
			s.m.Shorten.Inc(map[string]string{"status": "400"})
			writeErrEither(w, r, http.StatusBadRequest, "ttl_seconds too large")
			return
		}
		d := time.Duration(req.TTLSeconds) * time.Second
		ttl = &d
	}

	requesterIP := clientIP(r)
	l, err := s.sh.Create(r.Context(), shortener.CreateInput{
		URL:        req.URL,
		CustomCode: req.CustomCode,
		TTL:        ttl,
		CreatorIP:  requesterIP,
	})
	switch {
	// custom-code duplicates wrap ErrValidation too, so the conflict must be
	// checked first to keep 409 semantics
	case errors.Is(err, shortener.ErrCodeExists):
		s.m.Shorten.Inc(map[string]string{"status": "409"})
		writeErrEither(w, r, http.StatusConflict, "custom code already in use")
		return
	case errors.Is(err, shortener.ErrValidation):
		s.m.Shorten.Inc(map[string]string{"status": "400"})
		writeErrEither(w, r, http.StatusBadRequest, err.Error())
		return
	case clientMissing(r, err):
		return
	case err != nil:
		s.m.Shorten.Inc(map[string]string{"status": "500"})
		s.log.Error("shorten failed", "err", err)
		writeErrEither(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	s.m.Shorten.Inc(map[string]string{"status": "201"})
	now := time.Now()
	// htmx form: return the row HTML so the table updates without JS glue
	if r.Header.Get("HX-Request") == "true" {
		s.renderFragment(w, "row", s.toLinkJSON(l, s.cfg.BaseURL, requesterIP, now))
		return
	}
	writeJSON(w, http.StatusCreated, s.toLinkJSON(l, s.cfg.BaseURL, requesterIP, now))
}

// handleListLinks answers GET /api/links?limit=&offset= (200).
func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request) {
	limit := clampInt(parseQueryInt(r, "limit", 50), 1, 200)
	offset := max(0, parseQueryInt(r, "offset", 0))

	items, err := s.st.ListLinks(r.Context(), limit, offset)
	if err != nil {
		s.log.Error("list links failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	total, err := s.st.CountLinks(r.Context())
	if err != nil {
		s.log.Error("count links failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]linkJSON, 0, len(items))
	now := time.Now()
	for _, l := range items {
		// /api/links is a public listing: no IP is ever the viewer's own here
		out = append(out, s.toLinkJSON(l, s.cfg.BaseURL, "", now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "total": total, "limit": limit, "offset": offset,
	})
}

// handleGetLink answers GET /api/links/{code} (200/404).
func (s *Server) handleGetLink(w http.ResponseWriter, r *http.Request) {
	l, err := s.st.GetLink(r.Context(), r.PathValue("code"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "link not found")
			return
		}
		if clientMissing(r, err) {
			return
		}
		s.log.Error("get link failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, s.toLinkJSON(l, s.cfg.BaseURL, clientIP(r), time.Now()))
}

// handleDeleteLink allows only the link's creator IP to delete (204/403/404).
func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.st.GetLink(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "link not found")
			return
		}
		if clientMissing(r, err) {
			return
		}
		s.log.Error("delete get failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// ownership check succeeds only when the creator's ip matches this client
	if l.CreatorIP != clientIP(r) {
		writeErr(w, http.StatusForbidden, "only creator can delete")
		return
	}
	ok, err := s.st.DeleteLink(r.Context(), code)
	if err != nil {
		s.log.Error("delete link failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRedirect answers GET /r/{code} with a 302 (404/410 for missing links);
// the click lands in a buffered queue so redirects stay latency-independent.
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.st.GetLink(r.Context(), code)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.m.Redirect.Inc(map[string]string{"status": "404"})
			writeErr(w, http.StatusNotFound, "link not found")
		case clientMissing(r, err):
			// caller hung up mid-lookup: nobody receives a status anyway
			return
		default:
			s.m.Redirect.Inc(map[string]string{"status": "500"})
			s.log.Error("redirect lookup failed", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if l.Expired(time.Now()) {
		s.m.Redirect.Inc(map[string]string{"status": "410"})
		writeErr(w, http.StatusGone, "link expired")
		return
	}

	s.m.Redirect.Inc(map[string]string{"status": "302"})
	s.enqueueClick(r, l.ID)
	w.Header().Set("Cache-Control", "no-store") // counters must not be lost to caches
	w.Header().Set("Location", l.URL)
	w.WriteHeader(http.StatusFound)
}

// enqueueClick hands the redirect event to the async pipeline without ever
// blocking the response. A saturated queue drops the event loudly: a loss
// counter keeps analytics honest instead of silently incomplete.
func (s *Server) enqueueClick(r *http.Request, linkID int64) {
	c := store.Click{
		LinkID:    linkID,
		Ts:        time.Now().UTC(),
		IP:        clientIP(r),
		UserAgent: truncateStr(r.UserAgent(), 512),
		Referer:   truncateStr(r.Referer(), 2048),
	}
	if s.an.Ingest(c) {
		s.m.Clicks.Inc(nil)
		return
	}
	s.m.ClicksDropped.Inc(nil)
	s.log.Warn("click queue full, dropping event", "code", linkID)
}

// parseQueryInt reads an int query param with a fallback default.
func parseQueryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// clampInt keeps lo ≤ n ≤ hi.
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
