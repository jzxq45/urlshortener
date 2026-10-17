package httpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"urlshortener/internal/config"
	"urlshortener/internal/metrics"
	"urlshortener/internal/qr"
	"urlshortener/internal/shortener"
	"urlshortener/internal/store"
)

// newTestServer builds a full server against a temp db.
func newTestServer(t *testing.T, ratePerMin int) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "test.db")
	cfg.RateLimitPerMin = ratePerMin
	cfg.BaseURL = "http://test.local" // no trailing slash, as validate() demands
	// httptest sends requests from 192.0.2.1; trust it so tests can exercise
	// the XFF path the way a real reverse proxy would
	cfg.TrustedProxies = []string{"192.0.2.1/32", "127.0.0.0/8"}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(cfg, db, shortener.New(db), slog.New(slog.NewTextHandler(os.Stderr, nil)), metrics.New())
}

// do executes one request against the server's full middleware chain.
func do(t *testing.T, s *Server, method, path string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	resp := w.Result()
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, raw
}

func shorten(t *testing.T, s *Server, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	resp, raw := do(t, s, "POST", "/api/shorten", strings.NewReader(body), headers)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func TestUntrustedXFFIsIgnored(t *testing.T) {
	// regression: a forged X-Forwarded-For must not become the client IP
	t.Parallel()
	cfg := config.Default()
	cfg.BaseURL = "http://test.local"
	cfg.RateLimitPerMin = 100
	// no trusted proxies configured: header must be ignored
	cfg.TrustedProxies = nil

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(cfg, db, shortener.New(db), slog.New(slog.NewTextHandler(os.Stderr, nil)), metrics.New())

	resp, out := shorten(t, s, `{"url":"https://example.com"}`, map[string]string{"X-Forwarded-For": "8.8.8.8"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("shorten = %d", resp.StatusCode)
	}
	code, _ := out["code"].(string)
	// the creator is the socket address; the forged header must be ignored.
	// The requester is the creator itself, so its own IP shows unmasked —
	// masking rules are covered by TestUnmaskedOnlyForCreator.
	resp, raw := do(t, s, "GET", "/api/links/"+code, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"creator_ip":"192.0.2.1"`) {
		t.Errorf("forged XFF became creator_ip: %s", raw)
	}
	var j struct {
		CreatorIP string `json:"creator_ip"`
	}
	_ = json.Unmarshal(raw, &j)
	if j.CreatorIP != "192.0.2.1" {
		t.Errorf("creator_ip = %q, want socket address (XFF ignored)", j.CreatorIP)
	}
}

// TestUnmaskedOnlyForCreator pins the PII rule: full creator IP is visible
// only when the requester is the creator; everyone else gets "hidden".
func TestUnmaskedOnlyForCreator(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	_, out := shorten(t, s, `{"url":"https://example.com"}`, nil)
	code, _ := out["code"].(string)

	resp, raw := do(t, s, "GET", "/api/links/"+code, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get as creator = %d", resp.StatusCode)
	}
	var j struct {
		CreatorIP string `json:"creator_ip"`
	}
	_ = json.Unmarshal(raw, &j)
	if j.CreatorIP != "192.0.2.1" {
		t.Errorf("creator sees own ip: %q, want 192.0.2.1", j.CreatorIP)
	}

	// a different viewer (XFF through a trusted proxy) must get masked value
	resp, raw = do(t, s, "GET", "/api/links/"+code, nil, map[string]string{"X-Forwarded-For": "198.51.100.5"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"creator_ip":"hidden"`) {
		t.Errorf("stranger sees creator ip: %s", raw)
	}

	// public listing: nobody is the creator → all masked
	resp, raw = do(t, s, "GET", "/api/links", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"creator_ip":"hidden"`) {
		t.Errorf("listing leaked creator ip: %s", raw)
	}
}

func TestShortenAndRedirectFlow(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)

	resp, out := shorten(t, s, `{"url":"https://example.com/page"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("shorten status = %d, want 201 (body %v)", resp.StatusCode, out)
	}
	code, _ := out["code"].(string)
	if code == "" {
		t.Fatal("no code in response")
	}
	if out["short_url"].(string) != "http://test.local/r/"+code {
		t.Errorf("short_url = %v", out["short_url"])
	}

	resp, _ = do(t, s, "GET", "/r/"+code, nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "https://example.com/page" {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}

	// the click lands through the async queue: let one Workers pass process
	// it, then stop it
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Workers(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // let the insert complete on a live ctx
	cancel()
	<-done

	resp, raw := do(t, s, "GET", "/api/links/"+code, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"clicks":1`) {
		t.Errorf("clicks not stored: %d %s", resp.StatusCode, raw)
	}
}

func TestWorkersSavesClickAfterCtxCancel(t *testing.T) {
	// regression: worker must not drop an enqueued click when ctx is already
	// canceled (graceful-shutdown drain path)
	t.Parallel()
	s := newTestServer(t, 100)
	code := "wtesttst"
	if err := s.st.CreateLink(context.Background(), &store.Link{Code: code, URL: "https://example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	queueEvent := func() {
		req := httptest.NewRequest("GET", "/r/"+code, nil)
		s.enqueueClick(req, 1)
	}
	queueEvent()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing readable after this point: the drain branch only survives
	done := make(chan struct{})
	go func() { s.Workers(ctx); close(done) }()
	<-done

	var got store.Link
	if err := eventually(t, func() bool {
		var e error
		got, e = s.st.GetLink(context.Background(), code)
		return e == nil && got.Clicks == 1
	}); err != nil {
		t.Fatalf("click lost after cancel: err=%v link=%+v", err, got)
	}
}

// eventually polls cond in fixed intervals for 5s — a deterministic stand-in
// for testify's require.Eventually (dependency banned by project rules).
func eventually(t *testing.T, cond func() bool) error {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		if cond() {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("condition not met within 5s")
		case <-ticker.C:
		}
	}
}

func TestShortenValidation(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)

	cases := []struct {
		name, body, status string
		want               int
	}{
		{"bad json", `{`, "400", http.StatusBadRequest},
		{"bad url", `{"url":"ht!tp://x"}`, "400", http.StatusBadRequest},
		{"missing url", `{"custom_code":"x"}`, "400", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, out := shorten(t, s, c.body, nil)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status=%d want=%d body=%v", c.name, resp.StatusCode, c.want, out)
		}
		if e, _ := out["error"].(string); e == "" {
			t.Errorf("%s: no error message", c.name)
		}
	}
}

func TestCustomCodeCollision(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	body := `{"url":"https://example.com","custom_code":"mydocs"}`
	resp, _ := shorten(t, s, body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first custom create = %d", resp.StatusCode)
	}
	resp, out := shorten(t, s, body, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("collision = %d, want 409", resp.StatusCode)
	}
	if e, _ := out["error"].(string); !strings.Contains(e, "already") {
		t.Errorf("error text = %v", out)
	}
}

func TestShortenRateLimited(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 1)
	resp, _ := shorten(t, s, `{"url":"https://a.com"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first = %d", resp.StatusCode)
	}
	resp, out := shorten(t, s, `{"url":"https://b.com"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second = %d, want 429 (%v)", resp.StatusCode, out)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
}

func TestDeleteOwnership(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	_, out := shorten(t, s, `{"url":"https://example.com"}`, map[string]string{"X-Forwarded-For": "203.0.113.7"})
	code, _ := out["code"].(string)
	if code == "" {
		t.Fatal("no code")
	}

	// a different client ip may not delete
	resp, _ := do(t, s, "DELETE", "/api/links/"+code, nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete as another ip = %d, want 403", resp.StatusCode)
	}
	resp, _ = do(t, s, "DELETE", "/api/links/"+code, nil, map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete as creator = %d, want 204", resp.StatusCode)
	}
	resp, _ = do(t, s, "DELETE", "/api/links/"+code, nil, map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", resp.StatusCode)
	}
}

func TestRedirectExpiredAndMissing(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	db := s.st
	expiredAt := time.Now().UTC().Add(-time.Minute)
	if err := db.CreateLink(context.Background(), &store.Link{
		Code: "gone", URL: "https://example.com",
		CreatedAt: time.Now().UTC().Add(-time.Hour), ExpiresAt: &expiredAt,
	}); err != nil {
		t.Fatal(err)
	}

	resp, _ := do(t, s, "GET", "/r/gone", nil, nil)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("expired redirect = %d, want 410", resp.StatusCode)
	}
	resp, _ = do(t, s, "GET", "/r/nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing redirect = %d, want 404", resp.StatusCode)
	}
}

func TestExpiredQR(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	if _, err := s.qr.PNG("okcode"); err != nil {
		t.Fatal(err)
	}
	// unknown code → 404
	resp, _ := do(t, s, "GET", "/qr/nope.png", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("qr unknown = %d, want 404", resp.StatusCode)
	}
}

func TestQRRendersPNG(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	_, out := shorten(t, s, `{"url":"https://example.com"}`, nil)
	code, _ := out["code"].(string)

	resp, raw := do(t, s, "GET", "/qr/"+code+".png", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("qr = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("content type = %q", ct)
	}
	// PNG magic number  \x89PNG\r\n\x1a\n
	if len(raw) < 8 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Error("body is not a PNG")
	}
}

func TestStatsAndCSV(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	code := "statstest"
	if err := s.st.CreateLink(context.Background(), &store.Link{Code: code, URL: "https://example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.st.InsertClick(context.Background(), store.Click{LinkID: 1, Ts: time.Now().UTC(), IP: "5.5.5.5", Referer: "https://r"}); err != nil {
		t.Fatal(err)
	}

	resp, raw := do(t, s, "GET", "/api/links/"+code+"/stats", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats = %d", resp.StatusCode)
	}
	var st struct {
		TotalClicks int64 `json:"total_clicks"`
		UniqueIPs   int64 `json:"unique_ips"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.TotalClicks != 1 || st.UniqueIPs != 1 {
		t.Errorf("stats = %+v", st)
	}

	resp, raw = do(t, s, "GET", "/api/links/"+code+"/export.csv", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "5.5.5.5") {
		t.Errorf("csv = %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), code) {
		t.Error("csv disposition missing code")
	}
	resp, _ = do(t, s, "GET", "/api/links/unknown/stats", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown stats = %d, want 404", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	do(t, s, "GET", "/healthz", nil, nil)
	resp, raw := do(t, s, "GET", "/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d", resp.StatusCode)
	}
	if !strings.Contains(string(raw), "http_requests_total") ||
		!strings.Contains(string(raw), "active_links") {
		t.Errorf("metrics output incomplete:\n%s", raw)
	}
}

func TestCorsSameOrigin(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	resp, _ := do(t, s, "POST", "/api/shorten", strings.NewReader(`{"url":"https://x"}`),
		map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin = %d, want 403", resp.StatusCode)
	}
	// httptest.NewRequest defaults Host to example.com
	resp, _ = do(t, s, "POST", "/api/shorten", strings.NewReader(`{"url":"https://x"}`),
		map[string]string{"Origin": "http://example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("same origin = %d, want 201", resp.StatusCode)
	}
}

func TestHtmxErrorFragment(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)

	// validation failure through an htmx form: fragment, not JSON; the
	// error text echoes the user-supplied scheme, so escaping is on display
	resp, raw := do(t, s, "POST", "/api/shorten", strings.NewReader(`{"url":"<script>://x"}`),
		map[string]string{"HX-Request": "true"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("htmx error status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	if !strings.Contains(string(raw), "<tr>") || !strings.Contains(string(raw), "invalid input") {
		t.Errorf("fragment missing error row: %s", raw)
	}
	// the message must be HTML-escaped, not raw user input
	if !strings.Contains(string(raw), "&lt;script&gt;") {
		t.Errorf("error text not escaped: %s", raw)
	}
}

func TestHtmxFragmentResponse(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	resp, raw := do(t, s, "POST", "/api/shorten", strings.NewReader(`{"url":"https://example.com"}`),
		map[string]string{"HX-Request": "true"})
	// renderFragment answers 200: the partial replaces a table row client-side
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("htmx shorten = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("htmx response content type = %q", ct)
	}
	if !strings.Contains(string(raw), `data-copy="/r/`) {
		t.Errorf("fragment lacks short url: %s", raw)
	}
}

func TestPagesRender(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	for _, path := range []string{"/", "/links", "/stats"} {
		resp, raw := do(t, s, "GET", path, nil, nil)
		if resp.StatusCode != http.StatusOK || len(raw) == 0 {
			t.Errorf("%s = %d (%d bytes)", path, resp.StatusCode, len(raw))
		}
		if !strings.Contains(string(raw), "</html>") {
			t.Errorf("%s: page not rendered", path)
		}
	}
}

func TestShortenMaxBody(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	big := `{"url":"https://example.com","stuff":"` + strings.Repeat("x", 20*1024) + `"}`
	resp, _ := shorten(t, s, big, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body = %d, want 400", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, 100)
	resp, raw := do(t, s, "GET", "/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "ok") {
		t.Errorf("healthz = %d %s", resp.StatusCode, raw)
	}
}

func TestHandlerBuilds(t *testing.T) {
	// why: Go's ServeMux panics on duplicate patterns; this guards the
	// regression that once broke startup at first request
	t.Parallel()
	_ = newTestServer(t, 100).Handler()
	_ = uuid.NewString()
	_ = fmt.Sprint(qr.CacheCap)
}
