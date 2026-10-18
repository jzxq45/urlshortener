package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound (wrapped) is returned by lookups that match nothing.
var ErrNotFound = errors.New("store: not found")

// ErrCodeTaken (wrapped) is returned when a UNIQUE constraint on code fires.
var ErrCodeTaken = errors.New("store: code taken")

// RefCount is a value-count pair for referer aggregation.
type RefCount struct {
	Value string
	Count int64
}

// DayCount is a UTC calendar-day bucket.
type DayCount struct {
	Day   time.Time
	Count int64
}

// HourCount is a UTC hour bucket.
type HourCount struct {
	Hour  time.Time
	Count int64
}

// ClickRow is the subset of ClickClick rows exposed by stats/export.
type ClickRow struct {
	Ts        time.Time
	IP        string
	UserAgent string
	Referer   string
}

// CreateLink inserts l and fills l.ID with the new row id.
func (s *Store) CreateLink(ctx context.Context, l *Link) error {
	res, err := s.rw.ExecContext(ctx,
		`INSERT INTO links (code, url, created_at, expires_at, clicks, creator_ip, custom_code)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		l.Code, l.URL, unix(l.CreatedAt), unixPtr(l.ExpiresAt), l.CreatorIP, b2i(l.CustomCode))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: insert link code=%s: %w", l.Code, ErrCodeTaken)
		}
		return fmt.Errorf("store: insert link: %w", err)
	}
	l.ID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: insert link lastid: %w", err)
	}
	return nil
}

// GetLink fetches a link by its short code.
func (s *Store) GetLink(ctx context.Context, code string) (Link, error) {
	return scanLink(s.ro.QueryRowContext(ctx, `SELECT `+linkCols+` FROM links WHERE code = ?`, code))
}

// ListLinks returns up to limit links ordered newest first.
func (s *Store) ListLinks(ctx context.Context, limit, offset int) ([]Link, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+linkCols+` FROM links ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list links: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CountLinks counts all links.
func (s *Store) CountLinks(ctx context.Context) (int64, error) {
	return count(ctx, s.ro, `SELECT count(*) FROM links`)
}

// CountActiveLinks counts non-expired links.
func (s *Store) CountActiveLinks(ctx context.Context, now time.Time) (int64, error) {
	return count(ctx, s.ro, `SELECT count(*) FROM links WHERE expires_at IS NULL OR expires_at > ?`, unix(now))
}

const (
	sqlInsertClick = `INSERT INTO clicks (link_id, ts, ip, user_agent, referer) VALUES (?, ?, ?, ?, ?)`
	sqlBumpClicks  = `UPDATE links SET clicks = clicks + 1 WHERE id = ?`
	sqlBumpClicksN = `UPDATE links SET clicks = clicks + ? WHERE id = ?`
)

// IncrementClicks bumps the cached counters on the link row.
func (s *Store) IncrementClicks(ctx context.Context, linkID int64) error {
	if _, err := s.rw.ExecContext(ctx, sqlBumpClicks, linkID); err != nil {
		return fmt.Errorf("store: increment clicks: %w", err)
	}
	return nil
}

// InsertClick appends one redirect event.
func (s *Store) InsertClick(ctx context.Context, c Click) error {
	if _, err := s.rw.ExecContext(ctx,
		sqlInsertClick, c.LinkID, unix(c.Ts), c.IP, c.UserAgent, c.Referer); err != nil {
		return fmt.Errorf("store: insert click: %w", err)
	}
	return nil
}

// RecordClick writes the click row and bumps the link counter as one
// transaction: two independent writes can split the difference on a transient
// error (SQLITE_BUSY) and leave the counter drifting from the real history.
// Production writes go through InsertClicksBatch; this remains the prim path.
func (s *Store) RecordClick(ctx context.Context, c Click) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record click begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if _, err := tx.ExecContext(ctx,
		sqlInsertClick, c.LinkID, unix(c.Ts), c.IP, c.UserAgent, c.Referer); err != nil {
		return fmt.Errorf("store: record click insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, sqlBumpClicks, c.LinkID); err != nil {
		return fmt.Errorf("store: record click counter: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: record click commit: %w", err)
	}
	return nil
}

// InsertClicksBatch writes a batch of click rows and the matching counter
// bumps in one transaction: a single commit amortizes over the whole batch,
// which is the difference between hundreds and ten-thousands of clicks/sec.
func (s *Store) InsertClicksBatch(ctx context.Context, clicks []Click) error {
	if len(clicks) == 0 {
		return nil
	}
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: batch click begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	var (
		qb   strings.Builder
		args = make([]any, 0, len(clicks)*5)
	)
	qb.WriteString(`INSERT INTO clicks (link_id, ts, ip, user_agent, referer) VALUES `)
	for i, c := range clicks {
		if i > 0 {
			qb.WriteString(",")
		}
		qb.WriteString("(?,?,?,?,?)")
		args = append(args, c.LinkID, unix(c.Ts), c.IP, c.UserAgent, c.Referer)
	}
	if _, err := tx.ExecContext(ctx, qb.String(), args...); err != nil {
		return fmt.Errorf("store: batch click insert: %w", err)
	}
	// one UPDATE per affected link, not per row
	bumps := make(map[int64]int64, 1)
	for _, c := range clicks {
		bumps[c.LinkID]++
	}
	for id, n := range bumps {
		if _, err := tx.ExecContext(ctx, sqlBumpClicksN, n, id); err != nil {
			return fmt.Errorf("store: batch click counter: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: batch click commit: %w", err)
	}
	return nil
}

// DeleteLink removes a link (clicks cascade) and reports whether it existed.
func (s *Store) DeleteLink(ctx context.Context, code string) (bool, error) {
	res, err := s.rw.ExecContext(ctx, `DELETE FROM links WHERE code = ?`, code)
	if err != nil {
		return false, fmt.Errorf("store: delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete link affected: %w", err)
	}
	return n > 0, nil
}

// ClicksInWindow counts clicks for a link since a timestamp.
func (s *Store) ClicksInWindow(ctx context.Context, linkID int64, since time.Time) (int64, error) {
	return count(ctx, s.ro, `SELECT count(*) FROM clicks WHERE link_id = ? AND ts >= ?`, linkID, unix(since))
}

// UniqueIPs counts distinct client IPs for a link since a timestamp.
func (s *Store) UniqueIPs(ctx context.Context, linkID int64, since time.Time) (int64, error) {
	return count(ctx, s.ro,
		`SELECT count(DISTINCT ip) FROM clicks WHERE link_id = ? AND ts >= ?`, linkID, unix(since))
}

// TotalClicksForLink sums clicks across all time for one link.
func (s *Store) TotalClicksForLink(ctx context.Context, linkID int64) (int64, error) {
	return count(ctx, s.ro, `SELECT count(*) FROM clicks WHERE link_id = ?`, linkID)
}

// TopReferers returns the limit most frequent referers for a link.
func (s *Store) TopReferers(ctx context.Context, linkID int64, since time.Time, limit int) ([]RefCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT referer, count(*) AS c FROM clicks
		 WHERE link_id = ? AND ts >= ? GROUP BY referer ORDER BY c DESC, referer LIMIT ?`,
		linkID, unix(since), limit)
	if err != nil {
		return nil, fmt.Errorf("store: top referers: %w", err)
	}
	return collectRefCounts(ctx, rows, err)
}

// TopReferersGlobal aggregates referers across every link.
func (s *Store) TopReferersGlobal(ctx context.Context, since time.Time, limit int) ([]RefCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT referer, count(*) AS c FROM clicks
		 WHERE ts >= ? GROUP BY referer ORDER BY c DESC, referer LIMIT ?`, unix(since), limit)
	if err != nil {
		return nil, fmt.Errorf("store: top referers global: %w", err)
	}
	return collectRefCounts(ctx, rows, err)
}

// ClicksByDay groups link clicks into UTC days between since and until.
func (s *Store) ClicksByDay(ctx context.Context, linkID int64, since, until time.Time) ([]DayCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT (ts / 86400) * 86400 AS day, count(*) FROM clicks
		 WHERE link_id = ? AND ts >= ? AND ts < ?
		 GROUP BY day ORDER BY day`, linkID, unix(since), unix(until))
	if err != nil {
		return nil, fmt.Errorf("store: clicks by day: %w", err)
	}
	return collectDayCounts(ctx, rows, err)
}

// ClicksByDayGlobal is ClicksByDay across all links.
func (s *Store) ClicksByDayGlobal(ctx context.Context, since, until time.Time) ([]DayCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT (ts / 86400) * 86400 AS day, count(*) FROM clicks
		 WHERE ts >= ? AND ts < ? GROUP BY day ORDER BY day`, unix(since), unix(until))
	if err != nil {
		return nil, fmt.Errorf("store: clicks by day global: %w", err)
	}
	return collectDayCounts(ctx, rows, err)
}

// ClicksByHour groups link clicks into UTC hours since a timestamp.
func (s *Store) ClicksByHour(ctx context.Context, linkID int64, since time.Time) ([]HourCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT (ts / 3600) * 3600 AS hour, count(*) FROM clicks
		 WHERE link_id = ? AND ts >= ? GROUP BY hour ORDER BY hour`, linkID, unix(since))
	if err != nil {
		return nil, fmt.Errorf("store: clicks by hour: %w", err)
	}
	return collectHourCounts(ctx, rows, err)
}

// ClicksByHourGlobal is ClicksByHour across all links.
func (s *Store) ClicksByHourGlobal(ctx context.Context, since time.Time) ([]HourCount, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT (ts / 3600) * 3600 AS hour, count(*) FROM clicks
		 WHERE ts >= ? GROUP BY hour ORDER BY hour`, unix(since))
	if err != nil {
		return nil, fmt.Errorf("store: clicks by hour global: %w", err)
	}
	return collectHourCounts(ctx, rows, err)
}

