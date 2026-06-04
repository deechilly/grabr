package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deechilly/grabr/internal/backup"
	"github.com/deechilly/grabr/internal/config"
	"github.com/deechilly/grabr/internal/crawler"
	"github.com/deechilly/grabr/internal/scheduler"
	"github.com/deechilly/grabr/internal/store"
	"github.com/deechilly/grabr/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("grabr: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Printf("grabr starting | data=%s addr=%s", cfg.DataDir, cfg.Addr)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Effective dirs: settings table overrides env defaults at boot.
	// Changes via the admin UI require a restart to take effect; the UI says so.
	mirrorsDir, backupsDir := resolveDirs(st, cfg)
	log.Printf("dirs | mirrors=%s backups=%s", mirrorsDir, backupsDir)

	backuper := backup.New(mirrorsDir, backupsDir, func(slug string) int {
		site, err := st.GetSiteBySlug(context.Background(), slug)
		if err != nil || site == nil {
			return cfg.DefaultBackupKeepN
		}
		return site.BackupKeepN
	})

	cr := &crawler.Crawler{Store: st, MirrorsDir: mirrorsDir, Backuper: backuper}
	sched := scheduler.New(st, cr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := sched.Start(ctx); err != nil {
		return err
	}

	srv, err := web.New(cfg, st, sched, mirrorsDir)
	if err != nil {
		return err
	}

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
		sched.Stop()
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	sched.Stop()
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
