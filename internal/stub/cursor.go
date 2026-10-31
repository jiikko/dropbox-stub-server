package stub

import (
	"fmt"
	"sync"
)

// maxCursors bounds the cursors kept in memory. Every list_folder / continue / search stores a
// full snapshot, so without a bound a client looping on them grows the stub without limit.
// The oldest cursors are dropped first; a dropped list cursor gets the same "reset" error Dropbox
// returns, which makes the app list the folder again.
const maxCursors = 1000

// cursorState is what a list_folder cursor points at: the listing it was taken from (to page
// through, and to diff against on /continue once the pages are exhausted).
type cursorState struct {
	display   string
	recursive bool
	limit     int
	snapshot  []Metadata // full listing at the time the cursor chain started or last caught up
	offset    int        // next index into snapshot to return
}

// searchState is what a search cursor points at.
type searchState struct {
	matches  []map[string]any
	pageSize int
	offset   int
}

// store keeps opaque cursors in memory, at most maxCursors of them (oldest dropped first).
type store[T any] struct {
	prefix string
	mu     sync.Mutex
	next   int
	m      map[string]*T
	order  []string
}

func newStore[T any](prefix string) *store[T] {
	return &store[T]{prefix: prefix, m: map[string]*T{}}
}

func (c *store[T]) put(st *T) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("%s-%d", c.prefix, c.next)
	c.m[id] = st
	c.order = append(c.order, id)
	for len(c.order) > maxCursors {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	return id
}

func (c *store[T]) get(id string) (*T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.m[id]
	return st, ok
}

func (c *store[T]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// diff returns what changed from old to cur as Dropbox list_folder entries: added or modified
// entries as-is, removed ones as ".tag": "deleted".
func diff(old, cur []Metadata) []Metadata {
	oldBy := make(map[string]Metadata, len(old))
	for _, m := range old {
		oldBy[m.PathLower] = m
	}
	curBy := make(map[string]bool, len(cur))
	var out []Metadata
	for _, m := range cur {
		curBy[m.PathLower] = true
		o, ok := oldBy[m.PathLower]
		if !ok || o.Tag != m.Tag || o.Rev != m.Rev || o.PathDisplay != m.PathDisplay {
			out = append(out, m)
		}
	}
	for _, m := range old {
		if !curBy[m.PathLower] {
			out = append(out, Metadata{Tag: "deleted", Name: m.Name, PathLower: m.PathLower, PathDisplay: m.PathDisplay})
		}
	}
	return out
}

// fingerprint summarizes a listing so longpoll can tell whether anything changed.
func fingerprint(entries []Metadata) string {
	h := make([]byte, 0, len(entries)*32)
	for _, m := range entries {
		h = append(h, m.Tag...)
		h = append(h, 0)
		h = append(h, m.PathDisplay...)
		h = append(h, 0)
		h = append(h, m.Rev...)
		h = append(h, '\n')
	}
	return string(h)
}
