package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr         string
	DataDir      string
	DBPath       string
	MirrorsDir   string
	BackupsDir   string
	AdminUser    string
	AdminPass    string
	DefaultIntervalSeconds int
	DefaultBackupKeepN     int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:                   ":" + getenv("GRABR_PORT", "8080"),
		DataDir:                getenv("GRABR_DATA_DIR", "./data"),
		AdminUser:              os.Getenv("GRABR_ADMIN_USER"),
		AdminPass:              os.Getenv("GRABR_ADMIN_PASS"),
		DefaultIntervalSeconds: getenvInt("GRABR_DEFAULT_INTERVAL_SECONDS", 24*3600),
		DefaultBackupKeepN:     getenvInt("GRABR_DEFAULT_BACKUP_KEEP_N", 5),
	}
	if strings.TrimSpace(c.AdminUser) == "" || strings.TrimSpace(c.AdminPass) == "" {
		return nil, errors.New("GRABR_ADMIN_USER and GRABR_ADMIN_PASS must be set")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	c.DBPath = filepath.Join(abs, "grabr.db")
	c.MirrorsDir = filepath.Join(abs, "mirrors")
	c.BackupsDir = filepath.Join(abs, "backups")
	for _, d := range []string{c.DataDir, c.MirrorsDir, c.BackupsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
