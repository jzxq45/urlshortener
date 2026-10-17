# Go URL Shortener

[![Go Version][go-version-badge]](#Requirements)
[![License: MIT][license-badge]](LICENSE)
[![Build Status][build-badge]](.github/workflows/ci.yml)

[go-version-badge]: https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white
[license-badge]: https://img.shields.io/badge/License-MIT-blue
[build-badge]: https://img.shields.io/github/actions/workflow/status/jzxq45/urlshortener/ci.yml?branch=main&label=CI

A high-performance URL shortener with click analytics, QR codes and a web interface — built on the Go standard library with a SQLite backend.

![Screenshot](docs/screenshot.png)

## Features

- **Base62 short codes** — 7-character codes generated from `crypto/rand`, with automatic collision retry (≈3.5e12 keyspace).
- **Custom codes** — pick your own slug (`[A-Za-z0-9_-]{3,32}`), guarded against reserved words.
- **TTL expiry** — links can auto-expire (`expires_at`), expired links answer `410 Gone`.
- **Rate limiter** — per-IP token bucket on link creation (`429` + `Retry-After`).
- **Click analytics** — total/unique clicks, 24h/7d/30d windows, top referers, daily & hourly series, CSV export, 90-day retention.
- **QR codes** — `GET /qr/{code}.png` renders cached PNG QR codes (LRU cache).
- **Prometheus metrics** — a self-contained text-format exporter at `/metrics` (no client_golang).
- **Web UI** — htmx forms, Chart.js charts, dark theme; zero JavaScript build step.
- **Async click recording** — redirects never wait on database writes (buffered queue with graceful drain).

## Quick Start

### Run with Go

```sh
go run ./cmd/server
```

The server starts on `http://localhost:8080` using `config.yaml` defaults.
All settings can be overridden with `SHORTENER_*` environment variables:

```sh
SHORTENER_LISTEN=0.0.0.0:8080 SHORTENER_BASE_URL=http://localhost:8080 go run ./cmd/server
```

### Run with Docker Compose

```sh
docker compose up -d
```

or build and run the container directly:

```sh
docker build -t urlshortener .
docker run -p 8080:8080 -v urlshortener-data:/data \
  -e SHORTENER_BASE_URL=http://localhost:8080 urlshortener
```

> `base_url` must match the public origin of the deployment — it is used to render short URLs and QR codes.

## API Reference

Full reference: [docs/API.md](docs/API.md).

### Create a short link

```sh
curl -X POST http://localhost:8080/api/shorten \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/very/long/page?query=1", "ttl_seconds": 604800}'
```

Response `201 Created`:

```json
{
  "code": "mWP16US",
  "short_url": "http://localhost:8080/r/mWP16US",
  "expires_at": "2026-10-24T02:09:42Z",
  "custom_code": false,
  "clicks": 0
}
```

Use `custom_code` to pick the slug; a taken slug answers `409 Conflict`.

### Follow a short link

```sh
curl -i http://localhost:8080/r/mWP16US
```

```
HTTP/1.1 302 Found
Location: https://example.com/very/long/page?query=1
Cache-Control: no-store
```

Unknown codes answer `404`, expired links answer `410 Gone`. Each follow is
recorded asynchronously as a click event.

## Architecture

```
            ┌──────────────────────────────────────────────┐
            │                    Server                    │
            │                                              │
  Client ──►│  GET /r/{code}      POST /api/shorten        │
   browser  │  GET /qr/{code}     GET /api/links/{code}    │
   curl &   │  GET /metrics       GET /{code}  (web UI)   │
 Prometheus │                                              │
            │       net/http router + middleware           │
            │       (rate limit, recovery, metrics)        │
            │                     │                        │
            │              internal/store                  │
            │   links + clicks =========► SQLite (WAL)     │
            └──────────────┬───────────────────────────────┘
                           │
                  click events recorded asynchronously
                  (buffered queue, drained by a worker)
```

Components (see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full picture):

- `cmd/server` — process entry point, lifecycle, graceful shutdown.
- `internal/httpsrv` — routing, middleware, HTTP handlers, HTML UI.
- `internal/shortener` — code generation and creation rules.
- `internal/store` — SQLite persistence, WAL mode, migrations.
- `internal/analytics` — async click consumer and retention sweeps.
- `internal/qr` — QR PNG rendering with an LRU cache.
- `internal/metrics` — Prometheus text-format exporter.

## Why SQLite

A URL shortener is write-light and read-fronted: a handful of inserts per redirect and a handful of reads per analytics view. For that shape of traffic SQLite wins on every operational axis that matters:

- **Zero-setup persistence** — the database is a single file; no server process, no connection credentials, no network hop between app and data.
- **WAL mode** — concurrent readers never block behind the (single) writer, which matches the redirect path exactly: many lookups, few inserts.
- **CGO=0 builds** — `modernc.org/sqlite` is a pure-Go driver, so the binary compiles with `CGO_ENABLED=0` and drops into a `FROM scratch` container of a few megabytes. No C toolchain in CI, no libc mismatches.
- **Operational simplicity** — backups are file copies; there is nothing to provision, monitor or scale separately. When the workload outgrows SQLite, `internal/store` is the seam to swap the storage engine without touching the HTTP or domain layers.

If you need multi-node writes, keep SQLite per node plus a replication tool, or replace `internal/store` — the rest of the codebase does not care which.

## Documentation

- [docs/API.md](docs/API.md) — endpoints reference
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — design and data model
- [docs/DEPLOY.md](docs/DEPLOY.md) — deployment and operations
- [CONTRIBUTING.md](CONTRIBUTING.md) — development setup and style

## License

Distributed under the [MIT License](LICENSE).
