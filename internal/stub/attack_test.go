package stub

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression tests for the red-team review of the stub (paths out of the root, resource bounds).

func TestSymlinkToParentCannotDeleteRoot(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink("..", filepath.Join(f.root, "up")); err != nil {
		t.Fatal(err)
	}
	st, body := f.rpc("/2/files/delete_v2", map[string]any{"path": "/up/" + filepath.Base(f.root)})
	if st == 200 {
		t.Fatalf("delete through up -> .. succeeded: %v", body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Videos", "a.mp4")); err != nil {
		t.Fatalf("root content was deleted: %v", err)
	}
}

func TestLinkOutOfRootCannotReachOutsideEntries(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	// O/back -> R/Videos/a.mp4 (a link outside the root pointing back in), reached through R/out -> O.
	if err := os.Symlink(filepath.Join(f.root, "Videos", "a.mp4"), filepath.Join(outside, "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "out")); err != nil {
		t.Fatal(err)
	}
	if st, body := f.rpc("/2/files/delete_v2", map[string]any{"path": "/out/back"}); st == 200 {
		t.Fatalf("delete of an outside link succeeded: %v", body)
	}
	if st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/out/back", "to_path": "/taken.mp4"}); st == 200 {
		t.Fatalf("move of an outside link succeeded: %v", body)
	}
	if _, err := os.Lstat(filepath.Join(outside, "back")); err != nil {
		t.Fatalf("outside link was touched: %v", err)
	}
}

func TestThumbnailSizeIsBounded(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/get_thumbnail_v2", nil)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Dropbox-API-Arg", `{"resource":{".tag":"path","path":"/Videos/a.mp4"},"size":"w100000h100000"}`)
	if st, _ := f.do(req); st != 400 {
		t.Fatalf("huge thumbnail = %d, want 400", st)
	}
	st, _ := f.rpc("/2/files/get_thumbnail_batch", map[string]any{"entries": []any{map[string]any{"path": "/Videos/a.mp4", "size": "w100000h100000"}}})
	if st != 400 {
		t.Fatalf("huge batch thumbnail = %d, want 400", st)
	}
}

func TestAutorenameGivesUpOnUnusableNames(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("a", 251) + ".mp4" // 255 bytes: " (1)" makes it too long for the file system
	write(t, f.root, long, "x")
	done := make(chan int, 1)
	go func() {
		st, _ := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/" + long, "autorename": true})
		done <- st
	}()
	select {
	case st := <-done:
		if st != 409 {
			t.Fatalf("autorename onto an unusable name = %d, want 409", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("autorename did not finish")
	}
}

func TestStaleIDDoesNotHitAnotherFile(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "b.mp4", "bbb")
	_, meta := f.rpc("/2/files/get_metadata", map[string]any{"path": "/Videos/a.mp4"})
	id := meta["id"].(string)
	if st, _ := f.rpc("/2/files/delete_v2", map[string]any{"path": "/Videos/a.mp4"}); st != 200 {
		t.Fatal("delete failed")
	}
	if st, _ := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/b.mp4", "to_path": "/Videos/a.mp4"}); st != 200 {
		t.Fatal("move failed")
	}
	if st, body := f.rpc("/2/files/delete_v2", map[string]any{"path": id}); st == 200 {
		t.Fatalf("stale id deleted another file: %v", body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Videos", "a.mp4")); err != nil {
		t.Fatalf("the other file was deleted: %v", err)
	}
}

// The id cache can go stale without the stub seeing it (files changed directly on disk, e.g. in
// Finder): the cached path then holds another file, and the id must not resolve to it.
func TestStaleIDAfterOnDiskChangeDoesNotHitAnotherFile(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "b.mp4", "bbb")
	_, meta := f.rpc("/2/files/get_metadata", map[string]any{"path": "/Videos/a.mp4"})
	id := meta["id"].(string)
	a := filepath.Join(f.root, "Videos", "a.mp4")
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.root, "b.mp4"), a); err != nil {
		t.Fatal(err)
	}
	if st, body := f.rpc("/2/files/delete_v2", map[string]any{"path": id}); st == 200 {
		t.Fatalf("stale id deleted another file: %v", body)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("the other file was deleted: %v", err)
	}
}

