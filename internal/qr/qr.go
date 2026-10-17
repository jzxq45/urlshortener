// Package qr renders PNG QR codes for short links with a small in-memory LRU.
package qr

import (
	"fmt"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

// CacheCap is the max QR PNGs held in memory.
const CacheCap = 1000

// entry keeps the PNG bytes; expiry lets hourly URL/base changes re-render.
type entry struct {
	png       []byte
	expiresAt time.Time
}

// cache is a simple LRU keyed by code.
type cache struct {
	mu   sync.Mutex
	m    map[string]*listN
	head *listN // most recent
	tail *listN // least recent
}

type listN struct {
	key  string
	prev *listN
	next *listN
	e    *entry
}

// Renderer builds QR PNGs offline (no network at render time).
type Renderer struct {
	base string
	c    cache
	ttl  time.Duration
}

// New builds a Renderer prefixing short URLs with base (no trailing slash).
func New(base string) *Renderer {
	return &Renderer{base: base, c: cache{m: make(map[string]*listN, CacheCap)}, ttl: time.Hour}
}

// PNG returns the QR PNG of base+'/r/'+code, cached by code.
func (r *Renderer) PNG(code string) ([]byte, error) {
	if c, ok := r.get(code); ok {
		return c, nil
	}
	q, err := qrcode.New(r.base+"/r/"+code, qrcode.Medium)
	if err != nil {
		// qrcode.New fails for empty/oversized content only; codes are fixed
		return nil, fmt.Errorf("qr: encode %s: %w", code, err)
	}
	png, err := q.PNG(256)
	if err != nil {
		return nil, fmt.Errorf("qr: png %s: %w", code, err)
	}
	r.put(code, png)
	return png, nil
}

func (r *Renderer) get(code string) ([]byte, bool) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	n, ok := r.c.m[code]
	if !ok {
		return nil, false
	}
	if time.Now().After(n.e.expiresAt) {
		r.remove(code, n)
		return nil, false
	}
	r.touch(n)
	return n.e.png, true
}

func (r *Renderer) put(code string, png []byte) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if n, ok := r.c.m[code]; ok {
		n.e = &entry{png: png, expiresAt: time.Now().Add(r.ttl)}
		r.touch(n)
		return
	}
	n := &listN{key: code, e: &entry{png: png, expiresAt: time.Now().Add(r.ttl)}}
	if r.c.head == nil {
		r.c.head, r.c.tail = n, n
	} else {
		n.next = r.c.head
		r.c.head.prev = n
		r.c.head = n
	}
	r.c.m[code] = n
	if len(r.c.m) > CacheCap {
		// evict LRU tail
		tail := r.c.tail
		if tail != nil {
			r.unlink(tail)
			delete(r.c.m, tail.key)
		}
	}
}

// touch moves n to head (most recently used).
func (r *Renderer) touch(n *listN) {
	if n == r.c.head {
		return
	}
	r.unlink(n)
	n.next = r.c.head
	if r.c.head != nil {
		r.c.head.prev = n
	}
	r.c.head = n
	if r.c.tail == nil {
		r.c.tail = n
	}
}

// unlink detaches n from the list without touching the map.
func (r *Renderer) unlink(n *listN) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		r.c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		r.c.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

// remove unlinks+deletes a node already holding the map key.
func (r *Renderer) remove(code string, n *listN) {
	r.unlink(n)
	delete(r.c.m, code)
}
