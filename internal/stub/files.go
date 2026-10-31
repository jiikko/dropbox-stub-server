package stub

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultListLimit = 2000
	maxListLimit     = 2000
	// maxAutorename bounds the " (N)" search so a name that can never be free (too long for the
	// file system, permission errors) ends with a conflict instead of looping forever.
	maxAutorename = 1000
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
	_, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	m, err := s.fs.metadataFor(display)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	if display != "" && m.Tag != "folder" {
		writeError(w, http.StatusConflict, "path", map[string]any{"path": map[string]any{".tag": "not_folder"}})
		return
	}
	entries, err := s.fs.list(display, arg.Recursive)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	limit := arg.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = defaultListLimit
	}
	st := &cursorState{display: display, recursive: arg.Recursive, limit: limit, snapshot: entries}
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
	cur, err := s.fs.list(st.display, st.recursive)
	if err != nil {
		// The folder itself is gone (or unreachable): the app lists again after a reset.
		writeError(w, http.StatusConflict, "reset", nil)
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
	_, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	m, err := s.fs.metadataFor(display)
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
	_, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	m, err := s.fs.metadataFor(display)
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
// Only regular files are served: a FIFO or a device would block the open (or never end).
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.RawURLEncoding.DecodeString(r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, display, err := s.fs.lookup(string(raw))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// O_NONBLOCK keeps a FIFO swapped in after the lookup from blocking the open; the fstat below
	// then refuses anything that is not a regular file.
	f, err := s.fs.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, path.Base(display), st.ModTime(), f)
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
	_, display, err := s.fs.lookup(arg.Options.Path)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	entries, err := s.fs.list(display, true)
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	max := arg.Options.MaxResults
	if max <= 0 || max > 1000 {
		max = 100
	}
	terms := strings.Fields(strings.ToLower(arg.Query))
	var matches []Metadata
	for _, m := range entries {
		if matchesAll(strings.ToLower(m.Name), terms) {
			matches = append(matches, m)
		}
	}
	s.writeSearchPage(w, &searchState{matches: matches, pageSize: max})
}

// writeSearchPage returns one page of a search; a cursor for /2/files/search/continue_v2 comes with
// has_more, like the real API.
func (s *Server) writeSearchPage(w http.ResponseWriter, st *searchState) {
	end := st.offset + st.pageSize
	if end > len(st.matches) {
		end = len(st.matches)
	}
	page := make([]map[string]any, 0, end-st.offset)
	for _, m := range st.matches[st.offset:end] {
		page = append(page, map[string]any{
			"match_type": map[string]any{".tag": "filename"},
			"metadata":   map[string]any{".tag": "metadata", "metadata": m},
		})
	}
	resp := map[string]any{"matches": page, "has_more": end < len(st.matches)}
	if end < len(st.matches) {
		next := *st
		next.offset = end
		resp["cursor"] = s.searches.put(&next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSearchContinue(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	st, ok := s.searches.get(arg.Cursor)
	if !ok {
		writeError(w, http.StatusConflict, "internal_error", nil)
		return
	}
	s.writeSearchPage(w, st)
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
	fromRel, fromDisplay, err := s.fs.lookup(arg.FromPath)
	if err != nil {
		writeLookupError(w, "from_lookup", err)
		return
	}
	if fromRel == "." {
		writeError(w, http.StatusConflict, "from_lookup", map[string]any{"from_lookup": map[string]any{".tag": "malformed_path"}})
		return
	}
	toRel, toDisplay, err := s.fs.lookupParent(arg.ToPath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "no_write_permission"}})
			return
		}
		writeLookupError(w, "to", err)
		return
	}
	if strings.HasPrefix(strings.ToLower(toDisplay), strings.ToLower(fromDisplay)+"/") {
		writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "cant_move_folder_into_itself"}})
		return
	}
	// Dropbox names are case-insensitive, so "taken" means any entry in the target folder whose name
	// matches case-insensitively, including a dangling link (os.Rename would replace it). Renaming an
	// entry onto itself (a case-only rename) is allowed only when it is the same file on disk: a root
	// that already holds a.mp4 and A.mp4 on a case-sensitive file system must not lose one of them.
	if taken, err := s.fs.nameTaken(toDisplay, fromRel); err != nil || taken {
		if !arg.Autorename {
			writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "conflict", "conflict": map[string]any{".tag": "file"}}})
			return
		}
		var ok bool
		toRel, toDisplay, ok = s.autorename(toDisplay)
		if !ok {
			writeError(w, http.StatusConflict, "to", map[string]any{"to": map[string]any{".tag": "conflict", "conflict": map[string]any{".tag": "file"}}})
			return
		}
	}
	// Known limit: two concurrent moves to the same free name can both pass the check above; the
	// stub serves one local user, so it does not lock across requests.
	if err := s.fs.root.Rename(fromRel, toRel); err != nil {
		writeLookupError(w, "from_lookup", fsError(err))
		return
	}
	s.fs.forget(fromDisplay)
	m, err := s.fs.metadataFor(toDisplay)
	if err != nil {
		writeLookupError(w, "to", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metadata": m})
}

// takenNames returns the lower-cased names in the folder of display (a display path whose parent
// exists). An unreadable folder is an error: callers treat it as "taken" rather than overwrite.
func (f *FS) takenNames(display string) (map[string]os.DirEntry, error) {
	entries, err := f.readDir(relOf(path.Dir(display)))
	if err != nil {
		return nil, err
	}
	names := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		names[strings.ToLower(e.Name())] = e
	}
	return names, nil
}

// nameTaken reports whether display's name is used in its folder by an entry other than selfRel.
func (f *FS) nameTaken(display, selfRel string) (bool, error) {
	names, err := f.takenNames(display)
	if err != nil {
		return true, err
	}
	e, ok := names[strings.ToLower(path.Base(display))]
	if !ok {
		return false, nil
	}
	self, err1 := f.root.Lstat(selfRel)
	other, err2 := f.root.Lstat(relOf(path.Join(path.Dir(display), e.Name())))
	if err1 == nil && err2 == nil && os.SameFile(self, other) {
		return false, nil // a case-only rename of the entry itself
	}
	return true, nil
}

// autorename appends " (1)", " (2)", ... before the extension until the name is free. The folder is
// read once; names that cannot be created (too long for the file system) end the search.
func (s *Server) autorename(display string) (string, string, bool) {
	names, err := s.fs.takenNames(display)
	if err != nil {
		return "", "", false
	}
	ext := path.Ext(display)
	base := strings.TrimSuffix(display, ext)
	for i := 1; i <= maxAutorename; i++ {
		candidate := base + " (" + strconv.Itoa(i) + ")" + ext
		if _, taken := names[strings.ToLower(path.Base(candidate))]; taken {
			continue
		}
		if _, statErr := s.fs.root.Lstat(relOf(candidate)); statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return "", "", false
		}
		return relOf(candidate), candidate, true
	}
	return "", "", false
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	rel, display, err := s.fs.lookup(arg.Path)
	if err != nil {
		writeLookupError(w, "path_lookup", err)
		return
	}
	if rel == "." {
		writeError(w, http.StatusConflict, "path_lookup", map[string]any{"path_lookup": map[string]any{".tag": "malformed_path"}})
		return
	}
	m, err := s.fs.metadataFor(display)
	if err != nil {
		writeLookupError(w, "path_lookup", err)
		return
	}
	if err := s.fs.root.RemoveAll(rel); err != nil {
		writeLookupError(w, "path_lookup", fsError(err))
		return
	}
	s.fs.forget(display)
	writeJSON(w, http.StatusOK, map[string]any{"metadata": m})
}
