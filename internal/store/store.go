package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db *sql.DB
}

func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS sites (
		id              BIGSERIAL PRIMARY KEY,
		slug            TEXT NOT NULL UNIQUE,
		name            TEXT NOT NULL,
		seed_url        TEXT NOT NULL,
		host            TEXT NOT NULL,
		interval_seconds INTEGER NOT NULL,
		respect_robots  BOOLEAN NOT NULL DEFAULT TRUE,
		backup_keep_n   INTEGER NOT NULL DEFAULT 5,
		max_concurrent  INTEGER NOT NULL DEFAULT 1,
		rate_limit_rps  DOUBLE PRECISION NOT NULL DEFAULT 1.0,
		enabled         BOOLEAN NOT NULL DEFAULT TRUE,
		created_at      TIMESTAMPTZ NOT NULL,
		updated_at      TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS crawls (
		id               BIGSERIAL PRIMARY KEY,
		site_id          BIGINT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		started_at       TIMESTAMPTZ NOT NULL,
		finished_at      TIMESTAMPTZ,
		status           TEXT NOT NULL,
		pages_visited    INTEGER NOT NULL DEFAULT 0,
		pages_discovered INTEGER NOT NULL DEFAULT 0,
		bytes_downloaded BIGINT NOT NULL DEFAULT 0,
		error_message    TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_crawls_site ON crawls(site_id, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS visited_urls (
		id           BIGSERIAL PRIMARY KEY,
		crawl_id     BIGINT NOT NULL REFERENCES crawls(id) ON DELETE CASCADE,
		url          TEXT NOT NULL,
		status_code  INTEGER,
		content_type TEXT,
		bytes        BIGINT,
		fetched_at   TIMESTAMPTZ,
		error        TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_visited_crawl ON visited_urls(crawl_id)`,
	`CREATE TABLE IF NOT EXISTS robots_logs (
		id          BIGSERIAL PRIMARY KEY,
		site_id     BIGINT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		fetched_at  TIMESTAMPTZ NOT NULL,
		status_code INTEGER,
		content     TEXT,
		error       TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_robots_site ON robots_logs(site_id, fetched_at DESC)`,
	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

func (s *Store) migrate() error {
	for i, stmt := range migrations {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return nil
}

// --- Site ---

type Site struct {
	ID              int64
	Slug            string
	Name            string
	SeedURL         string
	Host            string
	IntervalSeconds int
	RespectRobots   bool
	BackupKeepN     int
	MaxConcurrent   int
	RateLimitRPS    float64
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (s *Store) CreateSite(ctx context.Context, site *Site) error {
	now := time.Now().UTC()
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO sites (slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		site.Slug, site.Name, site.SeedURL, site.Host, site.IntervalSeconds,
		site.RespectRobots, site.BackupKeepN, site.MaxConcurrent, site.RateLimitRPS,
		site.Enabled, now, now,
	).Scan(&site.ID)
	if err != nil {
		return err
	}
	site.CreatedAt = now
	site.UpdatedAt = now
	return nil
}

func (s *Store) UpdateSite(ctx context.Context, site *Site) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE sites SET name=$1, seed_url=$2, host=$3, interval_seconds=$4, respect_robots=$5,
		    backup_keep_n=$6, max_concurrent=$7, rate_limit_rps=$8, enabled=$9, updated_at=$10
		WHERE id=$11`,
		site.Name, site.SeedURL, site.Host, site.IntervalSeconds,
		site.RespectRobots, site.BackupKeepN, site.MaxConcurrent, site.RateLimitRPS,
		site.Enabled, now, site.ID,
	)
	return err
}

func (s *Store) DeleteSite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sites WHERE id=$1`, id)
	return err
}

func (s *Store) GetSite(ctx context.Context, id int64) (*Site, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n,
		    max_concurrent, rate_limit_rps, enabled, created_at, updated_at
		FROM sites WHERE id=$1`, id)
	return scanSite(row)
}

func (s *Store) GetSiteBySlug(ctx context.Context, slug string) (*Site, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n,
		    max_concurrent, rate_limit_rps, enabled, created_at, updated_at
		FROM sites WHERE slug=$1`, slug)
	return scanSite(row)
}

func (s *Store) ListSites(ctx context.Context) ([]*Site, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n,
		    max_concurrent, rate_limit_rps, enabled, created_at, updated_at
		FROM sites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSite(r scanner) (*Site, error) {
	var s Site
	err := r.Scan(&s.ID, &s.Slug, &s.Name, &s.SeedURL, &s.Host, &s.IntervalSeconds,
		&s.RespectRobots, &s.BackupKeepN, &s.MaxConcurrent, &s.RateLimitRPS,
		&s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// --- Settings ---

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}

func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// --- Crawl summaries ---

type CrawlSummary struct {
	ID              int64
	SiteID          int64
	StartedAt       time.Time
	FinishedAt      *time.Time
	Status          string
	PagesVisited    int
	PagesDiscovered int
	BytesDownloaded int64
	ErrorMessage    string
}

func (s *Store) LatestCrawlForSite(ctx context.Context, siteID int64) (*CrawlSummary, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, site_id, started_at, finished_at, status, pages_visited, pages_discovered,
		    bytes_downloaded, COALESCE(error_message,'')
		FROM crawls WHERE site_id=$1 ORDER BY started_at DESC LIMIT 1`, siteID)
	var c CrawlSummary
	err := row.Scan(&c.ID, &c.SiteID, &c.StartedAt, &c.FinishedAt, &c.Status,
		&c.PagesVisited, &c.PagesDiscovered, &c.BytesDownloaded, &c.ErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// --- Crawl lifecycle ---

func (s *Store) StartCrawl(ctx context.Context, siteID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO crawls (site_id, started_at, status, pages_visited, pages_discovered, bytes_downloaded)
		VALUES ($1, $2, 'running', 0, 0, 0) RETURNING id`, siteID, time.Now().UTC()).Scan(&id)
	return id, err
}

func (s *Store) BumpCrawlProgress(ctx context.Context, crawlID int64, deltaVisited, deltaDiscovered int, deltaBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE crawls
		SET pages_visited    = pages_visited    + $1,
		    pages_discovered = pages_discovered + $2,
		    bytes_downloaded = bytes_downloaded + $3
		WHERE id=$4`, deltaVisited, deltaDiscovered, deltaBytes, crawlID)
	return err
}

func (s *Store) FinishCrawl(ctx context.Context, crawlID int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE crawls SET finished_at=$1, status=$2, error_message=$3 WHERE id=$4`,
		time.Now().UTC(), status, errMsg, crawlID)
	return err
}

func (s *Store) MarkRunningCrawlsAsFailed(ctx context.Context, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE crawls SET finished_at=$1, status='failed', error_message=$2
		WHERE status='running'`, time.Now().UTC(), reason)
	return err
}

