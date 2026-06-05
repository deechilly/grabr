package main

import (
	"context"
	"os"

	"github.com/deechilly/grabr/internal/config"
	"github.com/deechilly/grabr/internal/store"
)

// resolveDirs returns the effective mirrors and backups directories.
// Settings stored in the DB (set via the admin UI) override the env defaults.
// Both `serve` and `crawl` call this so a customized path applies uniformly to
// the portal and to crawler pods.
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
