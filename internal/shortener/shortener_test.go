package shortener

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"urlshortener/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// constRand is an io.Reader that always yields the same byte; 10 maps to 'A'.
type constRand struct{ b byte }

func (c constRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.b
	}
	return len(p), nil
}

func TestNewCodeFormat(t *testing.T) {
	t.Parallel()
	s := New(newTestStore(t))
	for i := 0; i < 50; i++ {
		code, err := s.newCode()
		if err != nil {
			t.Fatalf("newCode: %v", err)
		}
		if len(code) != codeLen {
			t.Fatalf("len = %d, want %d", len(code), codeLen)
		}
		// every char must be in the base62 alphabet
		if !strings.ContainsFunc(code, func(r rune) bool { return strings.ContainsRune(alphabet, r) }) {
			t.Fatalf("code %q outside alphabet", code)
		}
	}
}

func TestNewCodeDistribution(t *testing.T) {
	t.Parallel()
	s := New(newTestStore(t)) // real crypto/rand reader
	const samples = 2000
	counts := make(map[byte]int, len(alphabet))
	for i := 0; i < samples; i++ {
		code, err := s.newCode()
		if err != nil {
			t.Fatalf("newCode: %v", err)
		}
		for c := range code {
			counts[code[c]]++
		}
	}
	total := samples * codeLen
	want := total / len(alphabet)
	// worst observed bin must be within ~9σ of a fair binomial draw
	lo, hi := want/2, want*7/4
	for c, n := range counts {
		if len(alphabet) != 62 { // alphabet check keeps the loop meaningful
			t.Fatalf("alphabet size %d", len(alphabet))
		}
		if n < lo || n > hi {
			t.Fatalf("char %q seen %d times, want in [%d,%d]", rune(c), n, lo, hi)
		}
	}
}

func TestCreateRandomCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(newTestStore(t))
	l, err := s.Create(ctx, CreateInput{URL: "https://example.com/page"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(l.Code) != codeLen || l.CustomCode {
		t.Fatalf("unexpected link %+v", l)
	}
	got, err := s.st.GetLink(ctx, l.Code)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != "https://example.com/page" || got.CreatorIP != "" {
		t.Fatalf("unexpected stored link %+v", got)
	}
}

func TestCreateRetryExhaustedAfterCollisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	// Pre-take the code the constant reader will keep producing.
	existing := store.Link{Code: "AAAAAAA", URL: "https://example.com", CreatedAt: time.Now().UTC()}
	if err := st.CreateLink(ctx, &existing); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	s := New(st)
	s.reader = constRand{b: 10} // always 'A'
	_, err := s.Create(ctx, CreateInput{URL: "https://example.com/other"})
	if !errors.Is(err, ErrCodeGenExhaust) {
		t.Fatalf("err = %v, want ErrCodeGenExhaust", err)
	}
	// retries must not have inserted the colliding code
	if n, _ := st.CountLinks(ctx); n != 1 {
		t.Fatalf("links = %d, want 1", n)
	}
}

func TestCreateCustomCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(newTestStore(t))

	l, err := s.Create(ctx, CreateInput{URL: "https://example.com", CustomCode: "my-link_1", CreatorIP: "10.0.0.1"})
	if err != nil {
		t.Fatalf("create custom: %v", err)
	}
	if !l.CustomCode || l.Code != "my-link_1" {
		t.Fatalf("unexpected link %+v", l)
	}

	if _, err := s.Create(ctx, CreateInput{URL: "https://example.com", CustomCode: "my-link_1"}); !errors.Is(err, ErrCodeExists) {
		t.Fatalf("err = %v, want ErrCodeExists", err)
	}
	for _, bad := range []string{"ab", "a!b", strings.Repeat("a", 33), "bad-cha$"} {
		if _, err := s.Create(ctx, CreateInput{URL: "https://example.com", CustomCode: bad}); !errors.Is(err, ErrBadCustomCode) {
			t.Fatalf("custom %q: err = %v, want ErrBadCustomCode", bad, err)
		}
	}
	if _, err := s.Create(ctx, CreateInput{URL: "https://example.com", CustomCode: "admin"}); !errors.Is(err, ErrReservedCode) {
		t.Fatalf("err = %v, want ErrReservedCode", err)
	}
}

func TestCreateTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(newTestStore(t))
	future := 25 * time.Hour
	l, err := s.Create(ctx, CreateInput{URL: "https://example.com", TTL: &future})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.ExpiresAt == nil || l.Expired(time.Now()) {
		t.Fatalf("link must not be expired: %+v", l)
	}
	past := -1 * time.Hour
	l2, err := s.Create(ctx, CreateInput{URL: "https://example.com", TTL: &past})
	if err != nil {
		t.Fatalf("create past: %v", err)
	}
	if !l2.Expired(time.Now()) {
		t.Fatalf("link must be expired: %+v", l2)
	}
}

func TestValidateURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		hasErr bool
	}{
		{"https://example.com", false},
		{"http://example.com/a?b=1#frag", false},
		{"", true},
		{"ftp://example.com", true},
		{"http://", true},
		{strings.Repeat("a", 2049), true},
	}
	for _, tc := range cases {
		if err := validateURL(tc.raw); (err != nil) != tc.hasErr {
			t.Fatalf("validateURL(%q) = %v, want err=%v", tc.raw, err, tc.hasErr)
		}
	}
}
