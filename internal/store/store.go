// Package store persists links and clicks in SQLite (via the pure-Go
// modernc.org/sqlite driver, CGO-free). One write connection keeps SQLite
// writers serialized; a separate read pool exploits WAL concurrency.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed schema.sql
var schemaFS embed.FS

// Store wraps the two connection pools.
type Store struct {
	rw *sql.DB // single writer
	ro *sql.DB // read-only pool
}

// Link is a short link row.
type Link struct {
	ID         int64
	Code       string
	URL        string
	CreatedAt  time.Time
	ExpiresAt  *time.Time // nil = never expires
	Clicks     int64
	CreatorIP  string
	CustomCode bool
}

// Click is one recorded redirect event.
type Click struct {
	LinkID    int64
	Ts        time.Time
	IP        string
	UserAgent string
	Referer   string
}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string) (*Store, error) {
	// Per-connection pragmas via DSN: journal mode is persistent per file, but
	// busy_timeout and foreign_keys are per-connection, so they must ride the
	// DSN to apply to every pooled connection.
	prag := "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	rw, err := sql.Open("sqlite", fileDSN(path, prag))
	if err != nil {
		return nil, fmt.Errorf("store: open rw: %w", err)
	}
	// SQLite builds are fastest when writers are strictly serialized; 1 conn
	// turns SQLITE_BUSY races into natural queueing.
	rw.SetMaxOpenConns(1)

	ro, err := sql.Open("sqlite", fileDSN(path, prag+"&_pragma=query_only(1)"))
	if err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("store: open ro: %w", err)
	}
	ro.SetMaxOpenConns(20)
	// Go defaults MaxIdleConns to 2 — with an idle cap far below the open cap
	// every traffic burst would churn open/close syscall+pragma setup. Hold
	// the pool warm and recycle connections before driver/file drift.
	ro.SetMaxIdleConns(20)
	ro.SetConnMaxIdleTime(5 * time.Minute)
	ro.SetConnMaxLifetime(time.Hour)

	s := &Store{rw: rw, ro: ro}
	if err := s.migrate(rw); err != nil {
		_ = rw.Close()
		_ = ro.Close()
		return nil, err
	}
	return s, nil
}

// fileDSN builds a file:/// URI (three slashes — sqlite treats "file:C:/..."
// as authority "C") with the given pragma query. Forward slashes so the URI
// parser is happy on Windows too.
func fileDSN(path, query string) string {
	u := url.URL{
		Scheme:   "file",
		Path:     "/" + strings.ReplaceAll(path, "\\", "/"),
		RawQuery: query,
	}
	return u.String()
}

// migrate applies schema.sql; every statement is idempotent.
func (s *Store) migrate(db *sql.DB) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("store: read schema: %w", err)
	}
	for _, stmt := range strings.Split(string(raw), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || commentOnly(stmt) {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: migrate: %w: %s", err, firstLine(stmt))
		}
	}
	return nil
}

// commentOnly reports whether the statement has no executable content.
func commentOnly(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

// firstLine gives the statement for error context.
func firstLine(stmt string) string {
	for _, line := range strings.Split(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return line
		}
	}
	return ""
}

// Close closes both pools.
func (s *Store) Close() error {
	return firstErr(s.ro.Close(), s.rw.Close())
}

// Ping verifies both pools still reach the database inside their deadlines.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.rw.PingContext(ctx); err != nil {
		return fmt.Errorf("store: rw ping: %w", err)
	}
	if err := s.ro.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ro ping: %w", err)
	}
	return nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// Expired reports whether the link expiration has passed (nil = never).
func (l Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && now.After(*l.ExpiresAt)
}
