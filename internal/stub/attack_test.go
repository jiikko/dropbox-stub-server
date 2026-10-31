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

func TestIDFollowsParentRename(t *testing.T) {
	f := newFixture(t)
	_, meta := f.rpc("/2/files/get_metadata", map[string]any{"path": "/Videos/a.mp4"})
	if st, _ := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos", "to_path": "/Clips"}); st != 200 {
		t.Fatal("folder rename failed")
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
	if err := syscall.Mkfifo(filepath.Join(f.root, "pipe.mp4"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
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
