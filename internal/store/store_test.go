package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a throwaway db in t.TempDir().
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLinkCRUD(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	l := Link{Code: "abc123", URL: "https://example.com", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.CreateLink(ctx, &l); err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.ID == 0 {
		t.Fatal("id not filled")
	}

	got, err := s.GetLink(ctx, "abc123")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != l.URL || got.ExpiresAt != nil {
		t.Errorf("mismatch: %+v", got)
	}

	_, err = s.GetLink(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}

	// unique constraint fires ErrCodeTaken
	dup := Link{Code: "abc123", URL: "https://other", CreatedAt: time.Now().UTC()}
	if err := s.CreateLink(ctx, &dup); !errors.Is(err, ErrCodeTaken) {
		t.Errorf("want ErrCodeTaken, got %v", err)
	}

	if n, err := s.CountLinks(ctx); err != nil || n != 1 {
		t.Errorf("count links: n=%d err=%v", n, err)
	}

	ok, err := s.DeleteLink(ctx, "abc123")
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DeleteLink(ctx, "abc123"); ok {
		t.Error("second delete reported success")
	}
}

func TestClickAggregates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	l := Link{Code: "agg", URL: "https://example.com", CreatedAt: time.Now().UTC()}
	if err := s.CreateLink(ctx, &l); err != nil {
		t.Fatalf("create: %v", err)
	}
	// a fixed UTC anchor keeps day/hour bucketing deterministic no matter
	// when the test runs or where a midnight boundary falls
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	clicks := []Click{
		{LinkID: l.ID, Ts: base.Add(-2 * time.Hour), IP: "10.0.0.1", UserAgent: "ua1", Referer: "https://ref1"},
		{LinkID: l.ID, Ts: base.Add(-1 * time.Hour), IP: "10.0.0.1", UserAgent: "ua1", Referer: "https://ref1"},
		{LinkID: l.ID, Ts: base, IP: "10.0.0.2", UserAgent: "ua2", Referer: "https://ref2"},
	}
	for _, c := range clicks {
		if err := s.InsertClick(ctx, c); err != nil {
			t.Fatalf("insert click: %v", err)
		}
		if err := s.IncrementClicks(ctx, l.ID); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}

	if n, _ := s.TotalClicksForLink(ctx, l.ID); n != 3 {
		t.Errorf("total clicks = %d, want 3", n)
	}
	if n, _ := s.ClicksInWindow(ctx, l.ID, base.Add(-30*time.Minute)); n != 1 {
		t.Errorf("30min window = %d, want 1", n)
	}
	if n, _ := s.ClicksInWindow(ctx, l.ID, base.Add(-90*time.Minute)); n != 2 {
		t.Errorf("90min window = %d, want 2", n)
	}
	if n, _ := s.UniqueIPs(ctx, l.ID, time.Time{}); n != 2 {
		t.Errorf("unique ips = %d, want 2", n)
	}
	if updated, _ := s.GetLink(ctx, l.Code); updated.Clicks != 3 {
		t.Errorf("cached counter = %d, want 3", updated.Clicks)
	}

	refs, err := s.TopReferers(ctx, l.ID, time.Time{}, 10)
	if err != nil || len(refs) != 2 || refs[0].Value != "https://ref1" || refs[0].Count != 2 {
		t.Errorf("top referers: %+v err=%v", refs, err)
	}

	daily, err := s.ClicksByDay(ctx, l.ID, base.Add(-3*time.Hour), base.Add(time.Hour))
	if err != nil || len(daily) != 1 || daily[0].Count != 3 {
		t.Errorf("daily: %+v err=%v", daily, err)
	}
	hourly, err := s.ClicksByHour(ctx, l.ID, base.Add(-2*time.Hour-time.Second))
	if err != nil || len(hourly) != 3 {
		t.Errorf("hourly: %+v err=%v", hourly, err)
	}

	recent, err := s.RecentClicks(ctx, l.ID, 2)
	if err != nil || len(recent) != 2 || recent[0].IP != "10.0.0.2" {
		t.Errorf("recent: %+v err=%v", recent, err)
	}
}

func TestRetentionDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	l := Link{Code: "ret", URL: "https://example.com", CreatedAt: time.Now().UTC()}
	if err := s.CreateLink(ctx, &l); err != nil {
		t.Fatalf("create: %v", err)
	}
	old := time.Now().UTC().Add(-91 * 24 * time.Hour)
	if err := s.InsertClick(ctx, Click{LinkID: l.ID, Ts: old, IP: "1.1.1.1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertClick(ctx, Click{LinkID: l.ID, Ts: time.Now().UTC(), IP: "2.2.2.2"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := s.RetentionDeleteClicks(ctx, time.Now().UTC().Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("retention deleted=%d err=%v", n, err)
	}
	if left, _ := s.TotalClicksForLink(ctx, l.ID); left != 1 {
		t.Errorf("clicks left = %d, want 1", left)
	}
}

func TestLinkExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	if (Link{}).Expired(now) {
		t.Error("nil expiry must not expire")
	}
	if !(&Link{ExpiresAt: &past}).Expired(now) {
		t.Error("past expiry must expire")
	}
	if (&Link{ExpiresAt: &future}).Expired(now) {
		t.Error("future expiry must not expire yet")
	}
}
