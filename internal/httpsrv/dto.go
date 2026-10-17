package httpsrv

import (
	"encoding/json"
	"net/http"
	"time"
)

// writeErr answers with a JSON {"error": msg} body.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON answers with a JSON body; nothing is written after headers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// handleHealthz powers checks from load balancers.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMetrics serves the Prometheus exposition text.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if n, err := s.st.CountActiveLinks(r.Context(), time.Now()); err == nil {
		s.m.ActiveLinks.Set(float64(n))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.m.Collect()))
}
