package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/deechilly/grabr/internal/config"
	"github.com/deechilly/grabr/internal/k8s"
	"github.com/deechilly/grabr/internal/store"
	"github.com/deechilly/grabr/internal/web"
)

// reconcileRunningCrawls inspects every crawl row stuck in 'running' state and
// closes out those that no longer have a backing k8s Job. When k8s integration
// is disabled, all running rows are treated as orphaned.
func reconcileRunningCrawls(ctx context.Context, st *store.Store, kube *k8s.Client) error {
	running, err := st.ListRunningCrawls(ctx)
	if err != nil {
		return err
	}
	if len(running) == 0 {
		return nil
	}
	if kube == nil {
		log.Printf("reconcile: k8s disabled, marking %d running crawl(s) as failed", len(running))
		return st.MarkRunningCrawlsAsFailed(ctx, "interrupted by restart")
	}
	for _, r := range running {
		active, err := kube.HasRunningJob(r.Slug)
		if err != nil {
			log.Printf("reconcile: HasRunningJob %s: %v", r.Slug, err)
			continue
		}
		if active {
			log.Printf("reconcile: crawl %d (%s) still has an active Job, leaving as-is", r.CrawlID, r.Slug)
			continue
		}
		if err := st.FinishCrawl(ctx, r.CrawlID, "failed", "interrupted by restart"); err != nil {
			log.Printf("reconcile: FinishCrawl %d: %v", r.CrawlID, err)
		}
	}
	return nil
}

func runServe(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Printf("grabr serve | data=%s addr=%s", cfg.DataDir, cfg.Addr)

	st, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	mirrorsDir, backupsDir := resolveDirs(st, cfg)
	log.Printf("dirs | mirrors=%s backups=%s", mirrorsDir, backupsDir)
	_ = backupsDir // backupsDir is used by crawler jobs, not the portal directly

	kube, err := k8s.New(cfg.Namespace, cfg.CrawlerImage)
	if err != nil {
		return err
	}
	if kube == nil {
		log.Printf("k8s integration disabled (no in-cluster credentials or GRABR_K8S_* env vars)")
	} else {
		log.Printf("k8s integration enabled | namespace=%s image=%s", cfg.Namespace, cfg.CrawlerImage)
	}

	// Reconcile crawls left in 'running' from a previous portal lifetime:
	// only mark a row failed if k8s has no active Job for its site. A real
	// crawler pod that is still running will write its own FinishCrawl when it
	// completes.
	if err := reconcileRunningCrawls(context.Background(), st, kube); err != nil {
		log.Printf("reconcile running crawls: %v", err)
	}

	srv, err := web.New(cfg, st, kube, mirrorsDir)
	if err != nil {
		return err
	}

	ctx, stop := newSignalContext()
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("http listening on %s", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	_ = os.Stdout.Sync()
	return nil
}

