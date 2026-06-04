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
	// Strip analytics and tracking elements before rewriting URLs. They're
	// cross-host so URL rewriting wouldn't touch them anyway, but removing
	// them prevents the served mirror (and the offline tar.gz) from phoning
	// home to GA, GTM, Hotjar, Sentry, and friends.
	stripAnalytics(doc, base)
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

	// markCrossHostAnchor adds target="_blank" + rel="noopener noreferrer" to
	// any <a> whose href is a cross-host absolute URL. Inside the embedded
	// viewer this breaks the link out of the iframe into a new browser tab,
	// keeping the iframe on mirrored content.
	markCrossHostAnchor := func(n *html.Node) {
		href := attrValue(n, "href")
		if href == "" {
			return
		}
		raw := strings.TrimSpace(href)
		if raw == "" || strings.HasPrefix(raw, "#") {
			return
		}
		lower := strings.ToLower(raw)
		for _, p := range []string{"mailto:", "tel:", "javascript:", "data:"} {
			if strings.HasPrefix(lower, p) {
				return
			}
		}
		u, err := base.Parse(raw)
		if err != nil || u.Host == "" || strings.EqualFold(u.Host, host) {
			return
		}
		setAttr(n, "target", "_blank")
		setAttr(n, "rel", "noopener noreferrer")
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
			if n.Data == "a" {
				markCrossHostAnchor(n)
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

// setAttr sets (or overwrites) the named attribute on n.
func setAttr(n *html.Node, key, value string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: value})
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