func TestIDFollowsParentRename(t *testing.T) {
	f := newFixture(t)
	_, meta := f.rpc("/2/files/get_metadata", map[string]any{"path": "/Videos/a.mp4"})
	if st, body0 := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos", "to_path": "/Clips"}); st != 200 {
		t.Fatalf("folder rename failed: %v", body0)
	}
	st, body := f.rpc("/2/files/get_metadata", map[string]any{"path": meta["id"]})
	if st != 200 || body["path_display"] != "/Clips/a.mp4" {
		t.Fatalf("id after parent rename = %d %v", st, body)
	}
}

func TestMoveOntoCaseVariantIsAConflict(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "Target.mp4", "keep")
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/target.mp4"})
	if st != 409 {
		t.Fatalf("move onto a case variant = %d %v, want conflict", st, body)
	}
	got, _ := os.ReadFile(filepath.Join(f.root, "Target.mp4"))
	if string(got) != "keep" {
		t.Fatalf("Target.mp4 was overwritten: %q", got)
	}
}

func TestContentRefusesFIFO(t *testing.T) {
	f := newFixture(t)
	fifo := filepath.Join(f.root, "pipe.mp4")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// If the server does block on the FIFO (the regression), attach a writer at the end so the
	// blocked open returns and httptest.Server.Close (registered earlier, so it runs later) does not
	// wait forever: the regression then shows up as a failure instead of a hung test.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	_, body := f.rpc("/2/files/get_temporary_link", map[string]any{"path": "/pipe.mp4"})
	link, _ := body["link"].(string)
	if link == "" {
		return // refusing the link itself is fine too
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatalf("FIFO link did not answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("FIFO content = %d, want 404", resp.StatusCode)
	}
}

func TestCursorsAreBounded(t *testing.T) {
	s, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxCursors+50; i++ {
		s.cursors.put(&cursorState{})
	}
	if n := s.cursors.len(); n != maxCursors {
		t.Fatalf("cursors kept = %d, want %d", n, maxCursors)
	}
}

func TestSearchPagesWithCursor(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "Videos/c.mp4", "c")
	_, body := f.rpc("/2/files/search_v2", map[string]any{"query": "mp4", "options": map[string]any{"max_results": 1}})
	if body["has_more"] != true || body["cursor"] == nil || len(body["matches"].([]any)) != 1 {
		t.Fatalf("first page = %v", body)
	}
	_, body = f.rpc("/2/files/search/continue_v2", map[string]any{"cursor": body["cursor"]})
	if body["has_more"] != false || len(body["matches"].([]any)) != 1 {
		t.Fatalf("second page = %v", body)
	}
}

func TestUnknownIDInLinkLoopsFinishes(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if err := os.Symlink(".", filepath.Join(f.root, n)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan int, 1)
	go func() { st, _ := f.rpc("/2/files/get_metadata", map[string]any{"path": "id:nonexistent"}); done <- st }()
	select {
	case st := <-done:
		if st != 409 {
			t.Fatalf("unknown id = %d, want 409", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unknown id did not finish (the walk revisits the same directory through the links)")
	}
}

func TestDanglingLinkNameIsTaken(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink("missing", filepath.Join(f.root, "taken.mp4")); err != nil {
		t.Fatal(err)
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/taken.mp4"})
	if st != 409 {
		t.Fatalf("move onto a dangling link = %d %v, want conflict", st, body)
	}
	st, body = f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/taken.mp4", "autorename": true})
	if st != 200 || body["metadata"].(map[string]any)["path_display"] != "/taken (1).mp4" {
		t.Fatalf("autorename past a dangling link = %d %v", st, body)
	}
	if target, err := os.Readlink(filepath.Join(f.root, "taken.mp4")); err != nil || target != "missing" {
		t.Fatalf("the dangling link was replaced: %q %v", target, err)
	}
}

func TestExistingCaseCollisionIsNotOverwritten(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "c.mp4", "lower")
	if err := os.WriteFile(filepath.Join(f.root, "C.mp4"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "c.mp4")); string(got) != "lower" {
		t.Skip("case-insensitive file system: c.mp4 and C.mp4 cannot coexist")
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/c.mp4", "to_path": "/C.mp4"})
	if st != 409 {
		t.Fatalf("move onto an existing case variant = %d %v, want conflict", st, body)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "C.mp4")); string(got) != "upper" {
		t.Fatalf("C.mp4 was overwritten: %q", got)
	}
}

func TestFIFOInTheMiddleOfAPathDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	fifo := filepath.Join(f.root, "pipe.mp4")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	done := make(chan struct{}, 2)
	go func() { f.rpc("/2/files/get_metadata", map[string]any{"path": "/pipe.mp4/child"}); done <- struct{}{} }()
	go func() {
		f.rpc("/2/files/search_v2", map[string]any{"query": "x", "options": map[string]any{"path": "/pipe.mp4"}})
		done <- struct{}{}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a request through the FIFO blocked")
		}
	}
}

