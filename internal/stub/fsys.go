package stub

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errNotFound / errMalformed are mapped to Dropbox-style 409 errors by the handlers.
var (
	errNotFound  = errors.New("not_found")
	errMalformed = errors.New("malformed_path")
)

// ignoredNames are files the real Dropbox client never syncs, so the stub never lists them either.
var ignoredNames = map[string]bool{
	".DS_Store":   true,
	"Icon\r":      true,
	"desktop.ini": true,
	"Thumbs.db":   true,
	".dropbox":    true,
}

// FS maps Dropbox paths ("" = root, "/a/b") onto a local directory.
//
// Every file operation goes through an *os.Root. os.Root refuses, at the system-call level, any
// path whose resolution leaves the root (a ".." component, or a symlink that points outside), and
// it resolves the path in the same call that performs the operation. So there is no window between
// "checked that it is inside" and "deleted / opened / renamed it" (TOCTOU), and a symlink inside
// the root that leads out of it cannot be used to reach outside files. The FS never touches files
// through plain os.* path functions.
//
// Dropbox paths are case-insensitive. Lookups walk each component and fall back to a
// case-insensitive match, so the stub behaves the same on case-sensitive file systems (Linux CI)
// as on the default macOS one. path_display uses the on-disk spelling.
type FS struct {
	root    *os.Root
	rootDir string // absolute, symlinks resolved; for display only

	mu  sync.Mutex
	ids map[string]string // "id:..." -> display path, a cache verified on every use
}

// NewFS opens root (it must be an existing directory).
func NewFS(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(real)
	if err != nil {
		return nil, err
	}
	st, err := r.Stat(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	if !st.IsDir() {
		r.Close()
		return nil, fmt.Errorf("root %q is not a directory", root)
	}
	return &FS{root: r, rootDir: real, ids: map[string]string{}}, nil
}

// Root returns the resolved root directory.
func (f *FS) Root() string { return f.rootDir }

// cleanDropboxPath validates the syntax of a Dropbox path and returns its cleaned form
// ("" for the root, otherwise "/a/b" without a trailing slash).
func cleanDropboxPath(p string) (string, error) {
	if p == "" || p == "/" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", errMalformed
	}
	for _, seg := range strings.Split(p[1:], "/") {
		// The app never sends these; rejecting them keeps the error explicit instead of relying on
		// os.Root to refuse the climb.
		if seg == ".." || seg == "." {
			return "", errMalformed
		}
	}
	c := path.Clean(p)
	if c == "/" {
		return "", nil
	}
	return c, nil
}

// relOf converts a display path ("" or "/A/b") to the os.Root-relative name ("." or "A/b").
func relOf(display string) string {
	if display == "" {
		return "."
	}
	return filepath.FromSlash(display[1:])
}

// lookup resolves a Dropbox path (or "id:...") to an existing entry: its os.Root-relative name and
// its display path.
func (f *FS) lookup(p string) (rel, display string, err error) {
	if strings.HasPrefix(p, "id:") {
		return f.lookupID(p)
	}
	clean, err := cleanDropboxPath(p)
	if err != nil {
		return "", "", err
	}
	display = ""
	if clean != "" {
		for _, seg := range strings.Split(clean[1:], "/") {
			name, err := f.matchName(relOf(display), seg)
			if err != nil {
				return "", "", err
			}
			display += "/" + name
		}
	}
	rel = relOf(display)
	if _, err := f.root.Stat(rel); err != nil {
		return "", "", fsError(err)
	}
	return rel, display, nil
}

// lookupID finds the entry whose stable id is id. The cache is only a hint: the entry it points at
// must still have that id (a deleted file's path may now hold another file), otherwise the tree is
// searched (a parent folder may have been renamed).
func (f *FS) lookupID(id string) (string, string, error) {
	f.mu.Lock()
	cached, ok := f.ids[id]
	f.mu.Unlock()
	if ok {
		if st, err := f.root.Stat(relOf(cached)); err == nil && stableID(st) == id {
			return relOf(cached), cached, nil
		}
		f.mu.Lock()
		delete(f.ids, id)
		f.mu.Unlock()
	}
	entries, err := f.list("", true)
	if err != nil {
		return "", "", err
	}
	for _, m := range entries {
		if m.ID == id {
			return relOf(m.PathDisplay), m.PathDisplay, nil
		}
	}
	return "", "", errNotFound
}

// lookupParent resolves the parent of a (possibly not yet existing) Dropbox path and returns the
// os.Root-relative name and display path the entry would have.
func (f *FS) lookupParent(p string) (rel, display string, err error) {
	clean, err := cleanDropboxPath(p)
	if err != nil {
		return "", "", err
	}
	if clean == "" {
		return "", "", errMalformed
	}
	dir, name := path.Split(clean)
	_, parentDisplay, err := f.lookup(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return "", "", err
	}
	if ignoredNames[name] {
		return "", "", errMalformed
	}
	display = parentDisplay + "/" + name
	return relOf(display), display, nil
}

