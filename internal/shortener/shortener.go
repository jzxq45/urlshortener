// Package shortener creates and stores short links: base62 codes with
// collision-retry, validated custom codes, and a reserved-code guard.
package shortener

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"urlshortener/internal/store"
)

const codeLen = 7

// alphabet is base62 in digit-first order so codes sort numerically.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// customCodeRE allows letters, digits, dash and underscore.
var customCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// Reserved prefixes and words that must not collide with routes.
var reserved = map[string]bool{
	"api": true, "admin": true, "health": true, "static": true,
	"favicon": true, "r": true, "links": true, "stats": true,
	// route protectors beyond the spec list
	"qr": true, "metrics": true, "login": true, "signup": true, "docs": true,
}

// Validation errors surfaced to HTTP handlers (errors.Is-friendly).
var (
	ErrReservedCode   = errors.New("shortener: code is reserved")
	ErrBadCustomCode  = errors.New("shortener: custom code must match [A-Za-z0-9_-]{3,32}")
	ErrCodeExists     = errors.New("shortener: code already in use")
	ErrCodeGenExhaust = errors.New("shortener: random-code collisions exhausted")
)

// ErrValidation wraps every input-validation failure so handlers can map
// inputs to 400 with one errors.Is check.
var ErrValidation = errors.New("shortener: invalid input")

// CreateInput is the user-supplied part of a shorten request.
type CreateInput struct {
	URL        string
	CustomCode string
	TTL        *time.Duration // nil = never expires
	CreatorIP  string
}

// Shortener owns code generation and link creation.
type Shortener struct {
	st *store.Store
	// reader is injectable for collision tests; production uses crypto/rand.
	reader interface {
		Read(p []byte) (int, error)
	}
}

// New builds a Shortener over the store.
func New(st *store.Store) *Shortener {
	return &Shortener{st: st, reader: rand.Reader}
}

// Create validates input, chooses a code, retries on rare random collisions,
// and persists the link.
func (s *Shortener) Create(ctx context.Context, in CreateInput) (store.Link, error) {
	now := time.Now().UTC()
	l := store.Link{
		URL:       in.URL,
		CreatedAt: now,
		CreatorIP: in.CreatorIP,
	}

	if err := validateURL(in.URL); err != nil {
		return l, fmt.Errorf("shortener: %w: %w", ErrValidation, err)
	}
	if in.TTL != nil {
		exp := now.Add(*in.TTL)
		l.ExpiresAt = &exp
	}
	if in.CustomCode != "" {
		if !customCodeRE.MatchString(in.CustomCode) {
			return l, fmt.Errorf("shortener: %w: %w", ErrValidation, ErrBadCustomCode)
		}
		if reserved[strings.ToLower(in.CustomCode)] {
			return l, fmt.Errorf("shortener: %w: %w", ErrValidation, ErrReservedCode)
		}
		l.CustomCode = true
		l.Code = in.CustomCode
		if err := s.st.CreateLink(ctx, &l); err != nil {
			if errors.Is(err, store.ErrCodeTaken) {
				return l, fmt.Errorf("shortener: %w: %w", ErrValidation, ErrCodeExists)
			}
			return l, err
		}
		return l, nil
	}

	// 5 collisions at 62^7 ≈ 3.5e12 keys is ~7 sigma; beyond that fail loudly.
	for i := 0; i < 5; i++ {
		code, err := s.newCode()
		if err != nil {
			return l, err
		}
		l.Code = code
		err = s.st.CreateLink(ctx, &l)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, store.ErrCodeTaken) {
			return l, err
		}
	}
	return l, ErrCodeGenExhaust
}

// newCode encodes crypto/rand as 7 base62 characters.
func (s *Shortener) newCode() (string, error) {
	out := make([]byte, 0, codeLen)
	buf := make([]byte, codeLen)
	n, err := s.reader.Read(buf)
	if err != nil {
		return "", fmt.Errorf("shortener: random: %w", err)
	}
	if n < codeLen {
		return "", fmt.Errorf("shortener: random: short read: %w", errors.New("need 7 bytes"))
	}
	// Modulo on byte values: 256 % 62 = 8 extra steps for 8 symbols —
	// negligible bias (8/256), documented instead of paying 2 reads/char.
	for _, b := range buf {
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out), nil
}

// validateURL requires http(s), a host, and a sane length.
func validateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: url is empty", errors.New("url invalid"))
	}
	if len(raw) > 2048 {
		return fmt.Errorf("url is %d bytes, max 2048", len(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http/https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host is empty")
	}
	return nil
}
