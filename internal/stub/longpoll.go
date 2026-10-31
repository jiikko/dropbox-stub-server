package stub

import (
	"net/http"
	"time"
)

// handleLongpoll waits until the folder behind cursor changes or the timeout passes.
// Like the real notify endpoint it takes no Authorization header.
func (s *Server) handleLongpoll(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		badRequest(w, "this endpoint does not accept an Authorization header")
		return
	}
	var arg struct {
		Cursor  string `json:"cursor"`
		Timeout int    `json:"timeout"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	st, ok := s.cursors.get(arg.Cursor)
	if !ok {
		writeError(w, http.StatusConflict, "reset", nil)
		return
	}
	timeout := arg.Timeout
	if timeout == 0 {
		timeout = 30
	}
	if timeout < 30 || timeout > 480 {
		badRequest(w, "request body: timeout: %d is out of range [30, 480]", timeout)
		return
	}
	want := fingerprint(st.snapshot)
	deadline := time.Now().Add(time.Duration(timeout) * s.opts.LongpollTimeoutUnit)
	for {
		cur, err := s.fs.list(st.display, st.recursive)
		if err != nil || fingerprint(cur) != want {
			// A folder that disappeared also counts as a change: /continue will then report reset.
			writeJSON(w, http.StatusOK, map[string]any{"changes": true})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"changes": false})
			return
		}
		wait := s.opts.LongpollPollInterval
		if wait > remaining {
			wait = remaining
		}
		if !waitOrDone(r.Context(), wait) {
			return
		}
	}
}
