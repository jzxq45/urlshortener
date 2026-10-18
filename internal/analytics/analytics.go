// Package analytics provides click aggregation, CSV export and the click
// retention sweep.
package analytics

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"time"

	"urlshortener/internal/store"
)

// Service owns click ingestion, CSV export and the retention sweep.
type Service struct {
	st        *store.Store
	retention time.Duration
	log       *slog.Logger
	// in is the ingest buffer; a saturated queue means the caller drops
	// events (and counts them) — redirects must never wait on analytics
	in chan store.Click
}

// IngestCap is the number of clicks buffered before drops start.
const IngestCap = 4096

const (
	ingestMaxBatch  = 512
	ingestFlushTick = 200 * time.Millisecond
)

// New builds the analytics service; retention must be positive.
func New(st *store.Store, retention time.Duration, log *slog.Logger) *Service {
	return &Service{
		st:        st,
		retention: retention,
		log:       log,
		in:        make(chan store.Click, IngestCap),
	}
}

// Ingest queues one click for asynchronous persistence; false means the
// buffer is full and the caller must account for the drop.
func (s *Service) Ingest(c store.Click) bool {
	select {
	case s.in <- c:
		return true
	default:
		return false
	}
}

// Run batches queued clicks into the store: one commit covers ingestMaxBatch
// rows instead of one commit per click. It runs until ctx is done; on cancel
// (which the server schedules after the HTTP drain) every already-accepted
// event is flushed with a detached context before returning.
func (s *Service) Run(ctx context.Context) error {
	// accepted clicks must reach the database even on shutdown: db close
	// happens later (main's defer runs after errgroup.Wait)
	writeCtx := context.WithoutCancel(ctx)
	batch := make([]store.Click, 0, ingestMaxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.st.InsertClicksBatch(writeCtx, batch); err != nil {
			s.log.Error("click batch insert failed", "n", len(batch), "err", err)
		} else {
			s.log.Debug("click batch saved", "n", len(batch))
		}
		batch = batch[:0]
	}
	t := time.NewTicker(ingestFlushTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// drain: take everything currently buffered, then commit the tail
			for {
				select {
				case c := <-s.in:
					batch = append(batch, c)
					if len(batch) == ingestMaxBatch {
						flush()
					}
				default:
					flush()
					return nil
				}
			}
		case <-t.C:
			flush()
		case c := <-s.in:
			batch = append(batch, c)
			if len(batch) >= ingestMaxBatch {
				flush()
			}
		}
	}
}

// Start runs the retention sweep immediately and then once a day until ctx
// is done; it returns after ctx cancellation (usable in an errgroup).
// Sweep errors are logged and retried: a transient SQLITE_BUSY or a locked
// database must not take down the whole server (it only runs on the bg path).
func (s *Service) Start(ctx context.Context) error {
	if err := s.sweep(ctx); err != nil {
		// only ctx cancellation is "normal" here
		s.log.Error("retention sweep failed", "err", err)
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.sweep(ctx); err != nil {
				s.log.Error("retention sweep failed", "err", err)
			}
		}
	}
}

// sweep removes clicks past the retention horizon.
func (s *Service) sweep(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-s.retention)
	n, err := s.st.RetentionDeleteClicks(ctx, cutoff)
	if err != nil {
		return err
	}
	s.log.Info("retention sweep", "deleted_clicks", n, "cutoff", cutoff.Format(time.RFC3339))
	return nil
}

// ExportCSV streams all clicks of one link as CSV rows; memory stays O(1)
// because the store hands rows over one at a time. The caller resolves
// the link so export and auth share the same not-found semantics.
func (s *Service) ExportCSV(ctx context.Context, code string, w io.Writer) (int, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"ts", "ip", "user_agent", "referer"}); err != nil {
		cw.Flush()
		return 0, fmt.Errorf("write csv header: %w", err)
	}
	written := 0
	err := s.st.ClicksForExport(ctx, code, func(c store.ClickRow) error {
		// encoding/csv quotes special chars itself, including newlines in UA
		if wErr := cw.Write([]string{c.Ts.Format(time.RFC3339), c.IP, c.UserAgent, c.Referer}); wErr != nil {
			return wErr
		}
		written++
		return nil
	})
	// flush unconditionally so partial data reaches the wire even on error
	cw.Flush()
	if err != nil {
		return written, err
	}
	return written, cw.Error()
}
