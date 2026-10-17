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

// Service owns retention scheduling and CSV export for handlers.
type Service struct {
	st        *store.Store
	retention time.Duration
	log       *slog.Logger
}

// New builds the analytics service; retention must be positive.
func New(st *store.Store, retention time.Duration, log *slog.Logger) *Service {
	return &Service{st: st, retention: retention, log: log}
}

// Start runs the retention sweep immediately and then once a day until ctx
// is done; it returns after ctx cancellation (usable in an errgroup).
func (s *Service) Start(ctx context.Context) error {
	if err := s.sweep(ctx); err != nil {
		return err
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.sweep(ctx); err != nil {
				return err
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

// ExportCSV writes all clicks of one link as CSV rows. The caller resolves
// the link so export and auth share the same not-found semantics.
func (s *Service) ExportCSV(ctx context.Context, code string, w io.Writer) (int, error) {
	rows, err := s.st.ClicksForExport(ctx, code)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"ts", "ip", "user_agent", "referer"}); err != nil {
		return 0, fmt.Errorf("write csv header: %w", err)
	}
	written := 0
	for _, c := range rows {
		if err := cw.Write([]string{c.Ts.Format(time.RFC3339), c.IP, c.UserAgent, c.Referer}); err != nil {
			return written, fmt.Errorf("write csv row: %w", err)
		}
		written++
	}
	cw.Flush()
	return written, cw.Error()
}
