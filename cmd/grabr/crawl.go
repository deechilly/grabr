package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"

	"github.com/deechilly/grabr/internal/backup"
	"github.com/deechilly/grabr/internal/config"
	"github.com/deechilly/grabr/internal/crawler"
	"github.com/deechilly/grabr/internal/store"
)

func runCrawl(args []string) error {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	siteID := fs.Int64("site-id", 0, "site ID to crawl (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *siteID == 0 {
		fs.Usage()
		return errors.New("--site-id is required")
	}

	cfg, err := config.LoadCrawl()
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := newSignalContext()
	defer stop()

	site, err := st.GetSite(ctx, *siteID)
	if err != nil {
		return err
	}
	if site == nil {
		return errors.New("site not found")
	}

	backuper := backup.New(cfg.MirrorsDir, cfg.BackupsDir, func(slug string) int {
		s, err := st.GetSiteBySlug(context.Background(), slug)
		if err != nil || s == nil {
			return 5
		}
		return s.BackupKeepN
	})

	cr := &crawler.Crawler{Store: st, MirrorsDir: cfg.MirrorsDir, Backuper: backuper}

	log.Printf("crawl: starting site %d (%s)", site.ID, site.Slug)
	_, status, err := cr.Run(ctx, site)
	log.Printf("crawl: finished site %d status=%s err=%v", site.ID, status, err)

	if err != nil && !errors.Is(err, context.Canceled) {
		_ = os.Stderr.Sync()
		os.Exit(1)
	}
	return nil
}
