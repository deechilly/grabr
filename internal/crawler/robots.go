package crawler

import (
	"context"
	"net/url"
	"time"

	"github.com/temoto/robotstxt"
)

// RobotsPolicy is what the crawler uses to decide whether to visit a URL and
// whether the parsed Crawl-delay should override the configured rate.
type RobotsPolicy struct {
	group *robotstxt.Group // group matched against userAgent; nil = no group matched

	// Raw holds the fetched body for persistence. Empty if not fetched / error.
	Raw        string
	StatusCode int
	FetchError string
}

// Allowed returns true if the policy permits fetching urlPath. When no policy
// was loaded (no robots.txt fetched, or parsing failed), it permits all paths.
func (p *RobotsPolicy) Allowed(urlPath string) bool {
	if p == nil || p.group == nil {
		return true
	}
	return p.group.Test(urlPath)
}

// CrawlDelay returns the Crawl-delay from robots.txt for the matched group,
// or 0 if none.
func (p *RobotsPolicy) CrawlDelay() time.Duration {
	if p == nil || p.group == nil {
		return 0
	}
	return p.group.CrawlDelay
}

// FetchRobotsTxt retrieves /robots.txt for the host of seedURL and returns a
// RobotsPolicy. A non-200 response or parse error returns a "permissive"
// policy (no rules), with the failure recorded for logging.
func FetchRobotsTxt(ctx context.Context, fetcher *Fetcher, seedURL *url.URL) *RobotsPolicy {
	robotsURL := &url.URL{Scheme: seedURL.Scheme, Host: seedURL.Host, Path: "/robots.txt"}
	resp, err := fetcher.Get(ctx, robotsURL.String())
	if err != nil {
		return &RobotsPolicy{FetchError: err.Error()}
	}
	pol := &RobotsPolicy{StatusCode: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 4xx (notably 404) is the "no robots.txt" case; per RFC 9309 the
		// crawler should assume everything is allowed.
		pol.Raw = string(resp.Body)
		return pol
	}
	pol.Raw = string(resp.Body)
	robots, err := robotstxt.FromBytes(resp.Body)
	if err != nil {
		pol.FetchError = err.Error()
		return pol
	}
	pol.group = robots.FindGroup(userAgent)
	return pol
}
