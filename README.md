# grabr

A small, self-hosted Go service that **crawls, mirrors, and serves websites locally**.
Configure a list of seed URLs through an admin UI, and grabr will periodically walk
each site, store a local copy of every reachable same-host page and asset, snapshot
the previous mirror as a `tar.gz` before each new crawl, and serve the mirrored
content back to you at `/sites/{slug}/...`.

It's designed to be polite to the sites it crawls (adaptive rate limiting, optional
robots.txt enforcement) and small enough to understand end-to-end: one Go binary,
one SQLite file, one data directory, one container.

---

## Table of contents

- [What it does](#what-it-does)
- [Quick start](#quick-start)
  - [Docker Compose](#docker-compose)
  - [Local Go](#local-go)
- [Configuration](#configuration)
  - [Environment variables](#environment-variables)
  - [Per-site settings](#per-site-settings)
  - [Global settings](#global-settings)
- [Admin UI tour](#admin-ui-tour)
- [How crawling works](#how-crawling-works)
  - [Discovery + download](#discovery--download)
  - [Adaptive rate limiting](#adaptive-rate-limiting)
  - [robots.txt](#robotstxt)
  - [Link rewriting](#link-rewriting)
  - [Backups + retention](#backups--retention)
  - [Mirror serving](#mirror-serving)
- [Data model](#data-model)
- [Project layout](#project-layout)
- [Development](#development)
- [Operational notes](#operational-notes)
- [Roadmap and known limitations](#roadmap-and-known-limitations)

---

## What it does

- **Indexes a configurable list of websites.** Each entry has a seed URL, a crawl
  interval, a backup retention count, and a starting rate limit.
- **Crawls slowly and politely.** Starts at a configurable RPS, ramps up only on
  sustained healthy responses, halves on `429` or `5xx`, honors `Retry-After`,
  and respects `robots.txt` (including `Crawl-delay`) by default.
- **Mirrors same-host pages and assets** (HTML, CSS, JS, images, fonts) to disk.
  Rewrites same-host links and asset URLs to relative paths so the mirror works
  whether it's served by grabr or extracted from a `tar.gz` and opened on disk.
- **Snapshots the previous mirror before each new crawl** as a `tar.gz` under
  the backups directory, with a configurable per-site retention count.
- **Serves the live mirror** at `/sites/{slug}/...`.
- **Shows live crawl progress** on the landing page via HTMX-polled fragments —
  a progress bar with a growing denominator (visited / discovered) updated every
  few seconds.
- **Provides per-site controls**: Pause/Resume the schedule, Crawl now, and
  Cancel an in-flight crawl.

---

## Quick start

### Docker Compose

```sh
cp .env.example .env
# edit .env: set GRABR_ADMIN_USER and GRABR_ADMIN_PASS
docker compose up --build
```

Then open <http://localhost:8080/> and sign in with the credentials you set.
Data persists in the named volume `grabr-data` (mounted at `/data` in the
container).

To change the published port:

```sh
GRABR_PORT=9090 docker compose up --build
```

### Local Go

Requires Go 1.26+.

```sh
GRABR_ADMIN_USER=admin \
GRABR_ADMIN_PASS=change-me \
GRABR_DATA_DIR=./data \
go run ./cmd/grabr
```

Open <http://localhost:8080/> and sign in.

> **Pick a stable data directory.** If you set `GRABR_DATA_DIR` to a transient
> path (e.g. `./tmp-data`), nothing inside grabr will clear it, but you might
> wipe it yourself with `rm -rf` between runs. Use `./data` or a fixed absolute
> path if you want state to survive.

---

## Configuration

### Environment variables

| Variable | Default | Description |
| -------- | ------- | ----------- |
| `GRABR_ADMIN_USER` | *required* | Basic-auth username for every page on the service. |
| `GRABR_ADMIN_PASS` | *required* | Basic-auth password. |
| `GRABR_PORT` | `8080` | Port the HTTP server binds to (the listener is `:GRABR_PORT`). |
| `GRABR_DATA_DIR` | `./data` | Root data directory. Contains `grabr.db`, `mirrors/`, and `backups/`. |
| `GRABR_DEFAULT_INTERVAL_SECONDS` | `86400` | Default crawl interval applied to newly-created sites. |
| `GRABR_DEFAULT_BACKUP_KEEP_N` | `5` | Default backup retention count applied to newly-created sites. |

The service refuses to start without `GRABR_ADMIN_USER` and `GRABR_ADMIN_PASS`.

### Per-site settings

Set in the admin UI when adding or editing a site:

| Setting | Notes |
| ------- | ----- |
| **Display name** | Free-form. Shown on the landing page. |
| **Slug** | Lowercase letters, digits, dashes. Used in URLs (`/sites/{slug}/`) and as the on-disk directory name. |
| **Seed URL** | Absolute `http://` or `https://` URL. The crawl starts here; only same-host URLs reachable from it are followed. |
| **Crawl interval (seconds)** | Minimum 60. The scheduler kicks a crawl every interval seconds (and once immediately on create). |
| **Backups to keep** | Rolling retention; older `tar.gz` snapshots are pruned after each crawl. |
| **Initial rate limit (rps)** | Starting requests-per-second. The adaptive controller ramps up or down from here. |
| **Max concurrent fetches** | Ceiling for future concurrency widening. (The current crawler is single-worker; widening is planned.) |
| **Respect robots.txt** | When on, `Disallow` rules and `Crawl-delay` are honored. The body is always fetched and logged regardless. |
| **Enabled** | Off = paused. No scheduled crawls fire and the Crawl-now button is hidden. |

### Global settings

The `/admin/settings` page persists the following keys in the `settings` table.
They are read at startup and override the env defaults:

- `mirrors_dir` — base directory for crawled content.
- `backups_dir` — base directory for `tar.gz` snapshots.
- `default_interval_seconds` — applied to newly-created sites.
- `default_backup_keep_n` — applied to newly-created sites.

> **Restart required.** Changing `mirrors_dir` or `backups_dir` does not move
> existing files. Move them yourself if needed; future crawls will write to
> the new locations. A restart is required for the new paths to take effect.

---

## Admin UI tour

The admin UI is intentionally minimal — server-rendered HTML with HTMX for live
progress updates and inline controls.

- `/` — **Sites landing page.** One card per site. Each card shows the current
  status badge, a progress bar (visited / discovered), the interval, the next
  scheduled run, and per-site controls. Cards poll for fresh state every 3
  seconds via HTMX.
- `/sites/{slug}/...` — **Mirror browse.** Serves the captured content for that
  site. The card link points at the seed URL's mirrored path, so a seed of
  `https://example.com/docs/` opens at `/sites/example/docs/`.
- `/admin/sites` — **Site table.** List, link to edit, delete.
- `/admin/sites/new` and `/admin/sites/{id}/edit` — **Site form.**
- `/admin/settings` — **Global settings.** Directory paths and new-site defaults.

Per-site card buttons:

| Button | Visible when | Action |
| ------ | ------------ | ------ |
| **Crawl now** | Site is enabled and not currently crawling | Kicks a crawl immediately. Goes through the same overlap-skip lock as scheduled runs (a kick during an in-flight crawl is dropped with a warning). |
| **Pause** | Site is enabled | Disables the schedule and cancels any in-flight crawl. |
| **Resume** | Site is paused | Re-enables the schedule. |
| **Cancel** | A crawl is in flight | Interrupts the running crawl; the crawl row is finalized with `status=cancelled`. |
| **Edit** | always | Goes to the site form. |

---

## How crawling works

### Discovery + download

Crawling is single-phase: each fetched page is parsed for same-host links and
assets, which are appended to an in-memory queue. Progress is reported as
`pages_visited / pages_discovered`, where the denominator grows as new links are
found. This means the progress bar can briefly regress in percentage when a
just-fetched page reveals lots of new URLs — that's expected.

The crawler keeps an in-memory visited set keyed by absolute URL (with query
string, no fragment) so the same URL is never enqueued twice in a single crawl.

### Adaptive rate limiting

Each crawl gets its own `Fetcher` with a token-bucket rate limiter:

- Starts at the site's configured `rate_limit_rps`.
- After 10 consecutive `2xx`/`3xx` responses, multiplies the rate by 1.25 (capped
  at `maxRPS = 20`).
- On `429` or `5xx`, halves the rate (floor `minRPS = 0.1`).
- Honors the `Retry-After` header (numeric seconds or HTTP-date), capped at
  60 seconds so a misbehaving server can't park the crawler indefinitely.
- Network errors (timeout, connection refused, etc.) also trigger a back-off.

All rate adjustments are logged with a `fetcher[<slug>]:` prefix.

### robots.txt

Before any other request, the crawler fetches `https://<host>/robots.txt`. The
raw body, status code, and any fetch error are persisted to the `robots_logs`
table — even when robots.txt is *not* being enforced — so you can see exactly
what the target served.

When **Respect robots.txt** is enabled for a site:

- The seed URL is checked first. If it's disallowed, the crawl is marked
  `failed`.
- Every discovered link is filtered through the matched user-agent group's
  `Disallow` rules before being enqueued.
- If the matched group specifies a `Crawl-delay`, the fetcher's max-rate
  ceiling is clamped to `1/delay`, preventing the adaptive controller from
  ramping above the requested rate.

### Link rewriting

For every HTML response, grabr rewrites same-host URLs in `href`, `src`,
`srcset`, `imagesrcset`, `action`, and `data` attributes to **relative** local
paths before writing the file. The rewriter:

- Resolves each URL against the document's base.
- Maps same-host URLs to their on-disk relative path via the writer's
  URL→path scheme.
- Computes the relative path from the current document's directory to the
  target, so links work regardless of where the mirror is rooted.
- Leaves cross-host URLs, anchors (`#section`), `mailto:`, `tel:`,
  `javascript:`, and `data:` URLs untouched.

This means a downloaded `tar.gz` snapshot can be extracted anywhere and opened
directly (`file://...`) without grabr being involved.

### Backups + retention

Immediately before each crawl (after the first), the current mirror directory
for that site is archived to:

```
<backups_dir>/<slug>/<UTC-stamp>.tar.gz
```

(stamp format: `YYYYMMDD-HHMMSS`). The archive root is the slug, so extracting
produces a `<slug>/...` directory tree mirroring the site.

After writing the new archive, older archives beyond the site's
`backup_keep_n` are deleted. The first-ever crawl creates no archive (the
mirror dir is empty).

### Mirror serving

`GET /sites/{slug}/*` serves the captured content for that slug. The handler:

- Looks up the site by slug (404 if not configured).
- Resolves directory-like and extensionless requests to the stored
  `index.html` for that path (matching the writer's "if no extension, treat
  as dir" mapping).
- Returns 404 if the file isn't yet mirrored.

For HTML pages, the rewritten relative links resolve correctly within the
served mirror without further intervention.

---

## Data model

SQLite, kept simple. All time columns store RFC3339 UTC strings. Booleans
are stored as `0` / `1` integers.

- **`sites`** — one row per configured site. Persistent.
- **`crawls`** — one row per crawl attempt. Holds running counters
  (`pages_visited`, `pages_discovered`, `bytes_downloaded`) updated as the
  crawl progresses, plus a final `status` (`running`, `ok`, `failed`,
  `cancelled`, `skipped`) and an optional `error_message`.
- **`visited_urls`** — one row per fetched URL per crawl. Cascades on
  `crawls` deletion.
- **`robots_logs`** — one row per robots.txt fetch. The raw body is
  preserved.
- **`settings`** — key/value store for global settings.
- **`schema_version`** — reserved for future migrations.

On startup, any crawl rows in `status='running'` are marked `failed` with
`error_message='interrupted by restart'` — grabr does not resume mid-flight
crawls, by design.

---

## Project layout

```
cmd/grabr/main.go              # entrypoint: config, store, scheduler, HTTP, signals
internal/config/               # env loader + path resolution
internal/store/                # SQLite open/migrate + typed CRUD
internal/crawler/
    crawler.go                 # per-site orchestrator (Run)
    fetcher.go                 # adaptive rate-limited HTTP client
    parser.go                  # HTML link/asset extractor
    rewriter.go                # same-host URL rewriting
    robots.go                  # robots.txt fetch + parse + Allowed/CrawlDelay
    writer.go                  # URL -> on-disk path mapper + atomic writer
internal/backup/               # tar.gz pre-crawl snapshots + retention prune
internal/scheduler/            # per-site goroutines, locks, kick/cancel API
internal/web/
    server.go                  # chi router, basic-auth, mirror file server
    handlers_index.go          # landing + progress fragment
    handlers_admin.go          # CRUD + settings + crawl-now/pause/cancel
    templates/                 # html/template files
    static/                    # CSS + bundled htmx.min.js
Dockerfile
docker-compose.yml
```

---

## Development

```sh
# Build
go build ./...

# Vet
go vet ./...

# Run with hot config
GRABR_ADMIN_USER=admin GRABR_ADMIN_PASS=test GRABR_DATA_DIR=./data go run ./cmd/grabr
```

The SQLite driver is `modernc.org/sqlite` (pure Go, no CGO). The Docker image
builds as a static binary with `CGO_ENABLED=0` and runs on `distroless/static`.

Templates and static assets are embedded into the binary via `//go:embed`, so
the deployed binary needs nothing alongside it except the data directory.

---

## Operational notes

- **Signals.** `SIGINT` and `SIGTERM` trigger a graceful shutdown: HTTP server
  drains, scheduler stops (cancelling in-flight crawls), SQLite closes (which
  checkpoints the WAL). On the next start, any `running` crawls are marked
  `failed` as described above.
- **Concurrency model.** Each site has its own goroutine inside the scheduler
  with its own ticker and a buffered kick channel. Only one crawl per site can
  be in flight at a time (overlap-skip lock). Crawls themselves are currently
  single-worker; the `max_concurrent` setting is plumbed through for a future
  widening pass.
- **Restart safety.** SQLite is opened in WAL mode with a 5s busy timeout, with
  `max_open_conns=1` to keep the driver's connection behavior predictable
  alongside per-site goroutines.
- **Storage growth.** Each crawl writes a fresh full mirror under
  `mirrors/<slug>/` (no incremental diff) and creates a `tar.gz` backup.
  Plan disk capacity for `(mirror size) + N * (compressed mirror size)` per
  site, where N is `backup_keep_n`.
- **Be a good citizen.** Even with adaptive rate limiting, crawling at high
  RPS against a single host is rude. The defaults (1 RPS initial, 20 RPS hard
  cap, respect robots.txt) are deliberately conservative. Don't raise them
  without a good reason.

---

## Roadmap and known limitations

Implemented:

- Site CRUD and global settings via admin UI
- Adaptive rate limiting with `Retry-After`
- robots.txt fetch, log, and enforce (per-site override)
- Same-host link/asset rewriting in HTML
- Mirror serving at `/sites/{slug}/...`
- Pause/Resume schedule + Cancel in-flight
- Kick-on-create and manual "Crawl now"
- Pre-crawl `tar.gz` backups with per-site retention
- Per-site progress fragment polled via HTMX

Not yet implemented:

- **Concurrency widening.** Crawls are currently single-worker. The
  `max_concurrent` per-site setting is stored but not yet honored.
- **CSS internal URL rewriting.** Absolute URLs inside CSS files (e.g.
  `url(/static/foo.png)`) are not rewritten. Relative `url(...)` inside CSS
  works correctly because the directory structure is preserved.
- **JavaScript URL rewriting.** Out of scope — can't parse JS reliably.
- **Backup browsing/download in the UI.** Snapshots exist on disk under
  `backups/<slug>/`; you can download them manually but there's no UI for it
  yet.
- **robots.txt log viewer.** Rows are written; no page yet to inspect them.
- **Multi-host crawl scope** (e.g. follow subdomains). Currently same-host
  exact match only.

PRs welcome.
