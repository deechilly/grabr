package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/deechilly/grabr/internal/crawler"
	"github.com/deechilly/grabr/internal/store"
)

type Scheduler struct {
	Store   *store.Store
	Crawler *crawler.Crawler

	mu      sync.Mutex
	runners map[int64]*runner
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type runner struct {
	siteID int64
	kickCh chan struct{}
	cancel context.CancelFunc

	busyMu      sync.Mutex
	busy        bool
	crawlCancel context.CancelFunc // non-nil while a crawl is in flight
}

func New(st *store.Store, cr *crawler.Crawler) *Scheduler {
	return &Scheduler{Store: st, Crawler: cr, runners: map[int64]*runner{}}
}

// Start launches per-site tickers for every currently-enabled site. Crawls
// that were "running" at the time of the previous shutdown are marked failed.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Store.MarkRunningCrawlsAsFailed(ctx, "interrupted by restart"); err != nil {
		return err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	for _, site := range sites {
		if site.Enabled {
			s.startRunner(site)
		}
	}
	return nil
}

func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Add registers a new site with the scheduler. Called by the create handler.
func (s *Scheduler) Add(site *store.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runners[site.ID]; ok {
		return
	}
	if site.Enabled {
		s.startRunnerLocked(site)
	}
}

// Update reconciles the scheduler with a site whose settings may have changed.
// Disabled sites get their runner stopped; enabled ones get (re)started so the
// new interval takes effect.
func (s *Scheduler) Update(site *store.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopRunnerLocked(site.ID)
	if site.Enabled {
		s.startRunnerLocked(site)
	}
}

func (s *Scheduler) Remove(siteID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopRunnerLocked(siteID)
}

// Kick triggers an immediate crawl for siteID. If a crawl is already running,
// the kick is dropped with a log (matches scheduled-overlap behavior).
// Returns true if the kick was accepted (queued), false if no runner exists.
func (s *Scheduler) Kick(siteID int64) bool {
	s.mu.Lock()
	r, ok := s.runners[siteID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case r.kickCh <- struct{}{}:
	default:
		// Channel buffer full → a kick is already pending; coalesce.
	}
	return true
}

// IsRunning reports whether a crawl is currently in flight for siteID.
func (s *Scheduler) IsRunning(siteID int64) bool {
	s.mu.Lock()
	r, ok := s.runners[siteID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	r.busyMu.Lock()
	defer r.busyMu.Unlock()
	return r.busy
}

// Cancel interrupts the in-flight crawl for siteID. Returns true if a crawl
// was actually cancelled. Safe to call when no crawl is running.
func (s *Scheduler) Cancel(siteID int64) bool {
	s.mu.Lock()
	r, ok := s.runners[siteID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	r.busyMu.Lock()
	defer r.busyMu.Unlock()
	if r.crawlCancel != nil {
		r.crawlCancel()
		return true
	}
	return false
}

func (s *Scheduler) startRunner(site *store.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startRunnerLocked(site)
}

func (s *Scheduler) startRunnerLocked(site *store.Site) {
	if _, ok := s.runners[site.ID]; ok {
		return
	}
	rctx, cancel := context.WithCancel(s.ctx)
	r := &runner{
		siteID: site.ID,
		kickCh: make(chan struct{}, 1),
		cancel: cancel,
	}
	s.runners[site.ID] = r
	s.wg.Add(1)
	go s.runSite(rctx, r, site.IntervalSeconds)
}

func (s *Scheduler) stopRunnerLocked(siteID int64) {
	r, ok := s.runners[siteID]
	if !ok {
		return
	}
	r.cancel()
	delete(s.runners, siteID)
}

func (s *Scheduler) runSite(ctx context.Context, r *runner, intervalSeconds int) {
	defer s.wg.Done()
	interval := time.Duration(intervalSeconds) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tryCrawl(ctx, r, "scheduled")
		case <-r.kickCh:
			s.tryCrawl(ctx, r, "manual")
		}
	}
}

func (s *Scheduler) tryCrawl(ctx context.Context, r *runner, source string) {
	crawlCtx, cancel := context.WithCancel(ctx)
	r.busyMu.Lock()
	if r.busy {
		r.busyMu.Unlock()
		cancel()
		log.Printf("scheduler: site %d %s crawl skipped (already running)", r.siteID, source)
		return
	}
	r.busy = true
	r.crawlCancel = cancel
	r.busyMu.Unlock()
	defer func() {
		r.busyMu.Lock()
		r.busy = false
		r.crawlCancel = nil
		r.busyMu.Unlock()
		cancel()
	}()

	site, err := s.Store.GetSite(crawlCtx, r.siteID)
	if err != nil || site == nil {
		log.Printf("scheduler: site %d lookup failed: %v", r.siteID, err)
		return
	}
	// Re-check enabled at run time so a pause that lands between the kick and
	// here doesn't accidentally start a crawl.
	if !site.Enabled {
		log.Printf("scheduler: site %d (%s) %s crawl skipped (paused)", site.ID, site.Slug, source)
		return
	}
	log.Printf("scheduler: site %d (%s) %s crawl starting", site.ID, site.Slug, source)
	crawlID, status, err := s.Crawler.Run(crawlCtx, site)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("scheduler: site %d crawl %d ended status=%s err=%v", site.ID, crawlID, status, err)
	} else {
		log.Printf("scheduler: site %d crawl %d ended status=%s", site.ID, crawlID, status)
	}
}
