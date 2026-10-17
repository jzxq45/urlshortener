-- Schema v1. All statements are idempotent so re-running migration is safe.

CREATE TABLE IF NOT EXISTS links (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT NOT NULL UNIQUE, -- UNIQUE already provides the code index
    url TEXT NOT NULL,
    created_at INTEGER NOT NULL, -- unix seconds UTC
    expires_at INTEGER, -- unix seconds UTC, NULL = never expires
    clicks INTEGER NOT NULL DEFAULT 0,
    creator_ip TEXT NOT NULL,
    custom_code INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_links_created_at ON links(created_at);

CREATE TABLE IF NOT EXISTS clicks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    link_id INTEGER NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    ts INTEGER NOT NULL, -- unix seconds UTC
    ip TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    referer TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_clicks_link_id_ts ON clicks(link_id, ts);
CREATE INDEX IF NOT EXISTS idx_clicks_ts ON clicks(ts);
