# HTTP API

Base URL in examples: `http://localhost:8080`. All `application/json`
endpoints; errors are `{"error": "<message>"}`. Cross-origin requests with a
mismatched `Origin` header are rejected with 403.

## Service

### GET /healthz
`200 {"status":"ok"}` — for load balancers.

### GET /metrics
Prometheus text format (counters `http_requests_total`, `shorten_total`,
`redirect_total`, `clicks_total`; gauge `active_links`; histogram
`http_request_duration_seconds`).

## Short links

### POST /api/shorten → 201

```json
{
  "url": "https://example.com/very/long/page",
  "custom_code": "my-link",     // optional, [A-Za-z0-9_-]{3,32}
  "ttl_seconds": 2592000        // optional, expires_at = now + ttl
}
```

Tail: `GET /api/shorten` over-limit → `429` with `Retry-After: 60`.
`400` invalid body/URL/code/TTL; `409` custom code already in use.

Response `201`:

```json
{
  "code": "mWP16US",
  "url": "https://example.com/very/long/page",
  "short_url": "http://localhost:8080/r/mWP16US",
  "created_at": "2026-10-17T02:09:42Z",
  "expires_at": null,
  "expired": false,
  "clicks": 0,
  "creator_ip": "192.0.2.1",
  "custom_code": false
}
```

htmx form requests (`HX-Request: true`) receive `200` with an HTML table-row
fragment instead of JSON.

### GET /api/links?limit=&offset= → 200

Paged list (newest first). `limit` clamped to 1..200 (default 50).

```json
{"items": [ /* link objects */ ], "total": 42, "limit": 50, "offset": 0}
```

### GET /api/links/{code} → 200 | 404
Single link object.

### DELETE /api/links/{code} → 204 | 403 | 404
Only the client that created the link (its IP seen by the server) may delete.
Click records cascade.

### GET /r/{code} → 302 | 404 | 410
Redirect with `Location` and `Cache-Control: no-store`. Interactive version:
`GET /{code}` renders the link detail page with charts.

## Analytics

### GET /api/links/{code}/stats → 200 | 404

```json
{
  "code": "mWP16US",
  "total_clicks": 123,
  "unique_ips": 45,
  "windows": {"24h": 7, "7d": 30, "30d": 98},
  "top_referers": [{"value": "https://news.ycombinator.com", "count": 10}],
  "recent": [{"ts": "2026-10-17T02:00:00Z", "ip": "…", "user_agent": "…", "referer": "…"}],
  "daily_30d": [{"day": "2026-10-17T00:00:00Z", "count": 3}],
  "hourly_24h": [{"hour": "2026-10-17T01:00:00Z", "count": 1}]
}
```

### GET /api/links/{code}/export.csv → 200 | 404
Full click stream, `attachment; filename="<code>.csv"`,
`ts,ip,user_agent,referer` rows ordered oldest first.

## QR

### GET /qr/{code}.png → 200 | 404 | 410
PNG (256px, error level M) of the short URL. `Cache-Control: public, max-age=3600`.
The bare `/qr/{code}` also works.

## Pages

`GET /` form + newest 20 · `GET /links` all links, paged · `GET /{code}`
link details with Chart.js charts · `GET /stats` global analytics ·
`GET /static/*` assets (all embedded in the binary).

## Reserved codes

`api admin health static favicon r links stats qr metrics login signup docs`
cannot be used as custom codes.
