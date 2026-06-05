# grabr

A small, self-hosted Go service that **crawls, mirrors, and serves websites locally**.
Configure a list of seed URLs through an admin UI, and grabr will periodically walk
each site, store a local copy of every reachable same-host page and asset, snapshot
the previous mirror as a `tar.gz` before each new crawl, and serve the mirrored
content back to you at `/sites/{slug}/...`.

It's designed to be polite to the sites it crawls (adaptive rate limiting, optional
robots.txt enforcement) and small enough to understand end-to-end: one Go binary
with three subcommands (`serve`, `crawl`, `migrate`), a Postgres database, a shared
PVC for mirror/backup files, and a Kubernetes namespace tying them together.

---

## Table of contents

- [What it does](#what-it-does)
- [Architecture](#architecture)
- [Quick start](#quick-start)
  - [Deploy to k3s](#deploy-to-k3s)
  - [Helper scripts](#helper-scripts)
- [Configuration](#configuration)
  - [Environment / Secret values](#environment--secret-values)
  - [Per-site settings](#per-site-settings)
  - [Global settings](#global-settings)
- [Admin UI tour](#admin-ui-tour)
- [How crawling works](#how-crawling-works)
  - [Discovery + download](#discovery--download)
  - [Adaptive rate limiting](#adaptive-rate-limiting)
  - [robots.txt](#robotstxt)
  - [Link rewriting](#link-rewriting)
  - [Analytics stripping](#analytics-stripping)
  - [Reprocessing an existing mirror](#reprocessing-an-existing-mirror)
  - [Embedded viewer](#embedded-viewer)
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

## Architecture

grabr runs as one Go binary with three subcommands:

- `grabr serve` — the **portal**. Long-running Deployment. Renders the admin
  UI, serves `/sites/{slug}/...` mirror traffic, and writes/updates a
  `CronJob` per configured site via the Kubernetes API.
- `grabr crawl --site-id N` — a **one-shot crawl**. Runs inside short-lived
  Jobs (one per scheduled fire, plus one per "Crawl now" click). Talks to
  Postgres for state and writes mirror files to the shared PVC.
- `grabr migrate --sqlite … --postgres …` — a one-off migration tool for
  moving an older SQLite-backed install into the new Postgres database.

The portal does **not** run crawls in-process. Scheduling is handled entirely
by k8s — the portal just reconciles a per-site CronJob and lets the cluster
fire Jobs. Pause = `suspend=true` on the CronJob. Cancel = delete the Job(s).

```
                ┌──────────────────────────────┐
                │  grabr-portal (Deployment)   │
                │  - admin UI / mirror server  │
                │  - PATCH/POST CronJobs       │
                └─────────────┬────────────────┘
                              │ k8s API
                ┌─────────────▼────────────────┐
                │  CronJob/Job per site        │
                │   args: ["crawl",            │
                │     "--site-id","N"]         │
                └──────────────────────────────┘
                  │                       │
                  ▼                       ▼
            ┌──────────┐           ┌──────────────┐
            │ Postgres │           │ PVC (RWO)    │
            │ (state)  │           │ mirrors/     │
            │          │           │ backups/     │
            └──────────┘           └──────────────┘
```

State lives in two places: a Postgres StatefulSet (sites, crawl history,
robots logs, settings) and a single PersistentVolumeClaim mounted by both
the portal and every crawl Job (`/data/mirrors`, `/data/backups`).

---

## Quick start

### Deploy to k3s

The repo ships a single deploy script that targets a remote linux box over
SSH and stands up k3s + Postgres + the portal end-to-end.

```sh
cp .env.example .env
# edit .env and set NAS_HOST=user@your-target-ip

cp k8s/01-secrets.yaml.example k8s/01-secrets.yaml
# edit k8s/01-secrets.yaml and fill in admin credentials + Postgres password

./scripts/k3s-setup.sh
```

The script will, on the target box:

1. Install k3s (skipped if already installed).
2. Fetch a kubeconfig and write it to `~/.kube/config-grabr`.
3. Apply the manifests in `k8s/` (namespace, secrets, PVC, Postgres, portal).
4. Cross-compile the grabr binary for `linux/amd64`, wrap it in a distroless
   image, and import it into the k3s containerd image store.
5. Roll out the portal Deployment and wait for it to become Ready.

When it's done, the portal is reachable at `http://<target-ip>:30080`. Sign
in with the admin credentials from `k8s/01-secrets.yaml`.

To **redeploy after code changes**, just re-run the script. It will rebuild
the binary, re-import the image, and `kubectl rollout restart` the portal.

To also migrate an older SQLite-backed install during the first deploy, run
with `--migrate`. The script will SCP the grabr binary to the target,
port-forward Postgres, and stream files into the PVC via a temporary alpine
sidecar pod.

```sh
./scripts/k3s-setup.sh --migrate
```

### Helper scripts

| Script | What it does |
| ------ | ------------ |
| `scripts/k3s-setup.sh` | The deploy script described above. Accepts `--migrate` and `--reinstall` (destructive: wipes k3s on the target). |
| `scripts/k9s.sh` | Thin wrapper around `k9s` using the kubeconfig from `k3s-setup.sh`, landing in the `grabr` namespace. Requires `brew install k9s`. |

---

## Configuration

### Environment / Secret values

All of the following are injected into the portal pod as Kubernetes Secret
references (see `k8s/01-secrets.yaml.example`). They're listed here as env
vars because that's what `grabr serve` actually reads — but at deploy time
they live in a Secret, not in `.env`.

| Variable | Required | Description |
| -------- | -------- | ----------- |
| `GRABR_ADMIN_USER` | yes | Basic-auth username for every page on the service. |
| `GRABR_ADMIN_PASS` | yes | Basic-auth password. |
| `GRABR_DATABASE_URL` | yes | Postgres DSN. The in-cluster default is `postgres://grabr:…@postgres.grabr.svc.cluster.local:5432/grabr?sslmode=disable`. |
| `GRABR_NAMESPACE` | no (`grabr`) | Kubernetes namespace the portal manages CronJobs/Jobs in. |
| `GRABR_CRAWLER_IMAGE` | yes (in-cluster) | Container image used for crawler CronJob/Job pods. Same image as the portal. |
| `GRABR_PORT` | no (`8080`) | Port the HTTP server binds to. |
| `GRABR_DATA_DIR` | no (`/data`) | Root data directory. Contains `mirrors/` and `backups/`. Mounted from the shared PVC. |

The portal refuses to start without `GRABR_ADMIN_USER`, `GRABR_ADMIN_PASS`,
and `GRABR_DATABASE_URL`. For local Kubernetes-less dev (`go run`), set them
yourself and stand up Postgres separately — see `cmd/grabr/serve.go`.

### Per-site settings

Set in the admin UI when adding or editing a site:

| Setting | Notes |
| ------- | ----- |
| **Display name** | Free-form. Shown on the landing page. |
| **Slug** | Lowercase letters, digits, dashes. Used in URLs (`/sites/{slug}/`) and as the on-disk directory name. |
| **Seed URL** | Absolute `http://` or `https://` URL. The crawl starts here; only same-host URLs reachable from it are followed. |
| **Crawl interval (seconds)** | Minimum 60. Translated to a cron expression on the site's CronJob — the closest divisor of the surrounding unit (minutes, hours, or days). Non-divisor intervals (e.g. 17 min) don't fire on a strict cadence because `*/N` resets at unit boundaries. |
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
- `/view/{slug}/...` — **Embedded viewer.** A persistent grabr top bar with a
  "back to grabr" link, the site name, and a live-updating crumb, plus an
  iframe filling the rest of the viewport pointing at the corresponding
  `/sites/{slug}/...` path. Card links on the landing page point here. See
  [Embedded viewer](#embedded-viewer).
- `/sites/{slug}/...` — **Raw mirror.** Serves the captured content for that
  site, no chrome. Useful for direct linking, scripting, or opening in a
  separate tab.
- `/admin/sites` — **Site table.** List, link to edit, delete.
- `/admin/sites/new` and `/admin/sites/{id}/edit` — **Site form.**
- `/admin/settings` — **Global settings.** Directory paths and new-site defaults.

Per-site card buttons:

| Button | Visible when | Action |
| ------ | ------------ | ------ |
| **Crawl now** | Site is enabled and not currently crawling | Creates a manual `Job` (separate from the CronJob) that runs `grabr crawl --site-id N`. The CronJob's `concurrencyPolicy: Forbid` plus a DB-level "is anything running" check prevent overlap with a scheduled fire. |
| **Pause** | Site is enabled | Sets `spec.suspend=true` on the site's CronJob and deletes any in-flight Job for the site. |
| **Resume** | Site is paused | Clears `spec.suspend` on the CronJob. |
| **Cancel** | A crawl is in flight | Deletes all Jobs labelled with this site's slug and marks the running `crawls` row as `cancelled`. |
| **Reprocess** | Crawl is not in flight | Re-runs the HTML rewriter (link rewriting + analytics stripping) on the existing mirror with no re-download. See [Reprocessing](#reprocessing-an-existing-mirror). |
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

### Analytics stripping

After URL rewriting, every mirrored HTML document is run through an analytics
pass that removes tracker, ad, and session-replay code so the served mirror
(and the offline `tar.gz`) doesn't phone home. Specifically removed:

- `<script src>`, `<iframe src>`, `<img src>` pointing at known analytics or
  ad hosts (suffix match against the embedded list — Google Analytics, GTM,
  DoubleClick, Facebook Pixel, Segment, Mixpanel, Heap, Amplitude, Hotjar,
  FullStory, Microsoft Clarity, Sentry/Bugsnag/NewRelic browser SDKs,
  Yandex Metrica, Intercom, Optimizely, etc.).
- `<link rel="preconnect|dns-prefetch|preload|prefetch">` to those same hosts.
- Inline `<script>` blocks whose body matches well-known tracker initializers
  (`gtag(`, `ga('create')`, `_gaq.push`, `fbq(`, `mixpanel.init`,
  `heap.load`, `amplitude.getInstance`, `hj(`, `clarity(`, `FS.identify`,
  `_hsq.push`, etc.).
- `<noscript>` blocks whose raw text references any of the analytics hosts
  above (catches GA fallback pixels that survive script removal).

Cross-host *non-analytics* resources (fonts, CDN JS/CSS, embedded YouTube
players, recaptcha widgets) are left untouched. The blocklist is conservative:
only obvious trackers, not "could be" hosts. See `internal/crawler/analytics.go`
if you want to extend it.

### Reprocessing an existing mirror

If you've already crawled a site and a rewriter improvement lands (new
analytics host, better link rewriting), you don't need to re-download. Click
**Reprocess** on the site card (works whether the site is enabled or paused,
as long as a crawl isn't already in flight). The job:

1. Walks `<mirrors_dir>/<slug>/` recursively.
2. For each `.html` / `.htm` file, runs `RewriteHTML` against the stored body.
3. Writes the result back atomically only when it actually changed (so file
   mtimes don't churn on idempotent passes).

There's no network access and no entry in the `crawls` table — the result is
returned inline as a flash message ("Reprocessed N HTML file(s) (changed M of
S scanned)"). The reprocess goes through the same per-site busy lock as a
crawl, so kicking a crawl while a reprocess is running (or vice versa) is
dropped with a warning.

### Embedded viewer

Clicking a site on the landing page opens `/view/{slug}/<seed-path>`, which
renders a thin top bar plus an iframe over the mirrored content:

```
+----------------------------------------------------------+
| <- grabr   Site Name   /docs/forms/         Open ^       |
+----------------------------------------------------------+
|                                                          |
|       (iframe showing /sites/<slug>/docs/forms/)         |
|                                                          |
+----------------------------------------------------------+
```

- **← grabr** returns to the index in one click.
- **Crumb** shows the path you're currently viewing inside the mirror.
- **Open** opens the current page directly in a new tab (raw `/sites/...`
  URL, no iframe chrome).

A small same-origin script polls the iframe location every ~750ms and updates
both the crumb and the browser URL bar (`window.history.replaceState`), so a
deep URL inside the iframe is shareable just by copying the address bar.

Cross-host links inside the mirror are tagged with `target="_blank"
rel="noopener noreferrer"` at rewrite time, so clicking e.g. a `github.com`
link inside the viewer opens a new browser tab rather than breaking out of
the iframe.

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

Postgres, kept simple. Time columns are `TIMESTAMPTZ`. Booleans are real
`BOOLEAN`s.

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

On portal startup, every crawl row in `status='running'` is reconciled
against k8s: if there is no active Job for the site's slug, the row is
finalized with `status='failed'` and `error_message='interrupted by
restart'`. Rows whose Jobs are still running are left alone — the crawl pod
will write its own `FinishCrawl` when it completes.

---

## Project layout

```
cmd/grabr/
    main.go                    # subcommand dispatcher (serve | crawl | migrate)
    serve.go                   # portal: HTTP server + reconcile loop
    crawl.go                   # one-shot crawl runner (used by Job pods)
    migrate.go                 # SQLite → Postgres migration tool
    dirs.go                    # shared mirrors_dir / backups_dir resolver
    signals.go                 # SIGINT/SIGTERM → context.Context
internal/config/               # env loader + path resolution
internal/store/                # Postgres pool + typed CRUD (pgx/v5)
internal/k8s/                  # client-go-free k8s API client (CronJob/Job CRUD)
internal/crawler/
    crawler.go                 # per-site orchestrator (Run)
    fetcher.go                 # adaptive rate-limited HTTP client
    parser.go                  # HTML link/asset extractor
    rewriter.go                # same-host URL rewriting
    analytics.go               # analytics/tracker stripping
    reprocess.go               # offline re-run of the rewriter on an existing mirror
internal/backup/               # tar.gz pre-crawl snapshots + retention prune
internal/web/
    server.go                  # chi router, basic-auth, mirror file server, /healthz
    handlers_index.go          # landing + progress fragment
    handlers_admin.go          # CRUD + settings + crawl-now/pause/cancel
    templates/                 # html/template files
    static/                    # CSS + bundled htmx.min.js
k8s/
    00-namespace.yaml
    01-secrets.yaml.example    # copy to 01-secrets.yaml and fill in
    02-storage.yaml            # PVC for /data
    03-postgres.yaml           # StatefulSet + Service
    04-portal.yaml             # Deployment + Service + RBAC
Dockerfile.prebuilt            # wraps bin/grabr-linux in distroless
scripts/
    k3s-setup.sh               # end-to-end deploy to a remote k3s node
    k9s.sh                     # k9s wrapper using the fetched kubeconfig
```

---

## Development

```sh
go build ./...               # build all packages
go vet ./...                 # vet
go test ./...                # tests (only internal/crawler currently has any)
```

To iterate on the Go code without redeploying to k3s, stand up Postgres
yourself (locally or via the in-cluster one with `kubectl port-forward
svc/postgres 5432:5432 -n grabr`) and run:

```sh
GRABR_ADMIN_USER=admin \
GRABR_ADMIN_PASS=test \
GRABR_DATABASE_URL=postgres://grabr:test@127.0.0.1:5432/grabr?sslmode=disable \
GRABR_DATA_DIR=./data \
go run ./cmd/grabr serve
```

Note that without `GRABR_K8S_*` env vars or in-cluster credentials, the k8s
integration is disabled — the portal will start and render the UI, but
Crawl-now and scheduling won't actually create anything.

The Postgres driver is `pgx/v5` (`stdlib`). The deployed image is
distroless `static-debian12:nonroot` wrapping a cross-compiled binary
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`). Templates and static assets are
embedded via `//go:embed`, so the binary needs nothing alongside it except
the data directory.

---

## Operational notes

- **Signals.** Both `grabr serve` and `grabr crawl` translate `SIGINT` /
  `SIGTERM` into a `context.Context` cancellation. The portal drains the
  HTTP server before exiting; crawl Jobs interrupt the active fetch loop.
- **Concurrency model.** Each site has a `CronJob` with
  `concurrencyPolicy: Forbid` and `backoffLimit: 0`, so a scheduled fire
  while an earlier crawl is still running is skipped by k8s. Manual
  "Crawl now" creates a separate `Job` and is additionally gated by a
  DB-level "is anything running for this site" check. Crawls themselves
  are currently single-worker; the `max_concurrent` setting is plumbed
  through for a future widening pass.
- **Reconcile on restart.** When the portal starts, every `crawls` row in
  `status='running'` is checked against the cluster. Rows whose Jobs no
  longer exist are flipped to `failed`; rows with an active Job are left
  alone (the crawl pod will eventually call `FinishCrawl`).
- **Storage.** Mirrors and backups live on a single `ReadWriteOnce` PVC
  (default `grabr-mirrors`, 20Gi, `local-path`). Both the portal and every
  crawl Job mount it at `/data`. Single-node deploys are fine; multi-node
  needs an `RWX` storage class (longhorn, nfs-csi, …).
- **Storage growth.** Each crawl writes a fresh full mirror under
  `mirrors/<slug>/` (no incremental diff) and creates a `tar.gz` backup.
  Plan disk capacity for `(mirror size) + N * (compressed mirror size)` per
  site, where N is `backup_keep_n`.
- **Image distribution.** `scripts/k3s-setup.sh` cross-compiles locally,
  builds the image with `--platform linux/amd64`, and pipes `docker save`
  through SSH into the target's k3s containerd image store. Manifests use
  `imagePullPolicy: Never` to match. No external registry is involved.
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
- Analytics/tracker stripping (GA, GTM, FB Pixel, Hotjar, FullStory, etc.)
- Reprocess action to re-run the rewriter on an existing mirror without re-downloading
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