// RecentClicks returns the newest limit clicks of a link.
func (s *Store) RecentClicks(ctx context.Context, linkID int64, limit int) ([]ClickRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT ts, ip, user_agent, referer FROM clicks
		 WHERE link_id = ? ORDER BY ts DESC, id DESC LIMIT ?`, linkID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent clicks: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	var out []ClickRow
	for rows.Next() {
		var cr ClickRow
		var ts int64
		if err := rows.Scan(&ts, &cr.IP, &cr.UserAgent, &cr.Referer); err != nil {
			return nil, fmt.Errorf("store: scan click: %w", err)
		}
		cr.Ts = fromUnix(ts)
		out = append(out, cr)
	}
	return out, rows.Err()
}

// ClicksForExport streams all clicks of a link (ordered oldest first) into
// fn one row at a time — no materialized slice, so a huge click history
// cannot exhaust memory (O(1) RSS regardless of history size).
func (s *Store) ClicksForExport(ctx context.Context, code string, fn func(ClickRow) error) error {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT c.ts, c.ip, c.user_agent, c.referer
		 FROM clicks c JOIN links l ON l.id = c.link_id WHERE l.code = ? ORDER BY c.ts ASC, c.id ASC`, code)
	if err != nil {
		return fmt.Errorf("store: export clicks: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	for rows.Next() {
		var cr ClickRow
		var ts int64
		if err := rows.Scan(&ts, &cr.IP, &cr.UserAgent, &cr.Referer); err != nil {
			return fmt.Errorf("store: scan export click: %w", err)
		}
		cr.Ts = fromUnix(ts)
		if err := fn(cr); err != nil {
			return fmt.Errorf("store: export click callback: %w", err)
		}
	}
	return rows.Err()
}

