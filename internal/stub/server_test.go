package stub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t    *testing.T
	root string
	srv  *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	write(t, root, "Videos/a.mp4", "0123456789")
	write(t, root, "Videos/B.mov", "abc")
	write(t, root, "Videos/notes.txt", "n")
	write(t, root, "Videos/.DS_Store", "x")
	write(t, root, "Documents/doc.pdf", "d")
	s, err := New(Options{Root: root, LongpollPollInterval: 10 * time.Millisecond, LongpollTimeoutUnit: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &fixture{t: t, root: root, srv: ts}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rpc posts a JSON body with a bearer token and decodes the JSON response.
func (f *fixture) rpc(path string, body any) (int, map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	return f.do(req)
}

func (f *fixture) do(req *http.Request) (int, map[string]any) {
	f.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func names(entries any) []string {
	var out []string
	for _, e := range entries.([]any) {
		m := e.(map[string]any)
		out = append(out, m[".tag"].(string)+":"+m["path_display"].(string))
	}
	return out
}

func errTag(body map[string]any, field string) string {
	e, _ := body["error"].(map[string]any)
	if field == "" {
		s, _ := e[".tag"].(string)
		return s
	}
	inner, _ := e[field].(map[string]any)
	s, _ := inner[".tag"].(string)
	return s
}

func TestListFolder(t *testing.T) {
	f := newFixture(t)
	st, body := f.rpc("/2/files/list_folder", map[string]any{"path": ""})
	if st != 200 {
		t.Fatalf("status %d %v", st, body)
	}
	if got := strings.Join(names(body["entries"]), ","); got != "folder:/Documents,folder:/Videos" {
		t.Fatalf("root entries = %s", got)
	}
	st, body = f.rpc("/2/files/list_folder", map[string]any{"path": "/videos"}) // case-insensitive
	if st != 200 {
		t.Fatalf("status %d %v", st, body)
	}
	got := strings.Join(names(body["entries"]), ",")
	if got != "file:/Videos/a.mp4,file:/Videos/B.mov,file:/Videos/notes.txt" {
		t.Fatalf("entries = %s (.DS_Store must be hidden, display keeps on-disk case)", got)
	}
	e := body["entries"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "path_lower", "rev", "size", "server_modified"} {
		if _, ok := e[k]; !ok {
			t.Errorf("file entry lacks %q: %v", k, e)
		}
	}
	if e["path_lower"] != "/videos/a.mp4" || e["size"].(float64) != 10 {
		t.Errorf("entry = %v", e)
	}
}

func TestListFolderPagingAndDelta(t *testing.T) {
	f := newFixture(t)
	_, body := f.rpc("/2/files/list_folder", map[string]any{"path": "/Videos", "limit": 2})
	if body["has_more"] != true || len(body["entries"].([]any)) != 2 {
		t.Fatalf("first page = %v", body)
	}
	_, body = f.rpc("/2/files/list_folder/continue", map[string]any{"cursor": body["cursor"]})
	if body["has_more"] != false || strings.Join(names(body["entries"]), ",") != "file:/Videos/notes.txt" {
		t.Fatalf("second page = %v", body)
	}
	cursor := body["cursor"]

	// Nothing changed: an empty delta.
	_, body = f.rpc("/2/files/list_folder/continue", map[string]any{"cursor": cursor})
	if len(body["entries"].([]any)) != 0 {
		t.Fatalf("delta without changes = %v", body)
	}
	cursor = body["cursor"]

	write(t, f.root, "Videos/new.mp4", "z")
	if err := os.Remove(filepath.Join(f.root, "Videos", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	_, body = f.rpc("/2/files/list_folder/continue", map[string]any{"cursor": cursor})
	if got := strings.Join(names(body["entries"]), ","); got != "file:/Videos/new.mp4,deleted:/Videos/notes.txt" {
		t.Fatalf("delta = %s", got)
	}
}

func TestUnknownCursorIsReset(t *testing.T) {
	f := newFixture(t)
	st, body := f.rpc("/2/files/list_folder/continue", map[string]any{"cursor": "nope"})
	if st != 409 || errTag(body, "") != "reset" {
		t.Fatalf("got %d %v", st, body)
	}
}

func TestPathsCannotLeaveRoot(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	write(t, outside, "secret.mp4", "s")
	if err := os.Symlink(outside, filepath.Join(f.root, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.mp4"), filepath.Join(f.root, "Videos", "secret.mp4")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/../" + filepath.Base(outside), "/Videos/../../x", "/link-out", "/link-out/secret.mp4", "/Videos/secret.mp4"} {
		st, body := f.rpc("/2/files/get_metadata", map[string]any{"path": p})
		if st != 409 {
			t.Errorf("get_metadata %q = %d %v, want 409", p, st, body)
		}
		st, _ = f.rpc("/2/files/delete_v2", map[string]any{"path": p})
		if st != 409 {
			t.Errorf("delete_v2 %q = %d, want 409", p, st)
		}
	}
	// Moving into a link that points outside must not land the file outside.
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/link-out/a.mp4"})
	if st != 409 {
		t.Errorf("move into outside link = %d %v", st, body)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.mp4")); err != nil {
		t.Fatalf("outside file was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "a.mp4")); err == nil {
		t.Fatal("file was moved outside the root")
	}
	// Links out of the root are invisible in listings.
	_, body = f.rpc("/2/files/list_folder", map[string]any{"path": "", "recursive": true})
	for _, n := range names(body["entries"]) {
		if strings.Contains(n, "link-out") || strings.Contains(n, "secret") {
			t.Errorf("listing exposes %s", n)
		}
	}
}

func TestGetMetadataNotFound(t *testing.T) {
	f := newFixture(t)
	st, body := f.rpc("/2/files/get_metadata", map[string]any{"path": "/nope.mp4"})
	if st != 409 || errTag(body, "path") != "not_found" {
		t.Fatalf("got %d %v", st, body)
	}
	if !strings.HasPrefix(body["error_summary"].(string), "path/not_found/") {
		t.Fatalf("error_summary = %v", body["error_summary"])
	}
}

func TestTemporaryLinkServesRanges(t *testing.T) {
	f := newFixture(t)
	st, body := f.rpc("/2/files/get_temporary_link", map[string]any{"path": "/Videos/a.mp4"})
	if st != 200 {
		t.Fatalf("status %d %v", st, body)
	}
	link := body["link"].(string)
	if !strings.HasPrefix(link, f.srv.URL+"/__content/") {
		t.Fatalf("link = %s", link)
	}
	req, _ := http.NewRequest(http.MethodGet, link, nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(got) != "234" {
		t.Fatalf("range = %d %q", resp.StatusCode, got)
	}
	st, body = f.rpc("/2/files/get_temporary_link", map[string]any{"path": "/Videos"})
	if st != 409 || errTag(body, "path") != "not_file" {
		t.Fatalf("folder link = %d %v", st, body)
	}
}

func TestContentRejectsTokensOutsideRoot(t *testing.T) {
	f := newFixture(t)
	tok := base64.RawURLEncoding.EncodeToString([]byte("/../../etc/hosts"))
	resp, err := http.Get(f.srv.URL + "/__content/" + tok)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	_, body := f.rpc("/2/files/search_v2", map[string]any{"query": "A.MP", "options": map[string]any{"path": ""}})
	matches := body["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %v", body)
	}
	md := matches[0].(map[string]any)["metadata"].(map[string]any)["metadata"].(map[string]any)
	if md["path_display"] != "/Videos/a.mp4" {
		t.Fatalf("match = %v", md)
	}
}

func TestMoveAndDelete(t *testing.T) {
	f := newFixture(t)
	_, before := f.rpc("/2/files/get_metadata", map[string]any{"path": "/Videos/a.mp4"})
	st, body := f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/a.mp4", "to_path": "/Videos/renamed.mp4"})
	if st != 200 {
		t.Fatalf("move = %d %v", st, body)
	}
	md := body["metadata"].(map[string]any)
	if md["path_display"] != "/Videos/renamed.mp4" || md["id"] != before["id"] {
		t.Fatalf("moved = %v (id must survive a rename, was %v)", md, before["id"])
	}
	if _, err := os.Stat(filepath.Join(f.root, "Videos", "renamed.mp4")); err != nil {
		t.Fatal(err)
	}

	st, body = f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/renamed.mp4", "to_path": "/Videos/B.mov"})
	if st != 409 || errTag(body, "to") != "conflict" {
		t.Fatalf("conflict = %d %v", st, body)
	}
	st, body = f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/renamed.mp4", "to_path": "/Videos/B.mov", "autorename": true})
	if st != 200 || body["metadata"].(map[string]any)["path_display"] != "/Videos/B (1).mov" {
		t.Fatalf("autorename = %d %v", st, body)
	}
	st, body = f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos/B.mov", "to_path": "/Videos/b.mov"})
	if st != 200 || body["metadata"].(map[string]any)["path_display"] != "/Videos/b.mov" {
		t.Fatalf("case-only rename = %d %v", st, body)
	}
	st, body = f.rpc("/2/files/move_v2", map[string]any{"from_path": "/Videos", "to_path": "/Videos/inner"})
	if st != 409 {
		t.Fatalf("move into itself = %d %v", st, body)
	}

	st, body = f.rpc("/2/files/delete_v2", map[string]any{"path": "/Documents"})
	if st != 200 || body["metadata"].(map[string]any)[".tag"] != "folder" {
		t.Fatalf("delete = %d %v", st, body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Documents")); err == nil {
		t.Fatal("folder still exists")
	}
	st, body = f.rpc("/2/files/delete_v2", map[string]any{"path": ""})
	if st != 409 {
		t.Fatalf("deleting the root = %d %v", st, body)
	}
}

func TestLongpoll(t *testing.T) {
	f := newFixture(t)
	_, body := f.rpc("/2/files/list_folder", map[string]any{"path": "/Videos"})
	cursor := body["cursor"]

	post := func() (int, map[string]any) {
		b, _ := json.Marshal(map[string]any{"cursor": cursor, "timeout": 30})
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/list_folder/longpoll", bytes.NewReader(b))
		return f.do(req)
	}
	// No change: times out (30 * 10ms in this fixture) with changes=false.
	st, body := post()
	if st != 200 || body["changes"] != false {
		t.Fatalf("idle longpoll = %d %v", st, body)
	}
	done := make(chan map[string]any, 1)
	go func() { _, b := post(); done <- b }()
	time.Sleep(30 * time.Millisecond) // sleep-ok: 窓を作る入力: longpoll が待ち始めてから変更を入れる
	write(t, f.root, "Videos/new.mp4", "z")
	select {
	case b := <-done:
		if b["changes"] != true {
			t.Fatalf("longpoll after change = %v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("longpoll did not return after a change")
	}

	b, _ := json.Marshal(map[string]any{"cursor": cursor})
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/list_folder/longpoll", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer t")
	if st, _ := f.do(req); st != 400 {
		t.Fatalf("longpoll with Authorization = %d, want 400", st)
	}
}

func TestThumbnails(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/get_thumbnail_v2", nil)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Dropbox-API-Arg", `{"resource":{".tag":"path","path":"/Videos/a.mp4"},"format":"jpeg","size":"w64h32","mode":"strict"}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		t.Fatalf("thumbnail = %d (%d bytes)", resp.StatusCode, len(data))
	}
	if !strings.Contains(resp.Header.Get("Dropbox-API-Result"), `"/Videos/a.mp4"`) {
		t.Fatalf("Dropbox-API-Result = %q", resp.Header.Get("Dropbox-API-Result"))
	}

	_, body := f.rpc("/2/files/get_thumbnail_batch", map[string]any{"entries": []any{
		map[string]any{"path": "/Videos/a.mp4", "format": map[string]any{".tag": "jpeg"}, "size": map[string]any{".tag": "w32h32"}},
		map[string]any{"path": "/Videos/notes.txt"},
		map[string]any{"path": "/missing.mp4"},
	}})
	entries := body["entries"].([]any)
	tags := []string{}
	for _, e := range entries {
		tags = append(tags, e.(map[string]any)[".tag"].(string))
	}
	if strings.Join(tags, ",") != "success,failure,failure" {
		t.Fatalf("batch tags = %v", tags)
	}
	if entries[0].(map[string]any)["thumbnail"] == "" {
		t.Fatal("batch success lacks thumbnail")
	}
}

func TestAuthRequired(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/list_folder", strings.NewReader(`{"path":""}`))
	st, body := f.do(req)
	if st != 401 || errTag(body, "") != "invalid_access_token" {
		t.Fatalf("got %d %v", st, body)
	}
}

func TestOAuth(t *testing.T) {
	f := newFixture(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	q := url.Values{"redirect_uri": {"http://localhost:43823/auth/callback"}, "state": {"s1"}}
	resp, err := client.Get(f.srv.URL + "/oauth2/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Host != "localhost:43823" || loc.Query().Get("code") == "" || loc.Query().Get("state") != "s1" {
		t.Fatalf("authorize = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Both checks matter: a non-http scheme, and an http URL to another host (the open-redirect case).
	for _, bad := range []string{"https://evil.example/cb", "http://evil.example/cb", "http://192.168.1.2/cb"} {
		q.Set("redirect_uri", bad)
		resp, err = client.Get(f.srv.URL + "/oauth2/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("redirect to %s = %d, want 400", bad, resp.StatusCode)
		}
	}

	resp, err = http.PostForm(f.srv.URL+"/oauth2/token", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}})
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok["access_token"] == nil || tok["refresh_token"] == nil {
		t.Fatalf("token = %v", tok)
	}
}

func TestFaults(t *testing.T) {
	f := newFixture(t)
	b, _ := json.Marshal(Fault{Path: "/2/files/list_folder", Status: 429, Count: 1, RetryAfter: 3})
	resp, err := http.Post(f.srv.URL+"/__stub/faults", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/2/files/list_folder", strings.NewReader(`{"path":""}`))
	req.Header.Set("Authorization", "Bearer t")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "3" {
		t.Fatalf("injected = %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// Count 1: the next call goes through.
	if st, body := f.rpc("/2/files/list_folder", map[string]any{"path": ""}); st != 200 {
		t.Fatalf("after fault = %d %v", st, body)
	}
}
