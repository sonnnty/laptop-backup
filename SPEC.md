# laptop-backup — spec

A local service that mirrors chosen folders on this Linux laptop 1:1 into one Google Drive folder, with a web UI on the laptop.

## Scope
- Linux only. Refuse to start on other platforms (`runtime.GOOS != "linux"`, plus a `//go:build linux` guard where useful).
- Runs only on this laptop as a **systemd user service**; listens on `127.0.0.1:8095` (never 0.0.0.0).
- UI at `http://127.0.0.1:8095`; optionally reachable as `http://backup` (configurable host name) through a local nginx reverse proxy container (see Integration).

## Stack (goal: as light on the laptop as possible)
- Go (single static binary, `CGO_ENABLED=0`), standard library `net/http` for the server.
- Google: `google.golang.org/api/drive/v3` + `golang.org/x/oauth2/google`.
- UI: `html/template` + a little vanilla JS/CSS, embedded with `embed`. No frontend build step, no framework.
- SQLite via `modernc.org/sqlite` (pure Go, no cgo) for the file index and run history.
- Keep dependencies minimal; idle CPU ~0 (scheduler sleeps on a timer, no polling loops); stream files during upload, never load whole files into memory; walk directories without holding the full tree in memory.
- Layout: `cmd/laptop-backup/main.go`, packages under `internal/` (config, drive, sync, index, schedule, web). Makefile with `build`, `test`.

## Paths
- Config: `~/.config/laptop-backup/config.json` (sources, excludes, schedule, Drive folder ID, upload workers and speed limit).
- Secrets: `~/.config/laptop-backup/credentials.json` (OAuth client id/secret) and `token.json` (refresh token), mode 0600. Never committed.
- State: `~/.local/share/laptop-backup/state.db`, logs under the same dir.

## Google Drive
- Destination root folder ID: no default (each user creates their own Drive folder). Set in the Settings UI; while it is empty the dashboard and Settings show “not configured” with a link to Settings, and runs (manual or scheduled) are refused with a clear error. An empty ID is valid when loading the config; a non-empty one must look like a Drive ID.
- Auth: OAuth 2.0 user credentials, scope `https://www.googleapis.com/auth/drive` (a service account has 0 byte quota and cannot upload into a personal Drive).
- Each user brings their own OAuth client: a Desktop-type client in their Google Cloud project with the Drive API enabled and the consent screen published “In production” (in “Testing”, refresh tokens expire after 7 days). README explains the steps. The client id/secret are never in this repo: `install.sh` offers to import `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` from an optional `.env` file (`IMPORT_ENV`), else prompts; a downloaded client JSON can also be dropped in as `credentials.json`. Never import another app's refresh token.
- UI has a “Connect Google account” button: loopback OAuth flow with redirect `http://127.0.0.1:8095/oauth/callback`, `access_type=offline`, `prompt=consent`. Show the connected account email and a Disconnect button. Show a clear error state if the token gets revoked (`invalid_grant`).
- Equivalent Python reference (another project, not in this repo): auth URL with client_id, redirect_uri, response_type=code, scope, access_type=offline, prompt=consent; exchange code at `https://oauth2.googleapis.com/token`.

## Sync semantics (1:1 mirror, no versioned copies)
- Each source folder maps to a Drive subfolder under the root, mirroring its path relative to `$HOME` (e.g. `~/Documents/Personal` → `<root>/Documents/Personal`). Paths outside `$HOME` map under `<root>/_root/...`.
- New local file → upload. Changed file (size or mtime differs from index; confirm with md5 against Drive `md5Checksum` when only mtime changed) → `files().update` on the same Drive file ID, so there is never a duplicate.
- Deleted local file or folder → move the Drive item to trash (`trashed=true`), not permanent delete.
- Folder removed from the source list → its Drive copy is left alone (UI says so).
- Resumable uploads for files > 5 MB; retry with exponential backoff on 429/5xx; one failing file is logged and skipped, it does not abort the run.
- Skip symlinks, sockets, FIFOs, and unreadable files (log them).
- Exclude patterns (glob, editable in UI). Defaults: `node_modules/`, `.venv/`, `venv/`, `__pycache__/`, `.cache/`, `*.tmp`, `*.swp`, `.Trash-*/`, `vendor/`, `.git/`, `.next/`, `dist/`, `build/`, `.terraform/`.
- Uploads run in parallel: the directory walk stays sequential (it creates Drive folders before their files, so no duplicate folders) and hands files that need Drive to `upload_workers` workers (1–8, default 4) through a bounded queue. All uploads finish before deleted files are trashed. A fatal error in any worker (revoked token, storage full, index failure) stops the whole run.
- Optional upload speed limit `upload_limit_kbps` (KiB/s, 0 = unlimited), shared by all workers; changing it in the UI also applies to a run in progress.
- Only one run at a time (lock). Store the Drive file ID per path in the index so reruns are incremental; a “Full rescan” action rebuilds the index from Drive.

