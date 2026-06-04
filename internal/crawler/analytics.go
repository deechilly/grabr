package crawler

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// analyticsHosts is a suffix-match list of analytics, ad, and session-replay
// endpoints. A host matches if it equals the suffix or ends in "."+suffix.
// Includes the common heavyweights (Google, Facebook, segment, Hotjar,
// FullStory, Sentry, Microsoft Clarity, Yandex Metrica, etc.) plus assorted
// ad/tag networks. Conservative: only obvious trackers, not "could be" hosts.
var analyticsHosts = []string{
	// Google analytics + ads + tag manager
	"google-analytics.com",
	"googletagmanager.com",
	"googleadservices.com",
	"googlesyndication.com",
	"doubleclick.net",
	// Facebook / Meta
	"connect.facebook.net",
	"facebook.net",
	// Mainstream product analytics
	"segment.com",
	"segment.io",
	"mixpanel.com",
	"mxpnl.com",
	"heapanalytics.com",
	"heap.io",
	"amplitude.com",
	// Session replay / heatmaps
	"hotjar.com",
	"hotjar.io",
	"fullstory.com",
	"clarity.ms",
	"crazyegg.com",
	// Browser error reporting (treated as tracking for mirror purposes)
	"bugsnag.com",
	"nr-data.net",
	"newrelic.com",
	"sentry-cdn.com",
	"sentry.io",
	// Misc
	"snowplowanalytics.com",
	"intercom.io",
	"intercomcdn.com",
	"pdst.fm",
	"optimizely.com",
	"mc.yandex.ru",
	"stats.wp.com",
	"matomo.cloud",
}

// analyticsInlinePatterns identifies inline <script> bodies that are clearly
// initializing/calling a tracker. Patterns target the function call sites so
// we don't accidentally remove a script that merely mentions the word.
var analyticsInlinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgtag\s*\(`),
	regexp.MustCompile(`\bga\s*\(\s*['"](create|send|set|require)['"]`),
	regexp.MustCompile(`_gaq\s*\.\s*push`),
	regexp.MustCompile(`\bfbq\s*\(`),
	regexp.MustCompile(`mixpanel\s*\.\s*(init|track|identify)`),
	regexp.MustCompile(`heap\s*\.\s*(load|track|identify)`),
	regexp.MustCompile(`amplitude\s*\.\s*(getInstance|init)`),
	regexp.MustCompile(`\bhj\s*\(`),
	regexp.MustCompile(`\bclarity\s*\(`),
	regexp.MustCompile(`\bFS\s*\.\s*(identify|setUserVars|event)`),
	regexp.MustCompile(`_hsq\s*\.\s*push|__hsq\s*\.\s*push`),
	regexp.MustCompile(`segment\.com/analytics\.js`),
	regexp.MustCompile(`googletagmanager\.com/(gtm|gtag)`),
	regexp.MustCompile(`google-analytics\.com/analytics\.js`),
}

func isAnalyticsHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, suffix := range analyticsHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// hostFromAttr resolves an attribute URL against base (so a relative
// "/x.js" doesn't accidentally match) and returns its host.
func hostFromAttr(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if base == nil {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	}
	u, err := base.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// stripAnalytics walks the parsed document and removes analytics/tracking
// elements in place. Returns the count of removed nodes.
//
// Removed:
//   - <script src> pointing at analyticsHosts
//   - <script> (inline) whose body matches analyticsInlinePatterns
//   - <iframe src> pointing at analyticsHosts
//   - <img src> pointing at analyticsHosts (1x1 pixel trackers)
//   - <link rel="preconnect|dns-prefetch|preload"> to analyticsHosts
//   - <noscript> whose only meaningful child is one of the above
//
// Cross-host non-analytics resources are NOT touched.
func stripAnalytics(doc *html.Node, base *url.URL) int {
	var stripped int
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		c := n.FirstChild
		for c != nil {
			next := c.NextSibling
			if shouldStripAnalytics(c, base) {
				n.RemoveChild(c)
				stripped++
			} else {
				walk(c)
			}
			c = next
		}
	}
	walk(doc)
	return stripped
}

func shouldStripAnalytics(n *html.Node, base *url.URL) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "script":
		if src := attrValue(n, "src"); src != "" {
			return isAnalyticsHost(hostFromAttr(base, src))
		}
		body := textContent(n)
		if body == "" {
			return false
		}
		for _, pat := range analyticsInlinePatterns {
			if pat.MatchString(body) {
				return true
			}
		}
	case "iframe", "img":
		if src := attrValue(n, "src"); src != "" {
			return isAnalyticsHost(hostFromAttr(base, src))
		}
	case "link":
		rel := strings.ToLower(attrValue(n, "rel"))
		if rel == "preconnect" || rel == "dns-prefetch" || rel == "preload" || rel == "prefetch" {
			if href := attrValue(n, "href"); href != "" {
				return isAnalyticsHost(hostFromAttr(base, href))
			}
		}
	case "noscript":
		// golang.org/x/net/html parses <noscript> contents as raw text by
		// default (scripting-enabled mode), so we can't walk it as elements.
		// Instead, scan the literal text for analytics host substrings —
		// the common case is a GA fallback pixel like
		//   <noscript><img src="https://www.google-analytics.com/collect"></noscript>
		txt := strings.ToLower(textContent(n))
		if txt == "" {
			return false
		}
		for _, suffix := range analyticsHosts {
			if strings.Contains(txt, suffix) {
				return true
			}
		}
	}
	return false
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var buf bytes.Buffer
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			buf.WriteString(c.Data)
		}
	}
	return buf.String()
}
