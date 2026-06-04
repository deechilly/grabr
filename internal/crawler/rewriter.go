package crawler

import (
	"bytes"
	"net/url"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"
)

// urlAttrs lists which (tag, attr) combinations carry URLs we should rewrite.
// srcset is handled specially because it's a comma-separated list.
var urlAttrs = map[string][]string{
	"a":      {"href"},
	"link":   {"href"},
	"area":   {"href"},
	"img":    {"src"},
	"script": {"src"},
	"iframe": {"src"},
	"source": {"src"},
	"audio":  {"src"},
	"video":  {"src", "poster"},
	"track":  {"src"},
	"embed":  {"src"},
	"object": {"data"},
	"form":   {"action"},
}

// RewriteHTML rewrites same-host URLs in HTML to point at their local mirror
// path, expressed relative to the location of `currentRelPath` (the on-disk
// relative path of the document being rewritten). This makes the resulting
// HTML render correctly both when served from grabr and when extracted from
// the tar.gz to disk and opened directly.
//
// Cross-host URLs and non-fetchable schemes (mailto:, tel:, javascript:, etc)
// are left untouched.
func RewriteHTML(base *url.URL, body []byte, currentRelPath, host string) []byte {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return body
	}
	host = strings.ToLower(host)
	currentDir := filepath.ToSlash(filepath.Dir(currentRelPath))
	if currentDir == "." {
		currentDir = ""
	}

	rewrite := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return raw
		}
		// Leave alone: anchors, non-fetchable schemes.
		if strings.HasPrefix(raw, "#") {
			return raw
		}
		lower := strings.ToLower(raw)
		for _, p := range []string{"mailto:", "tel:", "javascript:", "data:"} {
			if strings.HasPrefix(lower, p) {
				return raw
			}
		}
		u, err := base.Parse(raw)
		if err != nil {
			return raw
		}
		if !strings.EqualFold(u.Host, host) {
			return raw
		}
		// Compute local relative path of the target on disk.
		targetRel := filepath.ToSlash(LocalPathFor(u))
		rel := relPath(currentDir, targetRel)
		if u.Fragment != "" {
			rel += "#" + u.Fragment
		}
		return rel
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if names, ok := urlAttrs[n.Data]; ok {
				for i, a := range n.Attr {
					for _, name := range names {
						if a.Key == name {
							n.Attr[i].Val = rewrite(a.Val)
						}
					}
				}
			}
			// srcset on <img>, <source>, <link rel="preload" imagesrcset=...>
			for i, a := range n.Attr {
				if a.Key == "srcset" || a.Key == "imagesrcset" {
					n.Attr[i].Val = rewriteSrcset(a.Val, rewrite)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return body
	}
	return buf.Bytes()
}

// rewriteSrcset rewrites each URL in a srcset attribute while preserving
// descriptors.
func rewriteSrcset(v string, rewrite func(string) string) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		urlPart, rest := p, ""
		if sp := strings.IndexAny(p, " \t"); sp >= 0 {
			urlPart = p[:sp]
			rest = p[sp:]
		}
		urlPart = rewrite(urlPart)
		parts[i] = urlPart + rest
	}
	return strings.Join(parts, ", ")
}

// relPath returns target expressed relative to fromDir, where both paths use
// forward slashes and target is rooted at the mirror root. If fromDir == "",
// target is returned as-is.
func relPath(fromDir, target string) string {
	if fromDir == "" {
		return target
	}
	rel, err := filepath.Rel(filepath.FromSlash(fromDir), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}
