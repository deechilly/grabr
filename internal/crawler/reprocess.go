package crawler

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ReprocessStats summarizes a reprocess pass.
type ReprocessStats struct {
	Scanned int // total files walked
	HTML    int // HTML files considered
	Changed int // HTML files rewritten on disk
}

// ReprocessMirror walks the existing mirror for a site and re-runs the HTML
// rewriter (link rewriting + analytics stripping) on every .html / .htm file
// in place. No network access. Intended for retroactively applying rewriter
// changes (e.g. analytics stripping) to sites that were crawled before the
// change.
//
// mirrorRoot is the directory containing the site's mirror tree (typically
// <mirrors_dir>/<slug>). host is the original site host — used as the same-
// host filter for URL rewriting and to synthesize document base URLs for
// resolving any remaining absolute URLs.
func ReprocessMirror(ctx context.Context, mirrorRoot, host string) (ReprocessStats, error) {
	var stats ReprocessStats
	host = strings.ToLower(host)

	info, err := os.Stat(mirrorRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return stats, err
	}
	if !info.IsDir() {
		return stats, nil
	}

	err = filepath.Walk(mirrorRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			return nil
		}
		stats.Scanned++
		if !isHTMLFile(info.Name()) {
			return nil
		}
		stats.HTML++

		rel, err := filepath.Rel(mirrorRoot, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Synthesize a base URL so RewriteHTML can resolve any remaining
		// absolute URLs in the document. Same-host references are already
		// relative from the prior crawl, so they round-trip without change.
		base := &url.URL{Scheme: "https", Host: host, Path: "/" + filepath.ToSlash(filepath.Dir(relSlash)) + "/"}

		out := RewriteHTML(base, body, relSlash, host)

		// Skip the disk write when nothing changed — avoids touching mtime
		// on files we didn't actually rewrite.
		if len(out) == len(body) && string(out) == string(body) {
			return nil
		}
		if err := WriteFile(mirrorRoot, relSlash, out); err != nil {
			return err
		}
		stats.Changed++
		return nil
	})
	return stats, err
}

func isHTMLFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm")
}
