# URL Shortener

A self-hosted URL shortener in Go with click analytics, QR codes and a web UI.

Built with the standard library first: `net/http` with Go 1.22+ method/wildcard
routing, `html/template` for the UI, `log/slog` for structured logs, and
SQLite (pure-Go `modernc.org/sqlite` driver, CGO-free) for storage. The only
runtime dependencies are `github.com/google/uuid` and `github.com/skip2/go-qrcode`.

## Features

- **Short links** — 7-char base62 codes from `crypto/rand`, optional custom
  codes (`[A-Za-z0-9_-]{3,32}`, reserved words guarded), optional TTL expiry.
- **Redirects** — `GET /r/{code}` → 302, 404 unknown, 410 expired. Clicks are
  recorded asynchronously via a buffered queue, so redirects don't pay for DB
  writes.
- **Analytics** — per-link stats (total, unique IPs, 24h/7d/30d windows, top
  referers, recent clicks, daily/hourly series), CSV export, and a daily
  retention sweep that deletes clicks older than `retention_days` (default 90).
- **Web UI** — htmx-powered form and tables, Chart.js charts, dark theme, no
  build step (assets embedded in the binary).
- **QR codes** — `GET /qr/{code}.png` renders PNGs with a small LRU cache.
- **Metrics** — `/metrics` serves the Prometheus text format from a minimal
  in-process exporter (no client_golang).
- **Hardening** — per-IP token-bucket rate limit on creation, same-origin
  guard for API calls, panic recovery with request IDs, 16 KB body cap.
- **Ops** — config via YAML + `SHORTENER_*` env overrides, graceful shutdown
  with click-queue drain, JSON logs, `/healthz`.

## Quick start

```sh
go build -o bin/server ./cmd/server
./bin/server
# open http://localhost:8080
```

## Run with Docker

```sh
docker compose up -d
```

or directly:

```sh
docker build -t urlshortener .
docker run -p 8080:8080 -e SHORTENER_BASE_URL=http://localhost:8080 urlshortener
```

## Configuration

File `config.yaml` (all keys optional) with `SHORTENER_*` env overrides:

| Key                 | Env                            | Default               |
| ------------------- | ------------------------------ | --------------------- |
| `listen`            | `SHORTENER_LISTEN`             | `0.0.0.0:8080`        |
| `db_path`           | `SHORTENER_DB_PATH`            | `urlshortener.db`     |
| `base_url`          | `SHORTENER_BASE_URL`           | `http://localhost:8080` |
| `retention_days`    | `SHORTENER_RETENTION_DAYS`     | `90`                  |
| `rate_limit_per_min`| `SHORTENER_RATE_LIMIT_PER_MIN` | `10`                  |
| `log_level`         | `SHORTENER_LOG_LEVEL`          | `info`                |

## Tests

```sh
go test -race ./...
```

## Documentation

- [ARCHITECTURE.md](docs/ARCHITECTURE.md) — design, data model, components.
- [API.md](docs/API.md) — HTTP endpoints reference.
- [DEPLOY.md](docs/DEPLOY.md) — deployment and operations guide.

## Status / project log

See [REPORT.md](REPORT.md).
