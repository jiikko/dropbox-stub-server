package stub

import (
	"fmt"
	"sync"
)

// cursorState is what a list_folder cursor points at: the listing it was taken from (to page
// through, and to diff against on /continue once the pages are exhausted).
type cursorState struct {
	abs       string
	display   string
	recursive bool
	limit     int
	snapshot  []Metadata // full listing at the time the cursor chain started or last caught up
	offset    int        // next index into snapshot to return
}

// cursorStore keeps cursors in memory. Cursors are opaque strings; an unknown cursor (for
// example after a stub restart) gets the same "reset" error Dropbox returns, which makes the app
// list the folder again.
type cursorStore struct {
	mu   sync.Mutex
	next int
	m    map[string]*cursorState
}

func newCursorStore() *cursorStore { return &cursorStore{m: map[string]*cursorState{}} }

func (c *cursorStore) put(st *cursorState) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("stubcursor-%d", c.next)
	c.m[id] = st
	return id
}

func (c *cursorStore) get(id string) (*cursorState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.m[id]
	return st, ok
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
