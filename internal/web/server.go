package web

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deechilly/grabr/internal/config"
	"github.com/deechilly/grabr/internal/k8s"
	"github.com/deechilly/grabr/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Server struct {
	cfg          *config.Config
	store        *store.Store
	k8s          *k8s.Client // nil if k8s integration is unavailable
	mirrorsDir   string
	fragmentTpls *template.Template

	pageMu  sync.RWMutex
	pageTpl map[string]*template.Template
}

func New(cfg *config.Config, st *store.Store, kube *k8s.Client, mirrorsDir string) (*Server, error) {
	frag, err := template.New("").ParseFS(templatesFS, "templates/site_progress.html")
	if err != nil {
		return nil, fmt.Errorf("parse fragment templates: %w", err)
	}
	return &Server{
		cfg:          cfg,
		store:        st,
		k8s:          kube,
		mirrorsDir:   mirrorsDir,
		fragmentTpls: frag,
		pageTpl:      map[string]*template.Template{},
	}, nil
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	staticSub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// No-auth liveness/readiness endpoint. The kubelet treats 2xx-3xx as
	// healthy, so the previous probe (GET / → 401) was never actually
	// succeeding — pods stayed 0/1 Ready forever.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Group(func(r chi.Router) {
		r.Use(s.basicAuth)
		r.Get("/", s.handleIndex)
		r.Get("/fragments/site/{id}/progress", s.handleSiteProgressFragment)
		r.Get("/sites/{slug}", s.handleMirrorRedirect)
		r.Get("/sites/{slug}/*", s.handleMirror)
		r.Get("/view/{slug}", s.handleViewerRedirect)
		r.Get("/view/{slug}/*", s.handleViewer)

		r.Route("/admin", func(r chi.Router) {
			r.Get("/sites", s.handleAdminSites)
			r.Get("/sites/new", s.handleAdminSiteNew)
			r.Post("/sites", s.handleAdminSiteCreate)
			r.Get("/sites/{id}/edit", s.handleAdminSiteEdit)
			r.Post("/sites/{id}", s.handleAdminSiteUpdate)
			r.Post("/sites/{id}/delete", s.handleAdminSiteDelete)
			r.Post("/sites/{id}/crawl-now", s.handleAdminSiteCrawlNow)
			r.Post("/sites/{id}/toggle-pause", s.handleAdminSiteTogglePause)
			r.Post("/sites/{id}/cancel", s.handleAdminSiteCancel)
			r.Post("/sites/{id}/reprocess", s.handleAdminSiteReprocess)
			r.Get("/settings", s.handleAdminSettings)
			r.Post("/settings", s.handleAdminSettingsSave)
		})
	})

	return r
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AdminUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="grabr"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type pageEnvelope struct {
	Title     string
	Nav       string
	Flash     string
	FlashKind string
}

func (s *Server) renderPage(w http.ResponseWriter, pageFile string, data any) {
	s.pageMu.RLock()
	t := s.pageTpl[pageFile]
	s.pageMu.RUnlock()
	if t == nil {
		parsed, err := template.New("").ParseFS(templatesFS, "templates/layout.html", "templates/"+pageFile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := parsed.ParseFS(templatesFS, "templates/site_progress.html"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.pageMu.Lock()
		s.pageTpl[pageFile] = parsed
		s.pageMu.Unlock()
		t = parsed
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.fragmentTpls.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleMirrorRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
}

func (s *Server) handleViewerRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
}

func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	site, err := s.store.GetSiteBySlug(r.Context(), slug)
	if err != nil || site == nil {
		http.NotFound(w, r)
		return
	}
	prefix := "/view/" + slug
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		rest = "/"
	}
	iframeSrc := "/sites/" + slug + rest
	if r.URL.RawQuery != "" {
		iframeSrc += "?" + r.URL.RawQuery
	}
	data := struct {
		Site        *store.Site
		Slug        string
		InitialPath string
		IframeSrc   string
	}{
		Site:        site,
		Slug:        slug,
		InitialPath: rest,
		IframeSrc:   iframeSrc,
	}
	s.renderViewer(w, data)
}

func (s *Server) renderViewer(w http.ResponseWriter, data any) {
	const name = "viewer.html"
	s.pageMu.RLock()
	t := s.pageTpl[name]
	s.pageMu.RUnlock()
	if t == nil {
		parsed, err := template.New("").ParseFS(templatesFS, "templates/"+name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.pageMu.Lock()
		s.pageTpl[name] = parsed
		s.pageMu.Unlock()
		t = parsed
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleMirror(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	site, err := s.store.GetSiteBySlug(r.Context(), slug)
	if err != nil || site == nil {
		http.NotFound(w, r)
		return
	}
	root := filepath.Join(s.mirrorsDir, slug)
	prefix := "/sites/" + slug + "/"
	http.StripPrefix(prefix, serveMirrorFS(root)).ServeHTTP(w, r)
}

func serveMirrorFS(root string) http.Handler {
	fs := http.Dir(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if urlPath == "" || strings.HasSuffix(urlPath, "/") {
			tryIndex := filepath.Join(urlPath, "index.html")
			if serveIfExists(w, r, fs, tryIndex) {
				return
			}
		}
		if serveIfExists(w, r, fs, urlPath) {
			return
		}
		if !strings.Contains(filepath.Base(urlPath), ".") {
			if serveIfExists(w, r, fs, filepath.Join(urlPath, "index.html")) {
				return
			}
		}
		http.NotFound(w, r)
	})
}

func serveIfExists(w http.ResponseWriter, r *http.Request, fs http.FileSystem, name string) bool {
	f, err := fs.Open("/" + filepath.ToSlash(name))
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
	return true
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dashRun := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dashRun = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if !dashRun && b.Len() > 0 {
				b.WriteRune('-')
				dashRun = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
