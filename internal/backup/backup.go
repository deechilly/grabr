package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backuper snapshots a site's mirror directory into a tar.gz under BackupsDir
// and prunes old snapshots beyond KeepN per site.
type Backuper struct {
	MirrorsDir string
	BackupsDir string
	KeepNFn    func(slug string) int // per-site retention; required.
}

func New(mirrorsDir, backupsDir string, keepN func(slug string) int) *Backuper {
	return &Backuper{MirrorsDir: mirrorsDir, BackupsDir: backupsDir, KeepNFn: keepN}
}

// Snapshot writes <BackupsDir>/<slug>/<utc-stamp>.tar.gz containing the current
// contents of <MirrorsDir>/<slug>. If the mirror dir is missing or empty, it
// skips creating the archive (nothing to back up on first-ever crawl).
// After writing, prunes older snapshots beyond the per-site retention.
func (b *Backuper) Snapshot(ctx context.Context, slug string) error {
	srcDir := filepath.Join(b.MirrorsDir, slug)
	info, err := os.Stat(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("mirror path %s is not a directory", srcDir)
	}
	empty, err := dirIsEmpty(srcDir)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	outDir := filepath.Join(b.BackupsDir, slug)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	outPath := filepath.Join(outDir, stamp+".tar.gz")
	tmp := outPath + ".tmp"

	if err := writeTarGz(ctx, tmp, srcDir, slug); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return err
	}

	keep := 5
	if b.KeepNFn != nil {
		if n := b.KeepNFn(slug); n > 0 {
			keep = n
		}
	}
	return prune(outDir, keep)
}

// List returns existing snapshot filenames for a slug, newest first.
func (b *Backuper) List(slug string) ([]string, error) {
	outDir := filepath.Join(b.BackupsDir, slug)
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(names) == 0, nil
}

func writeTarGz(ctx context.Context, dst, srcDir, archiveRoot string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		// Tar paths use forward slashes; root entry is the slug directory.
		var name string
		if rel == "." {
			name = archiveRoot
		} else {
			name = archiveRoot + "/" + filepath.ToSlash(rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			fh, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, fh)
			fh.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
}

func prune(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[keep:] {
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}