func TestHardLinkUnderAnotherNameIsTaken(t *testing.T) {
	f := newFixture(t)
	if err := os.Link(filepath.Join(f.root, "Videos", "a.mp4"), filepath.Join(f.root, "Videos", "b.mp4")); err != nil {
		t.Skipf("link: %v", err)
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/Videos/b.mp4"})
	if st != 409 {
		t.Fatalf("move onto a hard link of itself under another name = %d %v, want conflict", st, body)
	}
}

func TestCaseCollisionIsCheckedInBothDirections(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "c.mp4", "lower")
	if err := os.WriteFile(filepath.Join(f.root, "C.mp4"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "c.mp4")); string(got) != "lower" {
		t.Skip("case-insensitive file system: c.mp4 and C.mp4 cannot coexist")
	}
	for _, mv := range [][2]string{{"/c.mp4", "/C.mp4"}, {"/C.mp4", "/c.mp4"}} {
		if st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": mv[0], "to_path": mv[1]}); st != 409 {
			t.Fatalf("move %s -> %s = %d %v, want conflict", mv[0], mv[1], st, body)
		}
	}
}

func TestFoldEquivalentNameIsTaken(t *testing.T) {
	f := newFixture(t)
	// U+017F (long s) folds to "s" under strings.EqualFold, which lookup uses; the name check must agree.
	write(t, f.root, "ſ.mp4", "long s")
	if got, err := os.ReadFile(filepath.Join(f.root, "s.mp4")); err == nil && string(got) == "long s" {
		t.Skip("the file system folds long s to s itself")
	}
	if st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/s.mp4"}); st != 409 {
		t.Fatalf("move onto a fold-equivalent name = %d %v, want conflict", st, body)
	}
}

func TestNormalizationEquivalentNameIsTaken(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "é.mp4", "decomposed") // é as e + combining acute
	if _, err := os.Stat(filepath.Join(f.root, "é.mp4")); err != nil {
		t.Skip("the file system does not treat composed and decomposed names as the same")
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/é.mp4"})
	if st != 409 {
		t.Fatalf("move onto a normalization-equivalent name = %d %v, want conflict", st, body)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "é.mp4")); string(got) != "decomposed" {
		t.Fatalf("the existing file was overwritten: %q", got)
	}
}

func TestCaseOnlyRenameThroughFolderAlias(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink("Videos", filepath.Join(f.root, "Alias")); err != nil {
		t.Fatal(err)
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/Alias/A.mp4"})
	if st != 200 {
		t.Fatalf("case-only rename through a folder alias = %d %v, want 200", st, body)
	}
	entries, _ := os.ReadDir(filepath.Join(f.root, "Videos"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !strings.Contains(strings.Join(names, ","), "A.mp4") || strings.Contains(strings.Join(names, ","), "A (1).mp4") {
		t.Fatalf("Videos = %v, want a.mp4 renamed to A.mp4", names)
	}
}

func TestCaseOnlyRenameWithUnrelatedHardLink(t *testing.T) {
	f := newFixture(t)
	if err := os.Link(filepath.Join(f.root, "Videos", "a.mp4"), filepath.Join(f.root, "Videos", "other.mp4")); err != nil {
		t.Skipf("link: %v", err)
	}
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/Videos/A.mp4"})
	if st != 200 || body["metadata"].(map[string]any)["path_display"] != "/Videos/A.mp4" {
		t.Fatalf("case-only rename next to an unrelated hard link = %d %v, want 200 to /Videos/A.mp4", st, body)
	}
}
