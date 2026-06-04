package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deechilly/grabr/internal/store"
)

// --- Sites list ---

func (s *Server) handleAdminSites(w http.ResponseWriter, r *http.Request) {
	sites, err := s.store.ListSites(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	data := struct {
		pageEnvelope
		Sites []*store.Site
	}{
		pageEnvelope: pageEnvelope{Title: "Admin · Sites", Nav: "admin", Flash: r.URL.Query().Get("flash"), FlashKind: r.URL.Query().Get("flashKind")},
		Sites:        sites,
	}
	s.renderPage(w, "admin_sites.html", data)
}

// --- Site form (new + edit) ---

type siteFormData struct {
	pageEnvelope
	Site                 *store.Site
	FormAction           string
	RespectRobotsChecked bool
	EnabledChecked       bool
}

func (s *Server) handleAdminSiteNew(w http.ResponseWriter, r *http.Request) {
	data := siteFormData{
		pageEnvelope:         pageEnvelope{Title: "New site", Nav: "admin"},
		Site:                 &store.Site{IntervalSeconds: s.cfg.DefaultIntervalSeconds, BackupKeepN: s.cfg.DefaultBackupKeepN, MaxConcurrent: 4, RateLimitRPS: 1.0},
		FormAction:           "/admin/sites",
		RespectRobotsChecked: true,
		EnabledChecked:       true,
	}
	s.renderPage(w, "admin_site_form.html", data)
}

func (s *Server) handleAdminSiteEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	site, err := s.store.GetSite(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if site == nil {
		http.NotFound(w, r)
		return
	}
	data := siteFormData{
		pageEnvelope:         pageEnvelope{Title: "Edit " + site.Name, Nav: "admin"},
		Site:                 site,
		FormAction:           "/admin/sites/" + strconv.FormatInt(site.ID, 10),
		RespectRobotsChecked: site.RespectRobots,
		EnabledChecked:       site.Enabled,
	}
	s.renderPage(w, "admin_site_form.html", data)
}

func (s *Server) handleAdminSiteCreate(w http.ResponseWriter, r *http.Request) {
	site, err := s.parseSiteForm(r, &store.Site{})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if existing, _ := s.store.GetSiteBySlug(r.Context(), site.Slug); existing != nil {
		http.Error(w, "slug already exists", 400)
		return
	}
	if err := s.store.CreateSite(r.Context(), site); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.scheduler != nil {
		s.scheduler.Add(site)
		if site.Enabled {
			s.scheduler.Kick(site.ID)
		}
	}
	redirect(w, r, "/admin/sites", "Site "+site.Name+" created", "")
}

func (s *Server) handleAdminSiteUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	existing, err := s.store.GetSite(r.Context(), id)
	if err != nil || existing == nil {
		http.NotFound(w, r)
		return
	}
	updated, err := s.parseSiteForm(r, existing)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	updated.ID = existing.ID
	if updated.Slug != existing.Slug {
		if conflict, _ := s.store.GetSiteBySlug(r.Context(), updated.Slug); conflict != nil && conflict.ID != updated.ID {
			http.Error(w, "slug already exists", 400)
			return
		}
	}
	if err := s.store.UpdateSite(r.Context(), updated); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.scheduler != nil {
		s.scheduler.Update(updated)
	}
	redirect(w, r, "/admin/sites", "Site "+updated.Name+" updated", "")
}

func (s *Server) handleAdminSiteDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if err := s.store.DeleteSite(r.Context(), id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.scheduler != nil {
		s.scheduler.Remove(id)
	}
	redirect(w, r, "/admin/sites", "Site deleted", "")
}

func (s *Server) handleAdminSiteTogglePause(w http.ResponseWriter, r *http.Request) {
	site := s.lookupSite(w, r)
	if site == nil {
		return
	}
	site.Enabled = !site.Enabled
	if err := s.store.UpdateSite(r.Context(), site); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.scheduler != nil {
		s.scheduler.Update(site)
		if !site.Enabled {
			// Pausing also cancels any in-flight crawl.
			s.scheduler.Cancel(site.ID)
		}
	}
	s.respondSiteCard(w, r, site, "")
}

func (s *Server) handleAdminSiteReprocess(w http.ResponseWriter, r *http.Request) {
	site := s.lookupSite(w, r)
	if site == nil {
		return
	}
	flash := "Reprocess queued for " + site.Name
	if s.scheduler != nil {
		stats, accepted, err := s.scheduler.Reprocess(r.Context(), site.ID)
		switch {
		case err != nil:
			flash = "Reprocess failed: " + err.Error()
		case !accepted:
			flash = "Reprocess skipped — a crawl is already running"
		default:
			flash = fmt.Sprintf("Reprocessed %d HTML file(s) (changed %d of %d scanned)",
				stats.HTML, stats.Changed, stats.Scanned)
		}
	}
	s.respondSiteCard(w, r, site, flash)
}

func (s *Server) handleAdminSiteCancel(w http.ResponseWriter, r *http.Request) {
	site := s.lookupSite(w, r)
	if site == nil {
		return
	}
	if s.scheduler != nil {
		s.scheduler.Cancel(site.ID)
	}
	s.respondSiteCard(w, r, site, "Crawl cancelled for "+site.Name)
}

func (s *Server) lookupSite(w http.ResponseWriter, r *http.Request) *store.Site {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return nil
	}
	site, err := s.store.GetSite(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return nil
	}
	if site == nil {
		http.NotFound(w, r)
		return nil
	}
	return site
}

