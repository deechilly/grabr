package crawler

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ExtractLinks pulls all crawlable URLs out of an HTML document, resolved
// against base. Returns absolute URLs only. Caller filters by host/scope.
func ExtractLinks(base *url.URL, body []byte) []*url.URL {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var out []*url.URL
	seen := map[string]struct{}{}

	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Skip non-fetchable schemes and in-page anchors.
		if strings.HasPrefix(raw, "#") ||
			strings.HasPrefix(strings.ToLower(raw), "mailto:") ||
			strings.HasPrefix(strings.ToLower(raw), "tel:") ||
			strings.HasPrefix(strings.ToLower(raw), "javascript:") ||
			strings.HasPrefix(strings.ToLower(raw), "data:") {
			return
		}
		u, err := base.Parse(raw)
		if err != nil {
			return
		}
		u.Fragment = ""
		key := u.String()
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, u)
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a", "link":
				if v, ok := attr(n, "href"); ok {
					add(v)
				}
			case "img", "script", "iframe", "source", "audio", "video", "track", "embed":
				if v, ok := attr(n, "src"); ok {
					add(v)
				}
				if v, ok := attr(n, "srcset"); ok {
					for _, candidate := range parseSrcset(v) {
						add(candidate)
					}
				}
			case "form":
				if v, ok := attr(n, "action"); ok {
					add(v)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// parseSrcset extracts URLs from a srcset attribute (comma-separated
// "url descriptor" pairs).
func parseSrcset(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if sp := strings.IndexAny(part, " \t"); sp >= 0 {
			part = part[:sp]
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// LooksLikeHTML returns true if a content-type header indicates HTML.
func LooksLikeHTML(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml+xml")
}
