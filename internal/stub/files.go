package stub

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultListLimit = 2000
	maxListLimit     = 2000
)

func (s *Server) handleListFolder(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	abs, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		writeError(w, http.StatusConflict, "path", map[string]any{"path": map[string]any{".tag": "not_folder"}})
		return
	}
	entries, err := s.fs.list(abs, display, arg.Recursive)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	limit := arg.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = defaultListLimit
	}
	st := &cursorState{abs: abs, display: display, recursive: arg.Recursive, limit: limit, snapshot: entries}
	s.writePage(w, st)
}

// writePage returns the next page of st and stores the advanced cursor.
func (s *Server) writePage(w http.ResponseWriter, st *cursorState) {
	end := st.offset + st.limit
	if end > len(st.snapshot) {
		end = len(st.snapshot)
	}
	page := st.snapshot[st.offset:end]
	next := *st
	next.offset = end
	cursor := s.cursors.put(&next)
	if page == nil {
		page = []Metadata{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":  page,
		"cursor":   cursor,
		"has_more": end < len(st.snapshot),
	})
}

func (s *Server) handleListFolderContinue(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Cursor string `json:"cursor"`
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
	if st.offset < len(st.snapshot) {
		s.writePage(w, st)
		return
	}
	// All pages were returned: report what changed since the snapshot, like Dropbox's delta.
	cur, err := s.fs.list(st.abs, st.display, st.recursive)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusConflict, "reset", nil)
			return
		}
		writeLookupError(w, "path", err)
		return
	}
	changes := diff(st.snapshot, cur)
	if changes == nil {
		changes = []Metadata{}
	}
	next := *st
	next.snapshot = cur
	next.offset = len(cur)
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":  changes,
		"cursor":   s.cursors.put(&next),
		"has_more": false,
	})
}

func (s *Server) handleGetMetadata(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	if arg.Path == "" || arg.Path == "/" {
		// Dropbox does not return metadata for the root.
		badRequest(w, "request body: path: the root folder is unsupported")
		return
	}
	abs, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	m, err := s.fs.metadataFor(abs, display)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleGetTemporaryLink(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	abs, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	m, err := s.fs.metadataFor(abs, display)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	if m.Tag != "file" {
		writeError(w, http.StatusConflict, "path", map[string]any{"path": map[string]any{".tag": "not_file"}})
		return
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(display))
	writeJSON(w, http.StatusOK, map[string]any{
		"metadata": m,
		"link":     baseURL(r) + "/__content/" + token,
	})
}

// baseURL is the origin the client used to reach the stub, so links work through whatever
// address (127.0.0.1 / localhost / port) the app was configured with.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleContent serves a temporary link. http.ServeContent handles Range, which seeking needs.
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.RawURLEncoding.DecodeString(r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	abs, display, err := s.fs.lookup(string(raw))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, filepath.Base(display), st.ModTime(), f)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Query   string `json:"query"`
		Options struct {
			Path       string `json:"path"`
			MaxResults int    `json:"max_results"`
		} `json:"options"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	abs, display, err := s.fs.lookup(arg.Options.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	entries, err := s.fs.list(abs, display, true)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	max := arg.Options.MaxResults
	if max <= 0 || max > 1000 {
		max = 100
	}
	terms := strings.Fields(strings.ToLower(arg.Query))
	matches := []map[string]any{}
	hasMore := false
	for _, m := range entries {
		if !matchesAll(strings.ToLower(m.Name), terms) {
			continue
		}
		if len(matches) == max {
			hasMore = true
			break
		}
		matches = append(matches, map[string]any{
			"match_type": map[string]any{".tag": "filename"},
			"metadata":   map[string]any{".tag": "metadata", "metadata": m},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches, "has_more": hasMore})
}

func matchesAll(name string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	for _, t := range terms {
		if !strings.Contains(name, t) {
			return false
		}
	}
	return true
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		FromPath   string `json:"from_path"`
		ToPath     string `json:"to_path"`
		Autorename bool   `json:"autorename"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	fromAbs, _, err := s.fs.lookup(arg.FromPath)
	if err != nil {
		writeLookupError(w, "from_lookup", err)
		return
	}
	if fromAbs == s.fs.Root() {
		writeError(w, http.StatusConflict, "from_lookup", map[string]any{"from_lookup": map[string]any{".tag": "malformed_path"}})
		return
	}
	toAbs, toDisplay, err := s.fs.lookupParent(arg.ToPath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "no_write_permission"}})
			return
		}
		writeLookupError(w, "to", err)
		return
	}
	if !strings.EqualFold(fromAbs, toAbs) { // a case-only rename is allowed onto "itself"
		if _, err := os.Lstat(toAbs); err == nil {
			if !arg.Autorename {
				writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "conflict", "conflict": map[string]any{".tag": "file"}}})
				return
			}
			toAbs, toDisplay = autorename(toAbs, toDisplay)
		}
	}
	if strings.HasPrefix(toAbs+string(filepath.Separator), fromAbs+string(filepath.Separator)) && !strings.EqualFold(fromAbs, toAbs) {
		writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "cant_move_folder_into_itself"}})
		return
	}
	if err := os.Rename(fromAbs, toAbs); err != nil {
		writeLookupError(w, "from_lookup", err)
		return
	}
	m, err := s.fs.metadataFor(toAbs, toDisplay)
	if err != nil {
		writeLookupError(w, "to", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metadata": m})
}

// autorename appends " (1)", " (2)", ... before the extension until the name is free.
func autorename(abs, display string) (string, string) {
	ext := filepath.Ext(abs)
	baseAbs := strings.TrimSuffix(abs, ext)
	baseDisplay := strings.TrimSuffix(display, ext)
	for i := 1; ; i++ {
		suffix := " (" + strconv.Itoa(i) + ")" + ext
		if _, err := os.Lstat(baseAbs + suffix); errors.Is(err, fs.ErrNotExist) {
			return baseAbs + suffix, baseDisplay + suffix
		}
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	abs, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path_lookup", err)
		return
	}
	if abs == s.fs.Root() {
		writeError(w, http.StatusConflict, "path_lookup", map[string]any{"path_lookup": map[string]any{".tag": "malformed_path"}})
		return
	}
	m, err := s.fs.metadataFor(abs, display)
	if err != nil {
		writeLookupError(w, "path_lookup", err)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeLookupError(w, "path_lookup", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metadata": m})
}
