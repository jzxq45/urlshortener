package metrics

import (
	"strings"
	"testing"
)

func TestRegistryCollect(t *testing.T) {
	t.Parallel()
	r := New()
	r.HTTPRequests.Inc(map[string]string{"method": "GET", "path": "/r/{code}", "status": "302"})
	r.HTTPRequests.Inc(map[string]string{"method": "GET", "path": "/r/{code}", "status": "302"})
	r.HTTPRequests.Inc(map[string]string{"method": "GET", "path": "/r/{code}", "status": "404"})
	r.Shorten.Inc(map[string]string{"status": "201"})
	r.Clicks.Inc(nil)
	r.ActiveLinks.Set(3)
	r.Duration.Observe("/r/{code}", 0.02)
	r.Duration.Observe("/r/{code}", 0.3)

	out := r.Collect()
	for _, want := range []string{
		"# TYPE http_requests_total counter",
		`http_requests_total{method="GET",path="/r/{code}",status="302"} 2`,
		`http_requests_total{method="GET",path="/r/{code}",status="404"} 1`,
		`shorten_total{status="201"} 1`,
		"clicks_total 1",
		"active_links 3",
		`http_request_duration_seconds_bucket{path="/r/{code}",le="0.05"} 1`,
		`http_request_duration_seconds_bucket{path="/r/{code}",le="+Inf"} 2`,
		`http_request_duration_seconds_count{path="/r/{code}"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestCounterVecLabelEscaping(t *testing.T) {
	t.Parallel()
	r := New()
	r.HTTPRequests.Inc(map[string]string{"method": "G\"T", "path": `a\`, "status": "200"})
	out := r.Collect()
	if !strings.Contains(out, `method="G\"T"`) || !strings.Contains(out, `path="a\\"`) {
		t.Errorf("labels not escaped: %s", out)
	}
}