// respondSiteCard returns either the refreshed HTMX card fragment or a redirect
// back to the index with an optional flash, depending on the request type.
func (s *Server) respondSiteCard(w http.ResponseWriter, r *http.Request, site *store.Site, flash string) {
	if r.Header.Get("HX-Request") == "true" {
		latest, _ := s.store.LatestCrawlForSite(r.Context(), site.ID)
		s.renderFragment(w, "site_progress", buildSiteCardVM(site, latest))
		return
	}
	if flash == "" {
		flash = "Updated"
	}
	redirect(w, r, "/", flash, "")
}

func (s *Server) handleAdminSiteCrawlNow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	site, err := s.store.GetSite(r.Context(), id)
	if err != nil || site == nil {
		http.NotFound(w, r)
		return
	}
	if s.scheduler != nil {
		s.scheduler.Kick(site.ID)
	}
	// If invoked from HTMX, return the refreshed card fragment so the badge
	// flips to "running" immediately. Otherwise redirect back.
	if r.Header.Get("HX-Request") == "true" {
		latest, _ := s.store.LatestCrawlForSite(r.Context(), site.ID)
		s.renderFragment(w, "site_progress", buildSiteCardVM(site, latest))
		return
	}
	redirect(w, r, "/", "Crawl queued for "+site.Name, "")
}

func (s *Server) parseSiteForm(r *http.Request, into *store.Site) (*store.Site, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return nil, errStr("name is required")
	}
	seed := strings.TrimSpace(r.FormValue("seed_url"))
	u, err := url.Parse(seed)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errStr("seed URL must be an absolute http or https URL")
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		return nil, errStr("could not derive slug from name; please provide one")
	}
	interval, err := strconv.Atoi(r.FormValue("interval_seconds"))
	if err != nil || interval < 60 {
		return nil, errStr("interval must be a number of seconds >= 60")
	}
	keepN, err := strconv.Atoi(r.FormValue("backup_keep_n"))
	if err != nil || keepN < 1 {
		return nil, errStr("backups_keep_n must be >= 1")
	}
	maxConc, err := strconv.Atoi(r.FormValue("max_concurrent"))
	if err != nil || maxConc < 1 {
		return nil, errStr("max_concurrent must be >= 1")
	}
	rps, err := strconv.ParseFloat(r.FormValue("rate_limit_rps"), 64)
	if err != nil || rps <= 0 {
		return nil, errStr("rate_limit_rps must be > 0")
	}

	into.Name = name
	into.Slug = slug
	into.SeedURL = seed
	into.Host = u.Host
	into.IntervalSeconds = interval
	into.BackupKeepN = keepN
	into.MaxConcurrent = maxConc
	into.RateLimitRPS = rps
	into.RespectRobots = r.FormValue("respect_robots") == "1"
	into.Enabled = r.FormValue("enabled") == "1"
	return into, nil
}

// --- Settings ---

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.AllSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	data := struct {
		pageEnvelope
		MirrorsDir             string
		BackupsDir             string
		DefaultIntervalSeconds int
		DefaultBackupKeepN     int
	}{
		pageEnvelope:           pageEnvelope{Title: "Settings", Nav: "settings", Flash: r.URL.Query().Get("flash"), FlashKind: r.URL.Query().Get("flashKind")},
		MirrorsDir:             coalesce(settings["mirrors_dir"], s.cfg.MirrorsDir),
		BackupsDir:             coalesce(settings["backups_dir"], s.cfg.BackupsDir),
		DefaultIntervalSeconds: coalesceInt(settings["default_interval_seconds"], s.cfg.DefaultIntervalSeconds),
		DefaultBackupKeepN:     coalesceInt(settings["default_backup_keep_n"], s.cfg.DefaultBackupKeepN),
	}
	s.renderPage(w, "admin_settings.html", data)
}

func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	mirrors := strings.TrimSpace(r.FormValue("mirrors_dir"))
	backups := strings.TrimSpace(r.FormValue("backups_dir"))
	if mirrors == "" || backups == "" {
		http.Error(w, "mirrors_dir and backups_dir are required", 400)
		return
	}
	defInterval, err := strconv.Atoi(r.FormValue("default_interval_seconds"))
	if err != nil || defInterval < 60 {
		http.Error(w, "default_interval_seconds must be >= 60", 400)
		return
	}
	defKeep, err := strconv.Atoi(r.FormValue("default_backup_keep_n"))
	if err != nil || defKeep < 1 {
		http.Error(w, "default_backup_keep_n must be >= 1", 400)
		return
	}
	ctx := r.Context()
	for k, v := range map[string]string{
		"mirrors_dir":              mirrors,
		"backups_dir":              backups,
		"default_interval_seconds": strconv.Itoa(defInterval),
		"default_backup_keep_n":    strconv.Itoa(defKeep),
	} {
		if err := s.store.SetSetting(ctx, k, v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	redirect(w, r, "/admin/settings", "Settings saved", "")
}

func coalesce(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
func coalesceInt(v string, def int) int {
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

type stringErr string

func (e stringErr) Error() string { return string(e) }
func errStr(s string) error       { return stringErr(s) }

func redirect(w http.ResponseWriter, r *http.Request, target, flash, flashKind string) {
	if flash != "" {
		q := url.Values{}
		q.Set("flash", flash)
		if flashKind != "" {
			q.Set("flashKind", flashKind)
		}
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
