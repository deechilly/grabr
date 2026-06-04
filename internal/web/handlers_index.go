package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deechilly/grabr/internal/store"
)

type siteCardVM struct {
	Site          *store.Site
	Latest        *store.CrawlSummary
	StatusLabel   string
	StatusClass   string
	PercentStr    string // 0-100, no '%'
	LatestWhen    string
	NextRun       string
	IntervalLabel string
	LocalHomeURL  string // path to the mirrored copy of the seed URL
}

// localHomeFor returns the embedded-viewer URL for a site's seed page. The
// viewer wraps the mirror in an iframe with a "back to grabr" top bar.
// Derived from the seed URL's path so a seed of https://example.com/docs/
// produces /view/example/docs/.
func localHomeFor(site *store.Site) string {
	base := "/view/" + site.Slug
	u, err := url.Parse(site.SeedURL)
	if err != nil || u.Path == "" || u.Path == "/" {
		return base + "/"
	}
	path := u.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func buildSiteCardVM(site *store.Site, latest *store.CrawlSummary) siteCardVM {
	vm := siteCardVM{
		Site:          site,
		Latest:        latest,
		IntervalLabel: humanDuration(time.Duration(site.IntervalSeconds) * time.Second),
		StatusLabel:   "idle",
		StatusClass:   "",
		PercentStr:    "0",
		NextRun:       "—",
		LocalHomeURL:  localHomeFor(site),
	}
	if latest != nil {
		vm.StatusLabel = latest.Status
		switch latest.Status {
		case "running":
			vm.StatusClass = "running"
		case "ok":
			vm.StatusClass = "ok"
		case "failed":
			vm.StatusClass = "failed"
		case "skipped":
			vm.StatusClass = "skipped"
		case "cancelled":
			vm.StatusClass = "cancelled"
		}
		if latest.PagesDiscovered > 0 {
			pct := (float64(latest.PagesVisited) / float64(latest.PagesDiscovered)) * 100
			if pct > 100 {
				pct = 100
			}
			vm.PercentStr = strconv.FormatFloat(pct, 'f', 1, 64)
		}
		if latest.FinishedAt != nil {
			vm.LatestWhen = humanSince(*latest.FinishedAt) + " ago"
		} else {
			vm.LatestWhen = "since " + humanSince(latest.StartedAt) + " ago"
		}
	}
	// Paused overrides any non-running latest status — but a running crawl
	// stays visible so the cancel button has somewhere to land.
	if !site.Enabled && vm.StatusClass != "running" {
		vm.StatusLabel = "paused"
		vm.StatusClass = "paused"
	}
	if site.Enabled {
		if latest != nil {
			next := latest.StartedAt.Add(time.Duration(site.IntervalSeconds) * time.Second)
			if d := time.Until(next); d > 0 {
				vm.NextRun = "in " + humanDuration(d)
			} else {
				vm.NextRun = "soon"
			}
		} else {
			vm.NextRun = "on next tick"
		}
	} else {
		vm.NextRun = "paused"
	}
	return vm
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sites, err := s.store.ListSites(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	vms := make([]siteCardVM, 0, len(sites))
	for _, site := range sites {
		latest, _ := s.store.LatestCrawlForSite(r.Context(), site.ID)
		vms = append(vms, buildSiteCardVM(site, latest))
	}
	data := struct {
		pageEnvelope
		Sites []siteCardVM
	}{
		pageEnvelope: pageEnvelope{Title: "Sites", Nav: "index"},
		Sites:        vms,
	}
	s.renderPage(w, "index.html", data)
}

func (s *Server) handleSiteProgressFragment(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	site, err := s.store.GetSite(r.Context(), id)
	if err != nil || site == nil {
		http.NotFound(w, r)
		return
	}
	latest, _ := s.store.LatestCrawlForSite(r.Context(), site.ID)
	s.renderFragment(w, "site_progress", buildSiteCardVM(site, latest))
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	if hrs == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hrs)
}

func humanSince(t time.Time) string {
	return humanDuration(time.Since(t))
}
