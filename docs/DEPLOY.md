# Deployment

## Build

`CGO_ENABLED=0` always — the SQLite driver is pure Go:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o server ./cmd/server
```

The Docker image is a Scratch layer running as uid 65534 (nobody).

## Local / VM

1. Copy the binary and a `config.yaml` (or rely on env vars and defaults).
2. Run `./server -config /etc/urlshortener/config.yaml`.
3 systemd unit:

```ini
[Unit]
Description=URL shortener
After=network-online.target

[Service]
ExecStart=/usr/local/bin/server -config /etc/urlshortener/config.yaml
Environment=SHORTENER_DB_PATH=/var/lib/urlshortener/urlshortener.db
User=urlshortener              # owns /var/lib/urlshortener
Restart=on-failure
# graceful shutdown drains the click queue, give it room
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

## Configuration

| Env                       | Meaning                                          |
| ------------------------- | ------------------------------------------------ |
| `SHORTENER_BASE_URL`      | Public origin used when rendering short URLs; must be http(s):// and without a trailing slash. |
| `SHORTENER_LISTEN`        | `host:port` for the HTTP listener.               |
| `SHORTENER_DB_PATH`       | SQLite file (WAL sidecars live next to it).      |
| `SHORTENER_RETENTION_DAYS`| Click rows older than this are deleted daily.    |
| `SHORTENER_RATE_LIMIT_PER_MIN` | Shorten requests per client IP per minute.  |
| `SHORTENER_LOG_LEVEL`     | debug / info / warn / error (JSON, stdout).      |

## Behind a proxy

- Reverse-proxy `Host` and `X-Forwarded-For` (leftmost entry is trusted as the
  client IP for rate limiting, link ownership and analytics). Make sure the
  proxy overwrites any client-supplied `XFF`.
- Terminate TLS at the proxy and set `BASE_URL` to the https origin.
- The UI/redirects don't need sticky sessions; `/r/{code}` sends
  `Cache-Control: no-store`, don't cache it.

## Docker / compose

```sh
docker compose up -d
```

- `dbdata` volume persists `SHORTENER_DB_PATH=/data/urlshortener.db`.
- SQLite keeps WAL sidecars in the same directory; a bind mount works too.
- Backups: stop-free `sqlite3` backup or simple `.backup`:

```sh
docker compose exec server true  # no shell in scratch; run backup on host
sqlite3 /var/lib/docker/volumes/<dbdata>/_data/urlshortener.db ".backup 'backup.db'"
```

(simpler: `docker compose stop server`, copy the db, start again — the write
window is a single section).

## Health / monitoring

- Liveness/readiness: `GET /healthz`.
- Prometheus: `GET /metrics` (text format 0.0.4; ready for a `prometheus.io/scrape` annotation).
- Logs: one JSON line per request on stdout, plus panic stacks.

## Scaling notes

Single node is the intended shape (SQLite WAL): one writer connection
serializes inserts; reads scale through the WAL pool. For horizontal scaling
you would need to move storage off SQLite — the store package is the seam.

## Housekeeping

- Clicks older than `retention_days` (default 90) are deleted once a day.
  The deleted count appears in logs (`also`, adjust via config).
- The DB file never exceeds a few MB per million clicks in practice;
  `PRAGMA wal_checkpoint(TRUNCATE)` after a big delete keeps windows small.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| 429 on shorten | rate limit (`SHORTENER_RATE_LIMIT_PER_MIN`), or shared proxy IP |
| 403 on API call | foreign `Origin` header — use the same origin, or no header |
| 403 on DELETE | only the creating IP may delete |
| `store: migrate` at startup | leftover db from an older schema (delete the file or sidecars) |
| click counts lag after restart | queue drains on shutdown; SIGKILL skips the drain by design |
