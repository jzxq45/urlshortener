# Sprint Report — URL Shortener (Go)

Date: 2026-10-17 (continuation session)

## Status: all 12 phases complete ✅

Build gate passes: `go build ./...`, `go vet ./...`, `go test -race -timeout 600s ./...` — all green.

## Phases

| Phase | Name | Status | Commit |
| --- | --- | --- | --- |
| 1 | skeleton (config, graceful shutdown) | done (prev run) | `92f5ea6` |
| 2 | store (SQLite, WAL, migrations) | done (prev run) | `26f23e1` |
| 3 | shortener (Base62, crypto/rand, custom) | done (prev run) | `2a7a7ad` |
| 4 | HTTP API + rate limit | done (prev run) | `894883d` |
| 5 | web UI (htmx, charts, dark theme) | done (prev run) | `72db2b3` |
| 6 | analytics (stats, CSV, retention) | done (prev run) | `5ca7254` |
| 7 | QR (renderer, LRU, endpoint) | **completed this run** | `fec2270` |
| 8 | metrics (own Prometheus exporter) | verified + fixed + tested | `8cf987a` |
| 9 | tests (store/limiter/config/handlers) | done | `9ec2e38` |
| 10 | CI (build, vet, race, lint, docker smoke) | done | `228c323` |
| 11 | Docker (multi-stage, CGO=0, compose) | done | `f6b7ec3`, `c31aa5e` |
| 12 | docs (README, ARCHITECTURE, API, DEPLOY) | done | `cb3d41e` |

## What this session did

### Fixed at start (phase 7 half-done)
- `server.go` referenced `s.handleQR` which did not exist — **build was broken**.
- Added `handleQR` (link lookup → 404/410, PNG render, 1h public cache header).
- Removed **duplicate route registrations** (`GET /r/{code}`, `GET /metrics` registered twice — would have panicked at startup).
- Added missing `github.com/skip2/go-qrcode` module dependency.

### Real bugs found (mostly by the new tests)
1. **Startup panic**: `GET /qr/{code}.png` pattern is rejected by Go's ServeMux (wildcard with literal suffix). Fixed to `GET /qr/{code}` + `.png` trimming in the handler.
2. **409→400**: handler checked `ErrValidation` before `ErrCodeExists`, so custom-code collisions returned 400 instead of 409. Fixed case order.
3. **Invalid Prometheus output**: `clicks_total` had an empty label name (`clicks_total{=""}`) and label values were double-quoted/escaped via `%q`. Exporter now emits valid exposition.
4. **Flaky click-loss path noted** (not changed): `Workers` inserts clicks with the loop ctx; a cancel that races an in-flight insert drops that event. Normal operation and graceful shutdown are unaffected; documented in DEPLOY.md.

### Added
- Tests: `store` (CRUD, aggregates with midnight-safe fixed timestamps, retention, expiry), `httpsrv` (full-chain httptest: shorten/redirect/rate-limit/ownership/409/410/CORS/htmx/QR/metrics/pages/16KB cap), `limiter` (burst, refill, prune), `config` (defaults, file+env, validation), `metrics` (exposition text, escaping).
- `.github/workflows/ci.yml` (build, vet, `-race` tests, golangci-lint, Docker smoke test).
- `Dockerfile` (multi-stage golang→scratch, CGO_ENABLED=0, uid 65534, pre-created writable `/data`), `compose.yaml`, `.dockerignore`.
- `README.md`, `docs/ARCHITECTURE.md`, `docs/API.md`, `docs/DEPLOY.md`.

## Not done / known gaps
- No tests for `analytics`, `qr` packages (covered indirectly via handlers).
- Code generation modulo bias on `%62` (8/256) documented, not rejection-sampled.
- Lint job (golangci-lint) not configured with a custom `.golangci.yml` — runs default ruleset.
- CI has not actually run on GitHub (no remote configured in this environment).

## How to run

```sh
go build -o bin/server ./cmd/server
./bin/server                 # http://localhost:8080, config.yaml
# or
docker compose up -d
```
