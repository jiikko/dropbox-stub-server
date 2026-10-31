// Package stub implements a local stand-in for the Dropbox HTTP API that serves a directory
// on disk as the Dropbox root.
package stub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Options configures a Server.
type Options struct {
	// Root is the directory served as the Dropbox root.
	Root string
	// Logger receives one line per request. nil disables request logging.
	Logger *log.Logger
	// LongpollPollInterval is how often longpoll re-reads the folder (default 250ms).
	LongpollPollInterval time.Duration
	// LongpollTimeoutUnit scales the client's "timeout" (seconds). Tests shrink it; default 1s.
	LongpollTimeoutUnit time.Duration
}

// Server is an http.Handler serving the Dropbox API subset the app uses.
type Server struct {
	fs       *FS
	opts     Options
	cursors  *store[cursorState]
	searches *store[searchState]
	faults   *faultStore
	mux      *http.ServeMux
}

// New creates a Server.
func New(opts Options) (*Server, error) {
	fsys, err := NewFS(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.LongpollPollInterval <= 0 {
		opts.LongpollPollInterval = 250 * time.Millisecond
	}
	if opts.LongpollTimeoutUnit <= 0 {
		opts.LongpollTimeoutUnit = time.Second
	}
	s := &Server{fs: fsys, opts: opts, cursors: newStore[cursorState]("stubcursor"), searches: newStore[searchState]("stubsearch"), faults: newFaultStore(), mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

// Root returns the resolved directory served as the Dropbox root.
func (s *Server) Root() string { return s.fs.Root() }

func (s *Server) routes() {
	// api.dropboxapi.com
	s.mux.HandleFunc("POST /2/files/list_folder", s.authed(s.handleListFolder))
	s.mux.HandleFunc("POST /2/files/list_folder/continue", s.authed(s.handleListFolderContinue))
	s.mux.HandleFunc("POST /2/files/get_metadata", s.authed(s.handleGetMetadata))
	s.mux.HandleFunc("POST /2/files/get_temporary_link", s.authed(s.handleGetTemporaryLink))
	s.mux.HandleFunc("POST /2/files/search_v2", s.authed(s.handleSearch))
	s.mux.HandleFunc("POST /2/files/search/continue_v2", s.authed(s.handleSearchContinue))
	s.mux.HandleFunc("POST /2/files/move_v2", s.authed(s.handleMove))
	s.mux.HandleFunc("POST /2/files/delete_v2", s.authed(s.handleDelete))
	s.mux.HandleFunc("POST /oauth2/token", s.handleToken)
	// content.dropboxapi.com
	s.mux.HandleFunc("POST /2/files/get_thumbnail_v2", s.authed(s.handleThumbnail))
	s.mux.HandleFunc("POST /2/files/get_thumbnail_batch", s.authed(s.handleThumbnailBatch))
	// notify.dropboxapi.com (no Authorization header, like the real endpoint)
	s.mux.HandleFunc("POST /2/files/list_folder/longpoll", s.handleLongpoll)
	// www.dropbox.com
	s.mux.HandleFunc("GET /oauth2/authorize", s.handleAuthorize)
	// temporary links
	s.mux.HandleFunc("GET /__content/{token}", s.handleContent)
	s.mux.HandleFunc("HEAD /__content/{token}", s.handleContent)
	// stub control
	s.mux.HandleFunc("GET /__stub/health", s.handleHealth)
	s.mux.HandleFunc("GET /__stub/faults", s.handleFaultsList)
	s.mux.HandleFunc("POST /__stub/faults", s.handleFaultsAdd)
	s.mux.HandleFunc("DELETE /__stub/faults", s.handleFaultsClear)
}

// ServeHTTP applies injected faults, then dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if s.opts.Logger != nil {
			s.opts.Logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	}()
	if !strings.HasPrefix(r.URL.Path, "/__stub/") {
		if f, ok := s.faults.take(r.URL.Path); ok {
			if f.DelayMS > 0 {
				select {
				case <-time.After(time.Duration(f.DelayMS) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			if f.Status > 0 {
				writeInjected(rec, f)
				return
			}
		}
	}
	s.mux.ServeHTTP(rec, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps streaming responses (none today) working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// authed requires a bearer token. Any non-empty token is accepted: the stub has no accounts.
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == "" {
			writeError(w, http.StatusUnauthorized, "invalid_access_token", nil)
			return
		}
		h(w, r)
	}
}

// decodeArg reads the JSON argument from the body (RPC endpoints) or from Dropbox-API-Arg
// (content endpoints).
func decodeArg(r *http.Request, v any) error {
	if arg := r.Header.Get("Dropbox-API-Arg"); arg != "" {
		return json.Unmarshal([]byte(arg), v)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	return json.Unmarshal(body, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// writeError writes a Dropbox-style error: {"error_summary": "...", "error": {".tag": ...}}.
// Endpoint errors (409) nest the cause, e.g. path/not_found/.
func writeError(w http.ResponseWriter, status int, tag string, inner map[string]any) {
	errObj := map[string]any{".tag": tag}
	summary := tag + "/"
	for k, v := range inner {
		errObj[k] = v
		if m, ok := v.(map[string]any); ok {
			if t, ok := m[".tag"].(string); ok {
				summary = tag + "/" + t + "/"
			}
		}
	}
	writeJSON(w, status, map[string]any{"error_summary": summary + "..", "error": errObj})
}

// writeLookupError maps an FS error to the Dropbox error for the given field (path, from_lookup, ...).
func writeLookupError(w http.ResponseWriter, field string, err error) {
	var tag string
	switch {
	case errors.Is(err, errNotFound):
		tag = "not_found"
	case errors.Is(err, errMalformed):
		tag = "malformed_path"
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error_summary": "internal_error/..", "error": map[string]any{".tag": "internal_error"}, "detail": err.Error()})
		return
	}
	writeError(w, http.StatusConflict, field, map[string]any{field: map[string]any{".tag": tag}})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "root": s.fs.Root()})
}

// waitOrDone sleeps for d unless ctx ends first. It reports whether the full duration elapsed.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func badRequest(w http.ResponseWriter, format string, a ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, "Error in call to API function: "+format+"\n", a...)
}
