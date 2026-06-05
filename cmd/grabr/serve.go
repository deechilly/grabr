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

	// On startup, mark any interrupted crawls as failed.
	if err := st.MarkRunningCrawlsAsFailed(context.Background(), "interrupted by restart"); err != nil {
		return err
	}

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

func resolveDirs(st *store.Store, cfg *config.Config) (string, string) {
	mirrors, backups := cfg.MirrorsDir, cfg.BackupsDir
	if v, ok, _ := st.GetSetting(context.Background(), "mirrors_dir"); ok && v != "" {
		mirrors = v
	}
	if v, ok, _ := st.GetSetting(context.Background(), "backups_dir"); ok && v != "" {
		backups = v
	}
	_ = os.MkdirAll(mirrors, 0o755)
	_ = os.MkdirAll(backups, 0o755)
	return mirrors, backups
}
