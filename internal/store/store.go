package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite + WAL: single writer; keep concurrency simple for now
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sites (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		slug TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		seed_url TEXT NOT NULL,
		host TEXT NOT NULL,
		interval_seconds INTEGER NOT NULL,
		respect_robots INTEGER NOT NULL DEFAULT 1,
		backup_keep_n INTEGER NOT NULL DEFAULT 5,
		max_concurrent INTEGER NOT NULL DEFAULT 1,
		rate_limit_rps REAL NOT NULL DEFAULT 1.0,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS crawls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		started_at TEXT NOT NULL,
		finished_at TEXT,
		status TEXT NOT NULL,
		pages_visited INTEGER NOT NULL DEFAULT 0,
		pages_discovered INTEGER NOT NULL DEFAULT 0,
		bytes_downloaded INTEGER NOT NULL DEFAULT 0,
		error_message TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_crawls_site ON crawls(site_id, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS visited_urls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		crawl_id INTEGER NOT NULL REFERENCES crawls(id) ON DELETE CASCADE,
		url TEXT NOT NULL,
		status_code INTEGER,
		content_type TEXT,
		bytes INTEGER,
		fetched_at TEXT,
		error TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_visited_crawl ON visited_urls(crawl_id)`,
	`CREATE TABLE IF NOT EXISTS robots_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		fetched_at TEXT NOT NULL,
		status_code INTEGER,
		content TEXT,
		error TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_robots_site ON robots_logs(site_id, fetched_at DESC)`,
	`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
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
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sites (slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site.Slug, site.Name, site.SeedURL, site.Host, site.IntervalSeconds,
		boolToInt(site.RespectRobots), site.BackupKeepN, site.MaxConcurrent, site.RateLimitRPS,
		boolToInt(site.Enabled), now, now,
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	site.ID = id
	return nil
}

func (s *Store) UpdateSite(ctx context.Context, site *Site) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE sites SET name=?, seed_url=?, host=?, interval_seconds=?, respect_robots=?, backup_keep_n=?, max_concurrent=?, rate_limit_rps=?, enabled=?, updated_at=?
		WHERE id=?`,
		site.Name, site.SeedURL, site.Host, site.IntervalSeconds,
		boolToInt(site.RespectRobots), site.BackupKeepN, site.MaxConcurrent, site.RateLimitRPS,
		boolToInt(site.Enabled), now, site.ID,
	)
	return err
}

func (s *Store) DeleteSite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sites WHERE id=?`, id)
	return err
}

func (s *Store) GetSite(ctx context.Context, id int64) (*Site, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at FROM sites WHERE id=?`, id)
	return scanSite(row)
}

func (s *Store) GetSiteBySlug(ctx context.Context, slug string) (*Site, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at FROM sites WHERE slug=?`, slug)
	return scanSite(row)
}

func (s *Store) ListSites(ctx context.Context) ([]*Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots, backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at FROM sites ORDER BY name`)
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
	var respectRobots, enabled int
	var createdAt, updatedAt string
	err := r.Scan(&s.ID, &s.Slug, &s.Name, &s.SeedURL, &s.Host, &s.IntervalSeconds, &respectRobots, &s.BackupKeepN, &s.MaxConcurrent, &s.RateLimitRPS, &enabled, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.RespectRobots = respectRobots != 0
	s.Enabled = enabled != 0
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &s, nil
}

// --- Settings ---

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
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
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
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

// --- Crawl summaries (for landing page) ---

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
		SELECT id, site_id, started_at, finished_at, status, pages_visited, pages_discovered, bytes_downloaded, COALESCE(error_message,'')
		FROM crawls WHERE site_id=? ORDER BY started_at DESC LIMIT 1`, siteID)
	var c CrawlSummary
	var started string
	var finished sql.NullString
	err := row.Scan(&c.ID, &c.SiteID, &started, &finished, &c.Status, &c.PagesVisited, &c.PagesDiscovered, &c.BytesDownloaded, &c.ErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.StartedAt, _ = time.Parse(time.RFC3339, started)
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339, finished.String)
		c.FinishedAt = &t
	}
	return &c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