## Schedule (set in UI)
- Mode: every N hours (1–24) or daily at HH:MM; plus “Paused”.
- On startup, if the last successful run is older than the schedule interval (laptop was off/asleep), run once shortly after start.
- “Backup now” button runs immediately.

## UI pages
Top nav: ภาพรวม / ประวัติ / โฟลเดอร์ / ตั้งค่า / บันทึก.
1. **Dashboard** (`/`) — Google connection status, next scheduled run, current run progress (files scanned / uploaded / trashed / bytes, current file, and a live upload-speed sparkline while a run is active), a compact summary of the last finished run with a link to History, Backup now / Cancel, and a **usage** section (“การใช้งาน”):
   - stat tiles: files and total size in the backup, runs in the last 30 days with success rate, the service's own memory (`runtime.MemStats` HeapInuse; Sys as a tooltip), state DB size on disk;
   - charts (inline SVG drawn by `static/charts.js`; no chart library, no CDN, no build step; `viewBox` follows the card width, one column under ~700px; colours are CSS variables for light/dark; hover or tap shows exact numbers; every chart has `role="img"` + `aria-label` and a “ยังไม่มีข้อมูล” empty state): bytes uploaded per local day (30 days, empty days included); files uploaded / updated / trashed for the last 30 finished runs with a marker on runs that had errors; duration of the last 30 runs coloured by status; backed-up size and file count per source folder; Google Drive storage meter (Drive / trash / other services / free; “unlimited” when Google reports no limit).
   - data comes from `GET /api/stats` (JSON), loaded once on page load and again when the live poll sees a run finish (no timer). The index scan behind the per-source numbers (`path >= src+'/' AND path < src+'0'`, never LIKE) is cached in memory and redone only after a run finishes or the source list changes (at most once a minute while a run is active). The Drive quota (`about.get(fields=storageQuota)`) is cached for 10 minutes and fetched with a 2 s timeout, so `/api/stats` never waits on a slow Drive; on failure the last value is shown marked as old, else “ไม่สามารถดึงข้อมูลพื้นที่ได้” (or “ยังไม่ได้เชื่อมต่อ Google” when not connected).
2. **History** (`/history`) — every run, newest first: status, counts, size, duration, error link. The first page is rendered by the server; older pages load from `GET /api/runs?before=<id>` (keyset pagination) while scrolling, and a run that finishes while the page is open is added without a reload. The history tbody carries `data-cursor`; `data-next` is reserved for the live “next run” label on the dashboard.
3. **Folders** — list of source folders with add (server-side directory browser starting at `$HOME`) and remove; exclude patterns editor; preview of size/file count per folder.
4. **Settings** — schedule, upload workers and speed limit, Drive root folder ID (with “test access” button), Google connect/disconnect.
5. **Logs** — tail of the service log and per-run error list.
- Thai-friendly, clean UI; works on a narrow window too.

## Integration with the laptop
- `deploy/laptop-backup.service` — systemd **user** unit (`ExecStart=%h/.local/bin/laptop-backup`), `Restart=on-failure`.
- `deploy/backup.conf` — optional nginx server block for a local reverse proxy's `conf.d`: `listen 80; server_name backup; proxy_pass http://127.0.0.1:8095;` with the usual headers, plus websocket/SSE-friendly settings (`proxy_buffering off`) if progress uses SSE.
- The web server only accepts `Host` values `127.0.0.1:<port>`, `localhost:<port>` and the proxy host name (`-host` flag / `LAPTOP_BACKUP_HOST`, default `backup`) as DNS-rebinding protection.
- `install.sh`: `go build` the binary into `~/.local/bin/laptop-backup`, write default config, import OAuth client creds, install & enable the user unit (`systemctl --user enable --now`); when a proxy is configured, write `backup.conf` (with `server_name <PROXY_HOST>`) into the proxy conf.d, `docker exec <container> nginx -s reload`, and add `127.0.0.1   <PROXY_HOST>` to `/etc/hosts` (sudo, only if missing). Each step idempotent and reported; skipped steps say how to enable them.
- `install.sh` settings: environment variable > `install.local.env` next to the script (gitignored, parsed as `KEY=VALUE`, never sourced) > default. Keys: `PROXY_CONF_DIR` (no default: skip proxy and hosts steps), `PROXY_CONTAINER` (`local-proxy`), `PROXY_HOST` (`backup`), `IMPORT_ENV` (no default), `GO_IMAGE` (`golang:1.26`). `install.local.env.example` documents them; `./install.sh --print-config` prints the resolved values. No owner-specific paths, IDs or project names are committed.
- `loginctl enable-linger` is not required (backup only needs to run while logged in) — mention in README.

## Quality
- Tests (`go test ./...`) for: path mapping, exclude matching, diff logic (new/changed/deleted) against a fake Drive client, schedule next-run computation, catch-up logic.
- README in Thai: what it does, install, connect Google, uninstall.
