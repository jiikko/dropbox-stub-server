package stub

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// Fault makes the next Count requests to Path fail with Status and/or wait DelayMS first.
// It lets tests exercise the app's error paths (rate limits, expired tokens, slow responses)
// without a real outage.
type Fault struct {
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Tag        string `json:"tag,omitempty"`         // error ".tag" in the body (default per status)
	Count      int    `json:"count"`                 // <= 0 means 1
	DelayMS    int    `json:"delay_ms,omitempty"`    // wait before responding (or before the real handler when Status is 0)
	RetryAfter int    `json:"retry_after,omitempty"` // seconds, sent with 429
}

type faultStore struct {
	mu     sync.Mutex
	faults []*Fault
}

func newFaultStore() *faultStore { return &faultStore{} }

func (f *faultStore) add(x Fault) {
	if x.Count <= 0 {
		x.Count = 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &x)
}

// take consumes one use of the first fault matching path.
func (f *faultStore) take(path string) (Fault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.faults {
		if x.Path != path {
			continue
		}
		x.Count--
		got := *x
		if x.Count <= 0 {
			f.faults = append(f.faults[:i], f.faults[i+1:]...)
		}
		return got, true
	}
	return Fault{}, false
}

func (f *faultStore) list() []Fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Fault, 0, len(f.faults))
	for _, x := range f.faults {
		out = append(out, *x)
	}
	return out
}

func (f *faultStore) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = nil
}

func writeInjected(w http.ResponseWriter, f Fault) {
	tag := f.Tag
	if tag == "" {
		switch f.Status {
		case http.StatusUnauthorized:
			tag = "expired_access_token"
		case http.StatusTooManyRequests:
			tag = "too_many_requests"
		default:
			tag = "internal_error"
		}
	}
	if f.Status == http.StatusTooManyRequests {
		ra := f.RetryAfter
		if ra <= 0 {
			ra = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(ra))
	}
	writeJSON(w, f.Status, map[string]any{"error_summary": tag + "/..", "error": map[string]any{".tag": tag}})
}

func (s *Server) handleFaultsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"faults": s.faults.list()})
}

func (s *Server) handleFaultsAdd(w http.ResponseWriter, r *http.Request) {
	var f Fault
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err == nil {
		err = json.Unmarshal(body, &f)
	}
	if err != nil || f.Path == "" || (f.Status == 0 && f.DelayMS == 0) {
		badRequest(w, "need {\"path\": ..., \"status\": ... and/or \"delay_ms\": ...}")
		return
	}
	s.faults.add(f)
	writeJSON(w, http.StatusOK, map[string]any{"faults": s.faults.list()})
}

func (s *Server) handleFaultsClear(w http.ResponseWriter, _ *http.Request) {
	s.faults.clear()
	writeJSON(w, http.StatusOK, map[string]any{"faults": []Fault{}})
}
