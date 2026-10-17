// Package metrics implements a minimal Prometheus text-format registry:
// labelled counters, gauges and one histogram — no client_golang dependency.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// HistogramBuckets are the defaults Prometheus client uses for request sizes.
var HistogramBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// labelKey joins label values in fixed order; labels are known per metric.
func labelKey(cols []string, labels map[string]string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = labels[c]
	}
	return strings.Join(parts, "\x00")
}

// labelBytes escapes a label value for the exposition format.
func labelBytes(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// CounterVec is a counter split by label columns.
type CounterVec struct {
	name, help string
	cols       []string
	mu         sync.Mutex
	series     map[string]*float64
}

func newCounterVec(name, help string, cols ...string) *CounterVec {
	return &CounterVec{name: name, help: help, cols: cols, series: map[string]*float64{}}
}

// Add adds v to the series for the given labels (missing labels render "").
func (c *CounterVec) Add(labels map[string]string, v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := labelKey(c.cols, labels)
	f, ok := c.series[k]
	if !ok {
		f = new(float64)
		c.series[k] = f
	}
	*f += v
}

// Inc is Add(labels, 1).
func (c *CounterVec) Inc(labels map[string]string) { c.Add(labels, 1) }

// Gauge is a last-value metric.
type Gauge struct {
	name, help string
	mu         sync.Mutex
	v          float64
}

// Set updates the gauge value.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.v = v
	g.mu.Unlock()
}

// histSample holds per-label histogram state.
type histSample struct {
	counts []uint64 // cumulative counts per bucket
	sum    float64
	n      uint64
}

// HistogramVec is a histogram split by a single label column (path).
type HistogramVec struct {
	name, help, col string
	mu              sync.Mutex
	series          map[string]*histSample
}

func newHistogramVec(name, help, col string) *HistogramVec {
	return &HistogramVec{name: name, help: help, col: col, series: map[string]*histSample{}}
}

// Observe records one duration in seconds for the given label value.
func (h *HistogramVec) Observe(label string, v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[label]
	if !ok {
		s = &histSample{counts: make([]uint64, len(HistogramBuckets))}
		h.series[label] = s
	}
	// linear upper-bound scan; bucket count is small
	for i, b := range HistogramBuckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.n++
	s.sum += v
}

// Registry owns the metric set rendered at /metrics.
type Registry struct {
	mu sync.Mutex

	HTTPRequests *CounterVec // http_requests_total{method,path,status}
	Shorten      *CounterVec // shorten_total{status}
	Redirect     *CounterVec // redirect_total{status}
	Clicks       *CounterVec // clicks_total{}
	ActiveLinks  *Gauge      // active_links
	Duration     *HistogramVec
}

// New wires the registry with the standard metric names.
func New() *Registry {
	return &Registry{
		HTTPRequests: newCounterVec("http_requests_total", "Total HTTP requests processed.", "method", "path", "status"),
		Shorten:      newCounterVec("shorten_total", "Total /api/shorten outcomes.", "status"),
		Redirect:     newCounterVec("redirect_total", "Total redirect outcomes.", "status"),
		Clicks:       newCounterVec("clicks_total", "Total recorded redirect clicks."),
		ActiveLinks:  &Gauge{name: "active_links", help: "Links that are not expired."},
		Duration:     newHistogramVec("http_request_duration_seconds", "Handler latency in seconds.", "path"),
	}
}

// Collect renders the full exposition text. Single-process ownership means
// one Collect call never races with itself.
func (r *Registry) Collect() string {
	var b strings.Builder
	writeCounterVec(&b, r.HTTPRequests)
	writeCounterVec(&b, r.Shorten)
	writeCounterVec(&b, r.Redirect)
	writeCounterVec(&b, r.Clicks)
	writeGauge(&b, r.ActiveLinks)
	writeHistogram(&b, r.Duration)
	return b.String()
}

func writeCounterVec(b *strings.Builder, c *CounterVec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := strings.Split(k, "\x00")
		var lb []string
		for i, col := range c.cols {
			// manual quoting: %q would escape the already-escaped value again
			lb = append(lb, col+`="`+labelBytes(vals[i])+`"`)
		}
		label := ""
		if len(c.cols) > 0 {
			label = "{" + strings.Join(lb, ",") + "}"
		}
		// strconv avoids %e style floats for large counters.
		fmt.Fprintf(b, "%s%s %s\n", c.name, label, strconv.FormatFloat(*c.series[k], 'f', -1, 64))
	}
}

func writeGauge(b *strings.Builder, g *Gauge) {
	g.mu.Lock()
	v := g.v
	g.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, g.help, g.name, g.name, strconv.FormatFloat(v, 'f', -1, 64))
}

func writeHistogram(b *strings.Builder, h *HistogramVec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		label := `path="` + labelBytes(k) + `"`
		for i, bucket := range HistogramBuckets {
			// counts are cumulative per le bound
			fmt.Fprintf(b, "%s_bucket{%s,le=%q} %d\n", h.name, label, formatBucket(bucket), s.counts[i])
		}
		fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n", h.name, label, s.n)
		fmt.Fprintf(b, "%s_sum{%s} %s\n", h.name, label, strconv.FormatFloat(s.sum, 'f', -1, 64))
		fmt.Fprintf(b, "%s_count{%s} %d\n", h.name, label, s.n)
	}
}

func formatBucket(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
