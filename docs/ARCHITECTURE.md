# Architecture

## Overview

```
                 ┌─────────────────────────────── cmd/server/main.go ───────────────────────┐
                 │  config → store.Open → shortener.New → httpsrv.New → errgroup lifecycle   │
                 └───────────────────────────────────────────────────────────────────────────┘
                                                   │
   net/http ServeMux (Go 1.22 method+wildcard) ─────┤
                                                   │
┌───────────────────────────── internal/httpsrv ─────────────────────────────────────────┐
│  middleware: recoverPanics → requestID → log → sameOrigin → realIP(XFF) → metrics      │
│  handlers.go  API JSON (shorten/list/get/delete, redirect)                             │
│  stats.go     link stats JSON, CSV export, QR PNG endpoint                             │
│  pages.go     web UI: index / links / link detail / stats / 404 (+ row fragment)       │
│  dto.go       writeErr/writeJSON, /healthz, /metrics                                   │
│  limiter.go   per-IP token bucket (self-pruning, no janitor goroutine)                 │
│  templates/ static/  (html/template sets + htmx, Chart.js, dark theme)                 │
└────────────────────────────────────────────────────────────────────────────────────────┘
        │                          │
        ▼                          ▼
 internal/shortener        internal/store           internal/qr        internal/metrics
 codes, validation,   →   links/clicks tables,   PNG renderer +      Prometheus text
 collision retry          WAL, migrations        256-entry LRU       registry (no
                                                                     client_golang)
        │
 internal/analytics — async click consumer (Server.Workers), daily retention sweep
                      (analytics.Service.Start), CSV streaming (ExportCSV)
```

## Data model (SQLite, WAL)

`links`: `id`, `code` (UNIQUE), `url`, `created_at` (unix), `expires_at`
(nullable), `clicks` (denormalized fast counter), `creator_ip`, `custom_code`.

`clicks`: `id`, `link_id` → links(id) ON DELETE CASCADE, `ts` (unix),
`ip`, `user_agent`, `referer`.

Connection split: a **single** write connection (SQLite serializes writers
internally; `busy_timeout` turns contention into queueing) and a **read pool**
(20 conns in `query_only` mode) exploiting WAL parallel readers. All
timestamps are stored as unix seconds and always treated as UTC.

## Request flows

### Create (POST /api/shorten)
1. per-IP token bucket check → 429.
2. 16 KB body cap, strict JSON decode.
3. `shortener.Create`: validate URL (http/https, host, ≤2048), custom code
   regex + reserved-word guard, generate 7-char base62 via `crypto/rand`
   (`%62` on bytes: 8/256 bias documented, not worth rejection sampling),
   retry up to 5 collisions (`62^7 ≈ 3.5e12` keyspace).
4. insert; UNIQUE violation → 409.

### Redirect (GET /r/{code})
1. lookup; 404 missing, 410 expired.
2. enqueue click (ip/UA/referer truncated to sane caps; *drop on full queue* —
   redirect latency beats complete analytics).
3. 302 with `Cache-Control: no-store` so intermediaries don't eat clicks.

### Click persistence
`Workers(ctx)` in an errgroup goroutine drains the buffered queue into
`clicks` and bumps the cached `links.clicks`. On shutdown the worker first
drains with a detached `context.Background()` so queued clicks still land
after the mux stops.

### Retention
`analytics.Service.Start(ctx)` runs a daily sweep deleting clicks older than
`retention_days`. Links themselves outlive their clicks (the hits counter is
preserved).

### QR (GET /qr/{code}.png)
Wildcard patterns may not carry suffixes (`.png` after `{code}` panics
ServeMux registration), so the route is `GET /qr/{code}` and the handler
trims the `.png` suffix. `qr.Renderer` builds 256px PNG, level M, cached in a
hand-written LRU (256 bytes → up to 1000 entries) with 1h TTL so base-url or
domain changes eventually re-render.

## Metrics

`internal/metrics` implements counters (label vecs), one gauge and one
histogram rendered in Prometheus text format, escaping label values correctly
and never emitting empty label names. The `/metrics` scrape itself is excluded
from its own counters.

## Security notes

- `X-Forwarded-For` leftmost entry is trusted only as a parsed IP — deploy
  behind a proxy that strips client-supplied XFF (see docs/DEPLOY.md).
- Same-origin guard rejects API calls with a foreign `Origin` header
  (browsers send Origin on cross-site fetches; plain navigation has none).
- Link deletion is restricted to links whose `creator_ip` matches the client.
- No panic in request path: middleware converts panics into 500 + stack log.

## Why these choices

- **stdlib-first** keeps the dependency tree auditable; SQLite as a single
  file fits the scale (a URL shortener's analytics are write-light).
- **Pure-Go driver** keeps `CGO_ENABLED=0` → tiny scratch images, easy
  cross-compilation.
- **Async clicks** decouple the hot redirect path from disk latency, with an
  explicit, documented trade-off (drops under saturation).
