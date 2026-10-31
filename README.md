# dropbox-stub-server

A local stand-in for the Dropbox HTTP API. It serves a directory on your disk as the Dropbox root,
so an app can list, search, play (with seeking), rename and delete real files without touching a
real Dropbox account.

Built for the E2E tests and manual checks of
[Multi-Video Player for Dropbox](https://apps.apple.com/app/id6756692198). It implements the
subset of the API that app uses, not the whole Dropbox API.

## Install / run

```sh
go install github.com/jiikko/dropbox-stub-server@latest
dropbox-stub-server -root ~/Movies/stub-root -addr 127.0.0.1:8765
```

The first line on stdout is `dropbox-stub-server listening on http://...` (scripts can wait for it).
Requests are logged to stderr (`-quiet` turns that off).

**Safety**: there is no authentication, and everything under `-root` can be moved and deleted.
`-addr` only accepts loopback addresses. Every file operation goes through Go's
[`os.Root`](https://pkg.go.dev/os#Root), which refuses (at the system-call level, in the same call
that performs the operation) any path that leaves the root, including through symlinks. Links that
lead out of the root are not listed.

## Run in a container

The `Dockerfile` builds a small image (static binary on distroless). Mount the directory to serve at
`/data` and publish the port **to the host's 127.0.0.1 only**: inside the container the stub listens
on all interfaces (`-allow-non-loopback`), because the container's own loopback is unreachable from
the host.

Docker:

```sh
docker build -t dropbox-stub-server .
docker run --rm -p 127.0.0.1:8765:8765 -v ~/Movies/stub-root:/data dropbox-stub-server
```

Apple's [`container`](https://github.com/apple/container) (macOS):

```sh
container build -t dropbox-stub-server .
container run --rm -p 127.0.0.1:8765:8765 -v ~/Movies/stub-root:/data dropbox-stub-server
```

Inside the container the stub accepts connections on every interface, so other containers and
processes on the same machine that can reach the container's address (Docker's bridge network, the
`192.168.64.x` address Apple's `container` gives each container) can use it too. Use it only with
files you are fine exposing to the local machine; publishing to 127.0.0.1 keeps it off other machines.

Then point the app at `http://127.0.0.1:8765`. Temporary links use the address the client used, so
they work through the published port. Files under the mount are renamed and deleted by the stub
(it runs as root in the container).

## Pointing an app at it

Every endpoint is served from the one origin, whatever host the real API uses
(`api.dropboxapi.com` / `content.dropboxapi.com` / `notify.dropboxapi.com` / `www.dropbox.com`).
Replace the origin and keep the path.

| Real endpoint | Behavior |
|---|---|
| `POST /2/files/list_folder` | Lists the folder (`recursive` and `limit` supported). Paths are case-insensitive; `path_display` uses the on-disk spelling. `.DS_Store` and other files Dropbox never syncs are hidden. |
| `POST /2/files/list_folder/continue` | Next page; once all pages are returned, the changes since then (added / modified entries, `.tag: deleted` for removed ones). Unknown cursors get `reset`. |
| `POST /2/files/list_folder/longpoll` | Waits up to `timeout` seconds (30–480) for the folder to change. No `Authorization` header, like the real one. |
| `POST /2/files/get_metadata` | `path/not_found` (409) when missing. |
| `POST /2/files/get_temporary_link` | A link back to the stub that serves the file with `Range` support. |
| `POST /2/files/search_v2` / `search/continue_v2` | Case-insensitive file name match (all words must match), paged by `max_results` with a cursor. |
| `POST /2/files/move_v2` | Renames / moves on disk. A name that differs only in case counts as taken (`autorename` adds ` (1)`). The `id` survives a rename, including a rename of a parent folder. |
| `POST /2/files/delete_v2` | Deletes on disk (folders recursively). |
| `POST /2/files/get_thumbnail_v2` / `get_thumbnail_batch` | A solid-color JPEG (color derived from the path) for image and video files; `unsupported_extension` otherwise. Only the sizes Dropbox accepts (`w32h32` … `w2048h1536`). |
| `GET /oauth2/authorize` | Redirects straight back to `redirect_uri` (loopback http only) with `code` and the original `state`. |
| `POST /oauth2/token` | Fixed tokens for `authorization_code` (with `refresh_token`) and `refresh_token` grants. |

Any non-empty bearer token is accepted. Temporary links serve regular files only.

Limits:

- At most 1000 list cursors and 100 search cursors are kept (the oldest list cursor then gets
  `reset`, which makes a client list again).
- Symlinks inside the root work only when they are relative and stay inside the root (`os.Root`
  refuses absolute links). A recursive listing enters each directory once, so link loops end.
- Ids come from the file's device + inode. If a file is deleted outside the stub and the inode is
  reused, an old id can name the new file (Dropbox never reuses ids; the stub only sees the disk).
- Two concurrent moves to the same free name are not serialized; the stub serves one local user.
- Renaming a file onto a hard link of itself whose name differs only by Unicode normalization is a
  no-op that still answers 200 (nothing is lost).

What the stub defends against, and what it does not: it is a development tool for one local user.
It keeps every request inside `-root` (through `os.Root`), refuses non-loopback listening outside a
container, bounds memory per request (thumbnail sizes, cursors) and does not overwrite an existing
entry on rename. It does not authenticate requests, and it does not defend against other local
processes changing files under `-root` at the same time.

## Control endpoints

| Endpoint | Use |
|---|---|
| `GET /__stub/health` | `{"ok": true, "root": ...}` |
| `POST /__stub/faults` | Make the next `count` requests to `path` fail or slow down: `{"path": "/2/files/list_folder", "status": 429, "count": 1, "retry_after": 3}`, `{"path": "...", "delay_ms": 2000}`. 401 defaults to `expired_access_token`, 429 to `too_many_requests` (set `tag` to override). |
| `GET /__stub/faults` / `DELETE /__stub/faults` | List / clear the pending faults. |

## Development

```sh
make test   # go test -race
make lint   # gofmt + go vet
make run ROOT=~/Movies/stub-root
```

## License

MIT
