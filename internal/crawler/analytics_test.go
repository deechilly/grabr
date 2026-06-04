package crawler

import (
	"net/url"
	"strings"
	"testing"
)

func TestStripAnalytics(t *testing.T) {
	html := `<!doctype html><html><head>
<link rel="preconnect" href="https://www.googletagmanager.com">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="dns-prefetch" href="//connect.facebook.net">
<script async src="https://www.googletagmanager.com/gtag/js?id=GA-X"></script>
<script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments);} gtag('config','GA-X');</script>
<script src="https://cdn.example.com/legit.js"></script>
<script>console.log("not a tracker");</script>
<script src="https://static.hotjar.com/c/hotjar-1234.js?sv=6"></script>
</head><body>
<h1>Hello</h1>
<img src="https://stats.g.doubleclick.net/dc.gif">
<img src="/local-image.png">
<iframe src="https://www.googletagmanager.com/ns.html"></iframe>
<iframe src="https://www.youtube.com/embed/abc"></iframe>
<noscript><img src="https://www.google-analytics.com/collect?v=1"></noscript>
<noscript>real content here</noscript>
<script>!function(f,b,e,v,n,t,s){fbq('init','12345');}</script>
<script>mixpanel.init("KEY");</script>
</body></html>`
	base, _ := url.Parse("https://example.com/")
	out := RewriteHTML(base, []byte(html), "index.html", "example.com")
	s := string(out)

	// These must be gone:
	shouldStrip := []string{
		"googletagmanager.com",
		"hotjar.com",
		"doubleclick.net",
		"connect.facebook.net",
		"google-analytics.com",
		"gtag('config'",
		"fbq('init'",
		"mixpanel.init",
	}
	for _, want := range shouldStrip {
		if strings.Contains(s, want) {
			t.Errorf("expected analytics token %q to be stripped, still present:\n%s", want, s)
		}
	}

	// These must survive:
	shouldKeep := []string{
		"fonts.googleapis.com", // not on analytics list
		"cdn.example.com/legit.js",
		"console.log",
		"local-image.png",
		"youtube.com/embed",
		"real content here", // noscript without analytics children
		"Hello",
	}
	for _, want := range shouldKeep {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q to survive, but it was removed:\n%s", want, s)
		}
	}
}
