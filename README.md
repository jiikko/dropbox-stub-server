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
`-addr` only accepts loopback addresses. Paths with `..`, and symlinks that lead outside the
root, are rejected (links out of the root are not listed).

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
| `POST /2/files/search_v2` | Case-insensitive file name match (all words must match). |
| `POST /2/files/move_v2` | Renames / moves on disk. `autorename` adds ` (1)`. The `id` survives a rename. |
| `POST /2/files/delete_v2` | Deletes on disk (folders recursively). |
| `POST /2/files/get_thumbnail_v2` / `get_thumbnail_batch` | A solid-color JPEG of the requested size (color derived from the path) for image and video files; `unsupported_extension` otherwise. |
| `GET /oauth2/authorize` | Redirects straight back to `redirect_uri` (loopback http only) with `code` and the original `state`. |
| `POST /oauth2/token` | Fixed tokens for `authorization_code` (with `refresh_token`) and `refresh_token` grants. |

Any non-empty bearer token is accepted.

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
