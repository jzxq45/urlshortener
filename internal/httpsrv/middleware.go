package httpsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ctxKey indexes context values without clashing with foreign keys.
type ctxKey int

const ctxKeyReqLog ctxKey = iota

// middlewares wraps h with the standard chain, outermost first.
func (s *Server) middlewares(h http.Handler) http.Handler {
	for _, mw := range []func(http.Handler) http.Handler{
		s.secureHeaders,
		s.recoverPanics,
		setRequestID(s.log),
		s.logRequests,
		s.corsSameOrigin,
		s.realIP,
		s.recordMetrics,
	} {
		h = mw(h)
	}
	return h
}

// secureHeaders stamps baseline protections on every response — first in the
// chain so even panic/error paths carry them.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// nosniff: JSON error bodies must never be reinterpreted as HTML
		h.Set("X-Content-Type-Options", "nosniff")
		// the UI is not framed anywhere, neither by us nor by attackers
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// conservatively closed CSP: scripts/styles are same-origin only
		// (htmx + app.js are served from /static)
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// setRequestID assigns a request id and request-scoped logger.
func setRequestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := uuid.NewString()
			logger := base.With("request_id", id)
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyReqLog, logger)))
		})
	}
}

func loggerFrom(r *http.Request) *slog.Logger {
	if lg, ok := r.Context().Value(ctxKeyReqLog).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}

// statusRecorder captures the status code; untouched handlers count as 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wrote {
		sr.status, sr.wrote = code, true
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wrote {
		sr.status, sr.wrote = http.StatusOK, true
	}
	return sr.ResponseWriter.Write(b)
}

// Flush forwards so streaming stays possible under logging.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests emits one structured line per request after completion.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		loggerFrom(r).Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"dur_ms", float64(time.Since(start).Microseconds())/1000,
			"ip", clientIP(r),
		)
	})
}

// recoverPanics converts handler panics into 500 responses with a stack log.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				route := r.Pattern
				if route == "" {
					route = "unmatched"
				}
				loggerFrom(r).Error("panic recovered",
					"err", panicText(rec),
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
				)
				s.m.HTTPRequests.Inc(map[string]string{
					"method": r.Method, "path": route, "status": "500",
				})
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// corsSameOrigin rejects cross-origin API use; navigation requests have no
// Origin header and pass untouched.
func (s *Server) corsSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !originMatches(o, r.Host) {
			writeErr(w, http.StatusForbidden, "cross-origin not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originMatches compares the origin's host part against the request host.
func originMatches(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == host
}

// realIP rewrites RemoteAddr from X-Forwarded-For, but only when the request
// came from a configured trusted proxy; see config.TrustedProxies. Untrusted
// peers keep their socket address, so header forgery can neither defeat the
// rate limiter nor fake link ownership.
func (s *Server) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			peer = host
		}
		if ipTrusted(peer, s.trusted) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				first := strings.TrimSpace(strings.Split(xff, ",")[0])
				if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
					r.RemoteAddr = ip.String()
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ipTrusted matches ip against the configured exact-IP/CIDR allowlist.
func ipTrusted(ip string, trusted []net.IPNet) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// parseTrustedProxies compiles config entries (IP or CIDR) into a match list.
func parseTrustedProxies(entries []string) []net.IPNet {
	out := make([]net.IPNet, 0, len(entries))
	for _, e := range entries {
		// config validation rejects malformed entries before we get here
		if ip := net.ParseIP(e); ip != nil {
			bits := net.IPv4len * 8
			if ip.To4() == nil {
				bits = net.IPv6len * 8
			}
			out = append(out, net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, *n)
		}
	}
	return out
}

// recordMetrics emits request count and latency after the mux picked a route.
func (s *Server) recordMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		// spare the self-scrape from its own counters
		if route == "/metrics" {
			return
		}
		s.m.HTTPRequests.Inc(map[string]string{
			"method": r.Method,
			"path":   route,
			"status": strconv.Itoa(sr.status),
		})
		s.m.Duration.Observe(route, time.Since(start).Seconds())
	})
}

// panicText renders any recovered value into a string.
func panicText(rec any) string {
	if err, ok := rec.(error); ok {
		return err.Error()
	}
	return fmt.Sprintf("panic: %v", rec)
}

// clientIP strips the port from RemoteAddr.
func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || ip == "" {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return ip
}
