package crawler

import (
	"context"
	"errors"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/deechilly/grabr/internal/store"
)

// Backuper is the subset of internal/backup the crawler needs.
type Backuper interface {
	Snapshot(ctx context.Context, slug string) error
}

// Crawler runs one crawl for one site.
type Crawler struct {
	Store      *store.Store
	MirrorsDir string
	Backuper   Backuper
}

// Run executes a single crawl for the given site, blocking until done or ctx
// is canceled. Returns the crawl ID and final status.
func (c *Crawler) Run(ctx context.Context, site *store.Site) (int64, string, error) {
	crawlID, err := c.Store.StartCrawl(ctx, site.ID)
	if err != nil {
		return 0, "failed", err
	}
	finalStatus := "ok"
	var finalErr error
	defer func() {
		msg := ""
		if finalErr != nil {
			msg = finalErr.Error()
		}
		if err := c.Store.FinishCrawl(context.Background(), crawlID, finalStatus, msg); err != nil {
			log.Printf("crawler: finishCrawl: %v", err)
		}
	}()

	// Pre-crawl backup. First-ever crawl: mirror dir is empty → backup is a no-op.
	if c.Backuper != nil {
		if err := c.Backuper.Snapshot(ctx, site.Slug); err != nil {
			log.Printf("crawler: backup snapshot for %s: %v", site.Slug, err)
		}
	}

	seedURL, parseErr := url.Parse(site.SeedURL)
	if parseErr != nil {
		finalStatus = "failed"
		finalErr = parseErr
		return crawlID, finalStatus, parseErr
	}
	if seedURL.Host == "" {
		finalStatus = "failed"
		finalErr = errors.New("seed URL missing host")
		return crawlID, finalStatus, finalErr
	}
	host := strings.ToLower(seedURL.Host)
	mirrorRoot := filepath.Join(c.MirrorsDir, site.Slug)

	fetcher := NewFetcherWithTag(site.RateLimitRPS, site.Slug)

	// robots.txt — always fetched and logged; only enforced when site opts in.
	robots := FetchRobotsTxt(ctx, fetcher, seedURL)
	if err := c.Store.AddRobotsLog(ctx, site.ID, robots.StatusCode, robots.Raw, robots.FetchError); err != nil {
		log.Printf("crawler[%s]: addRobotsLog: %v", site.Slug, err)
	}
	if delay := robots.CrawlDelay(); delay > 0 {
		fetcher.SetMaxRate(1.0 / delay.Seconds())
	}
	enforceRobots := site.RespectRobots
	allowed := func(u *url.URL) bool {
		if !enforceRobots {
			return true
		}
		return robots.Allowed(u.Path)
	}
	if enforceRobots && !allowed(seedURL) {
		finalStatus = "failed"
		finalErr = errors.New("seed URL is disallowed by robots.txt")
		return crawlID, finalStatus, finalErr
	}

	type job struct{ url *url.URL }
	var (
		queue   = []job{{url: seedURL}}
		visited = map[string]struct{}{seedURL.String(): {}}
		mu      sync.Mutex
	)
	if err := c.Store.BumpCrawlProgress(ctx, crawlID, 0, 1, 0); err != nil {
		log.Printf("crawler: bumpProgress: %v", err)
	}

	// v1 is single-worker. Adaptive widening is a follow-up.
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				finalStatus = "cancelled"
			} else {
				finalStatus = "failed"
			}
			finalErr = err
			return crawlID, finalStatus, finalErr
		}
		mu.Lock()
		if len(queue) == 0 {
			mu.Unlock()
			break
		}
		j := queue[0]
		queue = queue[1:]
		mu.Unlock()

		resp, fetchErr := fetcher.Get(ctx, j.url.String())
		if fetchErr != nil {
			_ = c.Store.AddVisitedURL(ctx, crawlID, j.url.String(), 0, "", 0, fetchErr.Error())
			_ = c.Store.BumpCrawlProgress(ctx, crawlID, 1, 0, 0)
			continue
		}
		bytesN := int64(len(resp.Body))
		_ = c.Store.AddVisitedURL(ctx, crawlID, j.url.String(), resp.StatusCode, resp.ContentType, bytesN, "")
		_ = c.Store.BumpCrawlProgress(ctx, crawlID, 1, 0, bytesN)

		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			continue
		}

		// Persist to disk. For HTML, rewrite same-host URLs to local relative
		// paths so the mirror works both served and extracted.
		relPath := LocalPathFor(j.url)
		bodyToWrite := resp.Body
		if LooksLikeHTML(resp.ContentType) {
			bodyToWrite = RewriteHTML(j.url, resp.Body, relPath, host)
		}
		if err := WriteFile(mirrorRoot, relPath, bodyToWrite); err != nil {
			log.Printf("crawler: write %s: %v", j.url, err)
		}

		if !LooksLikeHTML(resp.ContentType) {
			continue
		}

		// Discover more links from the ORIGINAL body so we still see the
		// absolute same-host URLs (rewriting would have turned them relative).
		links := ExtractLinks(j.url, resp.Body)
		var added int
		mu.Lock()
		for _, l := range links {
			if !strings.EqualFold(l.Host, host) {
				continue
			}
			if !allowed(l) {
				continue
			}
			key := l.String()
			if _, seen := visited[key]; seen {
				continue
			}
			visited[key] = struct{}{}
			queue = append(queue, job{url: l})
			added++
		}
		mu.Unlock()
		if added > 0 {
			_ = c.Store.BumpCrawlProgress(ctx, crawlID, 0, added, 0)
		}
	}

	return crawlID, finalStatus, nil
}