// RunningCrawl is a row from the crawls table whose status is 'running',
// joined with the site's slug so callers can correlate to k8s Jobs.
type RunningCrawl struct {
	CrawlID int64
	SiteID  int64
	Slug    string
}

func (s *Store) ListRunningCrawls(ctx context.Context) ([]RunningCrawl, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.site_id, s.slug
		FROM crawls c JOIN sites s ON s.id = c.site_id
		WHERE c.status='running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningCrawl
	for rows.Next() {
		var r RunningCrawl
		if err := rows.Scan(&r.CrawlID, &r.SiteID, &r.Slug); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AddRobotsLog(ctx context.Context, siteID int64, statusCode int, content, errMsg string) error {
	var statusVal, contentVal, errVal any
	if statusCode > 0 {
		statusVal = statusCode
	}
	if content != "" {
		contentVal = content
	}
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO robots_logs (site_id, fetched_at, status_code, content, error)
		VALUES ($1, $2, $3, $4, $5)`, siteID, time.Now().UTC(), statusVal, contentVal, errVal)
	return err
}

func (s *Store) AddVisitedURL(ctx context.Context, crawlID int64, urlStr string, status int, contentType string, bytes int64, errMsg string) error {
	var statusVal, bytesVal, ctVal, errVal any
	if status > 0 {
		statusVal = status
	}
	if bytes > 0 {
		bytesVal = bytes
	}
	if contentType != "" {
		ctVal = contentType
	}
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO visited_urls (crawl_id, url, status_code, content_type, bytes, fetched_at, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, crawlID, urlStr, statusVal, ctVal, bytesVal, time.Now().UTC(), errVal)
	return err
}