// fsError maps os errors (including os.Root's "path escapes from parent") to lookup errors.
func fsError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return errNotFound
	}
	// os.Root reports a path that would leave the root as an error that is neither of the above;
	// the stub treats it like a malformed path (the entry does not exist from the app's view).
	return errMalformed
}

func (f *FS) readDir(rel string) ([]os.DirEntry, error) {
	d, err := f.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadDir(-1)
}

func (f *FS) matchName(dirRel, seg string) (string, error) {
	if ignoredNames[seg] {
		return "", errNotFound
	}
	// Always read the directory instead of stat-ing seg: on a case-insensitive file system
	// (the macOS default) stat("videos") succeeds for "Videos", and path_display would echo the
	// requested spelling instead of the on-disk one.
	entries, err := f.readDir(dirRel)
	if err != nil {
		return "", fsError(err)
	}
	folded := ""
	for _, e := range entries {
		if ignoredNames[e.Name()] {
			continue
		}
		if e.Name() == seg {
			return seg, nil
		}
		if folded == "" && strings.EqualFold(e.Name(), seg) {
			folded = e.Name()
		}
	}
	if folded != "" {
		return folded, nil
	}
	return "", errNotFound
}

// Metadata is a Dropbox file / folder / deleted entry. Only the fields the app reads are filled.
type Metadata struct {
	Tag            string `json:".tag"`
	Name           string `json:"name"`
	ID             string `json:"id,omitempty"`
	PathLower      string `json:"path_lower"`
	PathDisplay    string `json:"path_display"`
	Size           *int64 `json:"size,omitempty"`
	Rev            string `json:"rev,omitempty"`
	ServerModified string `json:"server_modified,omitempty"`
	ClientModified string `json:"client_modified,omitempty"`
	IsDownloadable *bool  `json:"is_downloadable,omitempty"`
}

// metadataFor builds the metadata of an existing entry (symlinks inside the root are followed).
func (f *FS) metadataFor(display string) (Metadata, error) {
	st, err := f.root.Stat(relOf(display))
	if err != nil {
		return Metadata{}, fsError(err)
	}
	name := path.Base(display)
	if display == "" {
		name = ""
	}
	m := Metadata{
		Name:        name,
		ID:          stableID(st),
		PathLower:   strings.ToLower(display),
		PathDisplay: display,
	}
	if st.IsDir() {
		m.Tag = "folder"
	} else {
		m.Tag = "file"
		size := st.Size()
		m.Size = &size
		mod := st.ModTime().UTC().Truncate(time.Second).Format(time.RFC3339)
		m.ServerModified = mod
		m.ClientModified = mod
		// Dropbox revs are hex strings of at least 9 characters; this one changes whenever the
		// content (size or mtime) changes, which is what the app uses it for.
		m.Rev = fmt.Sprintf("%09x%x", st.ModTime().UnixNano()&0xfffffffff, size)
		dl := true
		m.IsDownloadable = &dl
	}
	if display != "" {
		f.mu.Lock()
		f.ids[m.ID] = display
		f.mu.Unlock()
	}
	return m, nil
}

// forget drops cached ids under display (after a delete or a move).
func (f *FS) forget(display string) {
	lower := strings.ToLower(display)
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, d := range f.ids {
		dl := strings.ToLower(d)
		if dl == lower || strings.HasPrefix(dl, lower+"/") {
			delete(f.ids, id)
		}
	}
}

// stableID derives an id from the file's identity (device + inode), so renames keep the id
// like they do on Dropbox.
func stableID(st os.FileInfo) string {
	var key string
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		key = fmt.Sprintf("%d:%d", sys.Dev, sys.Ino)
	} else {
		key = st.Name()
	}
	sum := sha1.Sum([]byte(key))
	return "id:" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// list returns the entries under the folder at display path dir (recursive walks the subtree),
// sorted by path_lower so pages are stable. Entries os.Root cannot reach (a symlink leading out of
// the root, a dangling link) are invisible, like files Dropbox cannot sync.
func (f *FS) list(dir string, recursive bool) ([]Metadata, error) {
	var out []Metadata
	var walk func(dirDisplay string) error
	walk = func(dirDisplay string) error {
		entries, err := f.readDir(relOf(dirDisplay))
		if err != nil {
			return fsError(err)
		}
		for _, e := range entries {
			if ignoredNames[e.Name()] {
				continue
			}
			childDisplay := dirDisplay + "/" + e.Name()
			m, err := f.metadataFor(childDisplay)
			if err != nil {
				continue // removed while listing, or unreachable through os.Root
			}
			out = append(out, m)
			if recursive && m.Tag == "folder" {
				// A folder that cannot be read (removed meanwhile, a loop of links) is skipped.
				_ = walk(childDisplay)
			}
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PathLower < out[j].PathLower })
	return out, nil
}