// ClicksCountGlobal counts clicks across all links since a timestamp.
func (s *Store) ClicksCountGlobal(ctx context.Context, since time.Time) (int64, error) {
	return count(ctx, s.ro, `SELECT count(*) FROM clicks WHERE ts >= ?`, unix(since))
}

// RetentionDeleteClicks removes clicks older than the cutoff and returns how
// many rows were removed. The delete runs in bounded chunks: a single
// unbounded DELETE on the sole write connection would stall every new click
// for the whole sweep and balloon the WAL.
func (s *Store) RetentionDeleteClicks(ctx context.Context, cutoff time.Time) (int64, error) {
	const chunk = 10_000
	var total int64
	for {
		res, err := s.rw.ExecContext(ctx,
			`DELETE FROM clicks WHERE rowid IN (SELECT rowid FROM clicks WHERE ts < ? LIMIT ?)`,
			unix(cutoff), chunk)
		if err != nil {
			return total, fmt.Errorf("store: retention delete: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: retention delete affected: %w", err)
		}
		total += n
		if n < chunk {
			return total, nil
		}
	}
}

// --- helpers -------------------------------------------------------------

const linkCols = `id, code, url, created_at, expires_at, clicks, creator_ip, custom_code`

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	var created, expires sql.NullInt64
	var custom int64
	if err := row.Scan(&l.ID, &l.Code, &l.URL, &created, &expires, &l.Clicks, &l.CreatorIP, &custom); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return l, fmt.Errorf("store: link by code: %w", ErrNotFound)
		}
		return l, fmt.Errorf("store: scan link: %w", err)
	}
	l.CreatedAt = fromUnix(created.Int64)
	if expires.Valid {
		t := fromUnix(expires.Int64)
		l.ExpiresAt = &t
	}
	l.CustomCode = custom != 0
	return l, nil
}

