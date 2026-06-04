package web

import (
	"fmt"
	"net/http"
	"strconv"
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
	}
	if !site.Enabled {
		vm.StatusLabel = "disabled"
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
	if site.Enabled {
		// Best-effort next-run estimate from latest start time.
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
