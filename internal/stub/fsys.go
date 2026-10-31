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

// errNotFound / errOutsideRoot are mapped to Dropbox-style 409 errors by the handlers.
var (
	errNotFound    = errors.New("not_found")
	errOutsideRoot = errors.New("outside_root")
	errMalformed   = errors.New("malformed_path")
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
// Dropbox paths are case-insensitive. Lookups walk each component and fall back to a
// case-insensitive match, so the stub behaves the same on case-sensitive file systems (Linux CI)
// as on the default macOS one. path_display uses the on-disk spelling.
type FS struct {
	root string // absolute, symlinks resolved

	mu  sync.Mutex
	ids map[string]string // "id:..." -> display path, filled as metadata is produced
}

// NewFS resolves root (it must be an existing directory).
func NewFS(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", root)
	}
	return &FS{root: real, ids: map[string]string{}}, nil
}

// Root returns the resolved root directory.
func (f *FS) Root() string { return f.root }

// cleanDropboxPath validates the syntax of a Dropbox path and returns its cleaned form
// ("" for the root, otherwise "/a/b" without a trailing slash).
func cleanDropboxPath(p string) (string, error) {
	if p == "" || p == "/" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errMalformed
	}
	for _, seg := range strings.Split(p[1:], "/") {
		// Reject ".." outright instead of letting path.Clean fold it: a request that tries to
		// climb is a bug or an attack, never something the app sends.
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

// lookup resolves a Dropbox path (or "id:...") to an existing on-disk path and its display path.
func (f *FS) lookup(p string) (abs, display string, err error) {
	if strings.HasPrefix(p, "id:") {
		f.mu.Lock()
		d, ok := f.ids[p]
		f.mu.Unlock()
		if !ok {
			return "", "", errNotFound
		}
		p = d
	}
	clean, err := cleanDropboxPath(p)
	if err != nil {
		return "", "", err
	}
	cur := f.root
	display = ""
	if clean != "" {
		for _, seg := range strings.Split(clean[1:], "/") {
			name, err := matchName(cur, seg)
			if err != nil {
				return "", "", err
			}
			cur = filepath.Join(cur, name)
			display += "/" + name
		}
	}
	if err := f.checkInside(cur); err != nil {
		return "", "", err
	}
	return cur, display, nil
}

// lookupParent resolves the parent of a (possibly not yet existing) Dropbox path and returns
// the on-disk path the entry would have.
func (f *FS) lookupParent(p string) (abs, display string, err error) {
	clean, err := cleanDropboxPath(p)
	if err != nil {
		return "", "", err
	}
	if clean == "" {
		return "", "", errMalformed
	}
	dir, name := path.Split(clean)
	parentAbs, parentDisplay, err := f.lookup(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return "", "", err
	}
	if ignoredNames[name] {
		return "", "", errMalformed
	}
	return filepath.Join(parentAbs, name), parentDisplay + "/" + name, nil
}

// checkInside rejects paths whose real location (after symlinks) is outside the root.
func (f *FS) checkInside(abs string) error {
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errNotFound
		}
		return err
	}
	if real != f.root && !strings.HasPrefix(real, f.root+string(filepath.Separator)) {
		return errOutsideRoot
	}
	return nil
}

func matchName(dir, seg string) (string, error) {
	if ignoredNames[seg] {
		return "", errNotFound
	}
	// Always read the directory instead of stat-ing seg: on a case-insensitive file system
	// (the macOS default) stat("videos") succeeds for "Videos", and path_display would echo the
	// requested spelling instead of the on-disk one.
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", errNotFound
		}
		return "", err
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

// metadataFor builds the metadata of an existing entry. Symlinks are followed only when they stay
// inside the root (checked by the caller via lookup / list).
func (f *FS) metadataFor(abs, display string) (Metadata, error) {
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Metadata{}, errNotFound
		}
		return Metadata{}, err
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
// sorted by path_lower so pages are stable.
func (f *FS) list(abs, display string, recursive bool) ([]Metadata, error) {
	var out []Metadata
	var walk func(dirAbs, dirDisplay string) error
	walk = func(dirAbs, dirDisplay string) error {
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if ignoredNames[e.Name()] {
				continue
			}
			childAbs := filepath.Join(dirAbs, e.Name())
			childDisplay := dirDisplay + "/" + e.Name()
			if f.checkInside(childAbs) != nil {
				continue // dangling symlink or a link out of the root: invisible, like a file Dropbox cannot sync
			}
			m, err := f.metadataFor(childAbs, childDisplay)
			if err != nil {
				if errors.Is(err, errNotFound) {
					continue // removed while listing
				}
				return err
			}
			out = append(out, m)
			if recursive && m.Tag == "folder" {
				if err := walk(childAbs, childDisplay); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(abs, display); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PathLower < out[j].PathLower })
	return out, nil
}