func collectRefCounts(ctx context.Context, rows *sql.Rows, err error) ([]RefCount, error) {
	if err != nil {
		return nil, fmt.Errorf("store: top referers: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	var out []RefCount
	for rows.Next() {
		var rc RefCount
		if err := rows.Scan(&rc.Value, &rc.Count); err != nil {
			return nil, fmt.Errorf("store: scan referer: %w", err)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

func collectDayCounts(ctx context.Context, rows *sql.Rows, err error) ([]DayCount, error) {
	if err != nil {
		return nil, fmt.Errorf("store: clicks by day: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	var out []DayCount
	for rows.Next() {
		var day, c int64
		if err := rows.Scan(&day, &c); err != nil {
			return nil, fmt.Errorf("store: scan day: %w", err)
		}
		out = append(out, DayCount{Day: fromUnix(day), Count: c})
	}
	return out, rows.Err()
}

func collectHourCounts(ctx context.Context, rows *sql.Rows, err error) ([]HourCount, error) {
	if err != nil {
		return nil, fmt.Errorf("store: clicks by hour: %w", err)
	}
	defer func() { _ = rows.Close() }() // iterative errors surface via rows.Err/Scan; Close itself is release-only
	var out []HourCount
	for rows.Next() {
		var hour, c int64
		if err := rows.Scan(&hour, &c); err != nil {
			return nil, fmt.Errorf("store: scan hour: %w", err)
		}
		out = append(out, HourCount{Hour: fromUnix(hour), Count: c})
	}
	return out, rows.Err()
}

func count(ctx context.Context, db *sql.DB, query string, args ...any) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

func unix(t time.Time) int64     { return t.UTC().Unix() }
func fromUnix(u int64) time.Time { return time.Unix(u, 0).UTC() }

func unixPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return unix(*t)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation matches the modernc driver's error text for
// SQLITE_CONSTRAINT_UNIQUE. Matching the stable sqlite error code text is
// simpler than depending on driver internals that change across versions.
func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed: UNIQUE"))
}
