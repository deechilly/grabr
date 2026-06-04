package crawler

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// LocalPathFor maps a URL to a relative on-disk path (using OS separators)
// under the site's mirror root. Directory-like URLs become <path>/index.html.
//
// Query strings are encoded into the filename so distinct query strings don't
// collide. (?a=1 becomes the directory __q__/a=1.html appended to the path.)
func LocalPathFor(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	// Treat trailing slash as directory.
	if strings.HasSuffix(p, "/") {
		p += "index.html"
	}
	// If the leaf has no extension at all, assume HTML and append /index.html
	// so it can coexist with siblings (e.g. /foo and /foo/bar both want to
	// write under /foo). This loses some fidelity but avoids collisions.
	if !strings.HasSuffix(p, "/") {
		base := filepath.Base(p)
		if !strings.Contains(base, ".") {
			p += "/index.html"
		}
	}
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+p)), "/")
	if u.RawQuery != "" {
		dir := filepath.Dir(clean)
		base := filepath.Base(clean)
		safeQuery := strings.NewReplacer("/", "_", "\\", "_", "?", "_", "&", "_", "=", "-").Replace(u.RawQuery)
		clean = filepath.Join(dir, base+"__q__"+safeQuery)
	}
	return filepath.FromSlash(clean)
}

// WriteFile writes content to <root>/<relPath>, creating parent dirs as
// needed. Atomic-ish: writes to a sibling .tmp file then renames.
func WriteFile(root, relPath string, data []byte) error {
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}
