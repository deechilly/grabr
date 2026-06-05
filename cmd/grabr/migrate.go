package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	sqlitePath := fs.String("sqlite", "", "path to grabr.db SQLite file (required)")
	postgresDSN := fs.String("postgres", "", "Postgres DSN, e.g. postgres://user:pass@host/dbname (required)")
	includeVisited := fs.Bool("include-visited", false, "also migrate visited_urls (can be large)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sqlitePath == "" || *postgresDSN == "" {
		fs.Usage()
		return errors.New("--sqlite and --postgres are required")
	}

	src, err := sql.Open("sqlite", *sqlitePath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer src.Close()

	dst, err := sql.Open("pgx", *postgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer dst.Close()

	if err := applyPGSchema(dst); err != nil {
		return fmt.Errorf("apply pg schema: %w", err)
	}

	// Warn if destination already has data.
	var siteCount int
	if err := dst.QueryRow(`SELECT COUNT(*) FROM sites`).Scan(&siteCount); err == nil && siteCount > 0 {
		log.Printf("WARNING: destination already has %d site(s); rows will be skipped on conflict", siteCount)
	}

	tx, err := dst.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	counts := map[string]int{}

	if n, err := migrateSettings(src, tx); err != nil {
		return fmt.Errorf("settings: %w", err)
	} else {
		counts["settings"] = n
	}

	if n, err := migrateSites(src, tx); err != nil {
		return fmt.Errorf("sites: %w", err)
	} else {
		counts["sites"] = n
	}

	if n, err := migrateCrawls(src, tx); err != nil {
		return fmt.Errorf("crawls: %w", err)
	} else {
		counts["crawls"] = n
	}

	if n, err := migrateRobotsLogs(src, tx); err != nil {
		return fmt.Errorf("robots_logs: %w", err)
	} else {
		counts["robots_logs"] = n
	}

	if *includeVisited {
		if n, err := migrateVisitedURLs(src, tx); err != nil {
			return fmt.Errorf("visited_urls: %w", err)
		} else {
			counts["visited_urls"] = n
		}
	}

	if err := resetSequences(tx); err != nil {
		return fmt.Errorf("reset sequences: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Printf("migration complete:")
	for _, t := range []string{"settings", "sites", "crawls", "robots_logs", "visited_urls"} {
		if n, ok := counts[t]; ok {
			log.Printf("  %-20s %d rows", t, n)
		}
	}
	if !*includeVisited {
		log.Printf("  visited_urls         skipped (use --include-visited to migrate)")
	}
	return nil
}

func migrateSettings(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return n, err
		}
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, k, v); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func migrateSites(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`
		SELECT id, slug, name, seed_url, host, interval_seconds, respect_robots,
		    backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at
		FROM sites ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			id, interval, keepN, maxConc   int64
			slug, name, seed, host         string
			robots, enabled                int
			rps                            float64
			createdStr, updatedStr         string
		)
		if err := rows.Scan(&id, &slug, &name, &seed, &host, &interval, &robots,
			&keepN, &maxConc, &rps, &enabled, &createdStr, &updatedStr); err != nil {
			return n, err
		}
		created := parseTime(createdStr)
		updated := parseTime(updatedStr)
		_, err := tx.Exec(`
			INSERT INTO sites (id, slug, name, seed_url, host, interval_seconds, respect_robots,
			    backup_keep_n, max_concurrent, rate_limit_rps, enabled, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (id) DO NOTHING`,
			id, slug, name, seed, host, interval, robots != 0,
			keepN, maxConc, rps, enabled != 0, created, updated,
		)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func migrateCrawls(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`
		SELECT id, site_id, started_at, finished_at, status,
		    pages_visited, pages_discovered, bytes_downloaded, error_message
		FROM crawls ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	now := time.Now().UTC()
	for rows.Next() {
		var (
			id, siteID                                int64
			startedStr                                string
			finishedStr                               sql.NullString
			status                                    string
			pagesVisited, pagesDiscovered             int
			bytesDownloaded                           int64
			errorMessage                              sql.NullString
		)
		if err := rows.Scan(&id, &siteID, &startedStr, &finishedStr, &status,
			&pagesVisited, &pagesDiscovered, &bytesDownloaded, &errorMessage); err != nil {
			return n, err
		}

		started := parseTime(startedStr)
		var finished *time.Time
		if finishedStr.Valid {
			t := parseTime(finishedStr.String)
			finished = &t
		}

		// Mark interrupted running crawls as failed.
		if status == "running" {
			status = "failed"
			t := now
			finished = &t
			if !errorMessage.Valid || errorMessage.String == "" {
				errorMessage = sql.NullString{String: "interrupted by migration", Valid: true}
			}
		}

		_, err := tx.Exec(`
			INSERT INTO crawls (id, site_id, started_at, finished_at, status,
			    pages_visited, pages_discovered, bytes_downloaded, error_message)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (id) DO NOTHING`,
			id, siteID, started, finished, status,
			pagesVisited, pagesDiscovered, bytesDownloaded, nullStr(errorMessage),
		)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func migrateRobotsLogs(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`
		SELECT id, site_id, fetched_at, status_code, content, error
		FROM robots_logs ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			id, siteID  int64
			fetchedStr  string
			statusCode  sql.NullInt64
			content     sql.NullString
			errMsg      sql.NullString
		)
		if err := rows.Scan(&id, &siteID, &fetchedStr, &statusCode, &content, &errMsg); err != nil {
			return n, err
		}
		fetched := parseTime(fetchedStr)
		_, err := tx.Exec(`
			INSERT INTO robots_logs (id, site_id, fetched_at, status_code, content, error)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (id) DO NOTHING`,
			id, siteID, fetched, nullInt(statusCode), nullStr(content), nullStr(errMsg),
		)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func migrateVisitedURLs(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`
		SELECT id, crawl_id, url, status_code, content_type, bytes, fetched_at, error
		FROM visited_urls ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			id, crawlID int64
			urlStr      string
			statusCode  sql.NullInt64
			contentType sql.NullString
			bytes       sql.NullInt64
			fetchedStr  sql.NullString
			errMsg      sql.NullString
		)
		if err := rows.Scan(&id, &crawlID, &urlStr, &statusCode, &contentType, &bytes, &fetchedStr, &errMsg); err != nil {
			return n, err
		}
		var fetched *time.Time
		if fetchedStr.Valid {
			t := parseTime(fetchedStr.String)
			fetched = &t
		}
		_, err := tx.Exec(`
			INSERT INTO visited_urls (id, crawl_id, url, status_code, content_type, bytes, fetched_at, error)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (id) DO NOTHING`,
			id, crawlID, urlStr, nullInt(statusCode), nullStr(contentType), nullInt(bytes), fetched, nullStr(errMsg),
		)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func resetSequences(tx *sql.Tx) error {
	seqs := []struct{ table, col string }{
		{"sites", "id"},
		{"crawls", "id"},
		{"robots_logs", "id"},
		{"visited_urls", "id"},
	}
	for _, s := range seqs {
		q := fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 1))`,
			s.table, s.col, s.col, s.table,
		)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("reset seq %s.%s: %w", s.table, s.col, err)
		}
	}
	return nil
}

func applyPGSchema(db *sql.DB) error {
	stmts := []string{
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
	for i, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("stmt %d: %w", i, err)
		}
	}
	return nil
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func nullStr(n sql.NullString) any {
	if n.Valid {
		return n.String
	}
	return nil
}

func nullInt(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}
