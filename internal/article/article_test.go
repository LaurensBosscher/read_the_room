package article

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
	"github.com/LaurensBosscher/read_the_room/internal/scriptlets"

	"github.com/PuerkitoBio/goquery"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url %q: %v", s, err)
	}
	return u
}

func mustPrepare(t *testing.T, raw string, rule rules.Rule) *goquery.Document {
	t.Helper()
	node, err := Prepare(raw, rule)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if node == nil {
		t.Fatal("Prepare returned nil node")
	}
	return goquery.NewDocumentFromNode(node)
}

func TestPrepareNoscript(t *testing.T) {
	raw := `<!doctype html><html><head><noscript><meta name="x" content="y"></noscript><title>T</title></head>` +
		`<body><noscript><img src="/pic.jpg"></noscript><p>hello</p></body></html>`
	doc := mustPrepare(t, raw, rules.Rule{})

	if n := doc.Find("head noscript").Length(); n != 0 {
		t.Errorf("head noscript: got %d, want 0", n)
	}
	if n := doc.Find("body noscript").Length(); n != 0 {
		t.Errorf("body noscript: got %d, want 0", n)
	}
	if n := doc.Find("body img").Length(); n != 1 {
		t.Fatalf("body img: got %d, want 1", n)
	}
	if src := doc.Find("body img").AttrOr("src", ""); src != "/pic.jpg" {
		t.Errorf("img src = %q, want /pic.jpg", src)
	}
}

func TestPrepareRemoveAndUnhide(t *testing.T) {
	raw := `<html><head><title>T</title></head><body>` +
		`<div class="paywall-modal">subscribe</div>` +
		`<div class="locked" style="display:none" hidden aria-hidden="true">visible</div>` +
		`</body></html>`
	rule := rules.Rule{
		RemoveSelectors: []string{".paywall-modal"},
		UnhideSelectors: []string{".locked"},
	}
	doc := mustPrepare(t, raw, rule)

	if n := doc.Find(".paywall-modal").Length(); n != 0 {
		t.Errorf(".paywall-modal: got %d, want 0", n)
	}
	locked := doc.Find(".locked")
	if locked.Length() != 1 {
		t.Fatalf(".locked: got %d, want 1", locked.Length())
	}
	for _, attr := range []string{"style", "hidden", "aria-hidden"} {
		if _, ok := locked.Attr(attr); ok {
			t.Errorf("attribute %q still present on .locked", attr)
		}
	}
	if locked.Text() != "visible" {
		t.Errorf(".locked text = %q, want visible", locked.Text())
	}
}

const originalRawKeepScriptsFalse = `<!doctype html><html><head>` +
	`<title>Fixture</title>` +
	`<script src="/app.js"></script>` +
	`<script type="application/ld+json">{"@type":"Article"}</script>` +
	`<link rel="preload" as="script" href="/pre.js">` +
	`<link rel="modulepreload" href="/mod.js">` +
	`<link rel="manifest" href="/manifest.json">` +
	`<base href="/oldbase/">` +
	`<meta http-equiv="Refresh" content="0;url=/next">` +
	`<meta http-equiv="content-security-policy" content="script-src 'self'">` +
	`<meta name="x" content="y">` +
	`</head><body>` +
	`<a class="same" href="/next">next</a>` +
	`<a class="www" href="https://www.site.example/other">www</a>` +
	`<a class="foreign" href="https://other.example/x">foreign</a>` +
	`<a class="mailto" href="mailto:x@y.example">mail</a>` +
	`<script>top.location='/gone'</script>` +
	`</body></html>`

func TestOriginalKeepScriptsFalse(t *testing.T) {
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "original"}
	out, err := Original(originalRawKeepScriptsFalse, base, rules.Rule{}, links, "<div class=\"banner\">bypassed</div>")
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	doc := mustPrepare(t, out, rules.Rule{})

	scripts := doc.Find("script")
	if scripts.Length() != 1 {
		t.Fatalf("script count: got %d, want 1", scripts.Length())
	}
	if !strings.EqualFold(strings.TrimSpace(scripts.AttrOr("type", "")), "application/ld+json") {
		t.Errorf("remaining script type = %q, want application/ld+json", scripts.AttrOr("type", ""))
	}
	if strings.Contains(out, "top.location") {
		t.Error("page script survived")
	}
	if strings.Contains(out, "scriptlets.js") || strings.Contains(out, "self.__bp") {
		t.Error("scriptlet runtime emitted with keep_scripts=false")
	}

	bases := doc.Find("head base")
	if bases.Length() != 1 {
		t.Fatalf("base count: got %d, want 1", bases.Length())
	}
	if href := bases.AttrOr("href", ""); href != base.String() {
		t.Errorf("base href = %q, want %q", href, base.String())
	}

	for _, sel := range []string{`link[rel="preload"]`, `link[rel="modulepreload"]`, `link[rel="manifest"]`} {
		if n := doc.Find(sel).Length(); n != 0 {
			t.Errorf("%s: got %d, want 0", sel, n)
		}
	}
	for _, sel := range []string{`meta[http-equiv]`} {
		if n := doc.Find(sel).Length(); n != 0 {
			t.Errorf("%s: got %d, want 0", sel, n)
		}
	}
	if n := doc.Find(`meta[name="x"]`).Length(); n != 1 {
		t.Errorf("meta[name=x] removed: got %d, want 1", n)
	}

	wantNext := "https://bp.example/read?url=" + url.QueryEscape("https://site.example/next") + "&view=original"
	if href := doc.Find("a.same").AttrOr("href", ""); href != wantNext {
		t.Errorf("a.same href = %q, want %q", href, wantNext)
	}
	// Host comparison strips www., but the escaped target keeps the URL as written.
	wantWww := "https://bp.example/read?url=" + url.QueryEscape("https://www.site.example/other") + "&view=original"
	if href := doc.Find("a.www").AttrOr("href", ""); href != wantWww {
		t.Errorf("a.www href = %q, want %q", href, wantWww)
	}
	if href := doc.Find("a.foreign").AttrOr("href", ""); href != "https://other.example/x" {
		t.Errorf("a.foreign href = %q, want unchanged", href)
	}
	if href := doc.Find("a.mailto").AttrOr("href", ""); href != "mailto:x@y.example" {
		t.Errorf("a.mailto href = %q, want unchanged", href)
	}

	banner := doc.Find("body .banner")
	if banner.Length() != 1 {
		t.Fatalf("banner: got %d, want 1", banner.Length())
	}
	if body := doc.Find("body").First(); body.Children().First().Is(".banner") == false {
		t.Error("banner is not the first child of body")
	}
}

func TestOriginalKeepScriptsTrue(t *testing.T) {
	raw := `<!doctype html><html><head><title>T</title></head><body>` +
		`<script src="https://site.example/blocked.js"></script>` +
		`<script src="/ok.js"></script>` +
		`<script>top.location='/x'</script>` +
		`<script>window.top.foo=1</script>` +
		`<script>if(location.hostname=="site.example"){location.href="/y"}</script>` +
		`<script>var keepMe=1;</script>` +
		`<p>text</p>` +
		`</body></html>`
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "original"}
	rule := rules.Rule{
		KeepScripts: true,
		BlockRegex:  []*regexp.Regexp{regexp.MustCompile(`blocked\.js`)},
	}
	out, err := Original(raw, base, rule, links, "")
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	doc := mustPrepare(t, out, rules.Rule{})

	if doc.Find(`script[src="https://site.example/blocked.js"]`).Length() != 0 {
		t.Error("block-regex script[src] survived")
	}
	if doc.Find(`script[src="/ok.js"]`).Length() != 1 {
		t.Error("non-blocked script[src] removed")
	}
	for _, bad := range []string{"top.location", "window.top"} {
		if strings.Contains(out, bad) {
			t.Errorf("inline script fragment %q survived", bad)
		}
	}
	if strings.Contains(out, "location.hostname") {
		t.Error("location.hostname redirect script survived")
	}
	if !strings.Contains(out, "keepMe") {
		t.Error("benign inline script removed")
	}
}

func TestOriginalScriptlets(t *testing.T) {
	raw := `<!doctype html><html><head><title>T</title><script src="/page.js"></script></head><body><p>x</p></body></html>`
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "original"}
	calls := []rules.Scriptlet{{Name: "set", Args: []string{"a", "b"}}}

	out, err := Original(raw, base, rules.Rule{KeepScripts: true, Scriptlets: calls}, links, "")
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	doc := mustPrepare(t, out, rules.Rule{})

	if doc.Find(`script[src="https://bp.example/scriptlets.js"]`).Length() != 1 {
		t.Fatalf("scriptlet bundle script missing:\n%s", out)
	}
	if !strings.Contains(out, scriptlets.Calls(calls)) {
		t.Fatalf("dispatcher missing:\n%s", out)
	}

	head := doc.Find("head").First()
	children := head.Children()
	if children.Length() < 3 {
		t.Fatalf("head children: got %d, want >= 3", children.Length())
	}
	if href := children.Eq(0).AttrOr("href", ""); href != base.String() {
		t.Errorf("first head child = %q, want base %q", children.Eq(0).Text()+href, base.String())
	}
	if src := children.Eq(1).AttrOr("src", ""); src != "https://bp.example/scriptlets.js" {
		t.Errorf("second head child src = %q, want scriptlet bundle", src)
	}
	if children.Eq(2).Is("script") == false {
		t.Errorf("third head child = %s, want inline dispatcher script", children.Eq(2).Text())
	}
	if strings.Index(out, "https://bp.example/scriptlets.js") > strings.Index(out, "/page.js") {
		t.Error("scriptlet bundle emitted after the page script")
	}

	off, err := Original(raw, base, rules.Rule{KeepScripts: false, Scriptlets: calls}, links, "")
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	if strings.Contains(off, "scriptlets.js") || strings.Contains(off, "self.__bp") {
		t.Errorf("scriptlet runtime emitted with keep_scripts=false:\n%s", off)
	}
}

func TestOriginalScriptletArgEscaping(t *testing.T) {
	raw := `<!doctype html><html><head><title>T</title></head><body><p>x</p></body></html>`
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "original"}
	calls := []rules.Scriptlet{{Name: "set", Args: []string{"x", "</script><img src=x>"}}}

	out, err := Original(raw, base, rules.Rule{KeepScripts: true, Scriptlets: calls}, links, "")
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	doc := mustPrepare(t, out, rules.Rule{})

	if doc.Find("script").Length() != 2 {
		t.Errorf("script count: got %d, want 2 (bundle + dispatcher)", doc.Find("script").Length())
	}
	if doc.Find("img").Length() != 0 {
		t.Error("scriptlet arg broke out of the dispatcher script")
	}
	if !strings.Contains(out, `\u003c/script\u003e`) {
		t.Errorf("escaped arg missing:\n%s", out)
	}
	if got := strings.Count(out, "</script>"); got != 2 {
		t.Errorf("literal </script> count: got %d, want 2", got)
	}
}

const ldjsonRaw = `<!doctype html><html><head><title>Fallback Title</title>` +
	`<script type="application/ld+json">{"@type":"BreadcrumbList","itemListElement":[]}</script>` +
	`<script type="application/ld+json">[{"@context":"https://schema.org","@type":"NewsArticle","@graph":[{"articleBody":"too short"}]}]</script>` +
	`<script type="application/ld+json">` +
	`{"@type":"NewsArticle","@graph":[` +
	`{"@type":"Article","articleBody":"tiny"},` +
	`{"@type":"Article","articleBody":"<p>A much longer article body extracted from structured data with plenty of words.</p><p>Second paragraph.</p>"}` +
	`]}` +
	`</script>` +
	`</head><body><p>hi</p></body></html>`

func TestExtractLDJSONGraphWins(t *testing.T) {
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "reader"}
	a, err := Extract(ldjsonRaw, base, rules.Rule{LDJSON: true}, &links)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(string(a.ContentHTML), "much longer article body") {
		t.Errorf("ContentHTML missing @graph body:\n%s", a.ContentHTML)
	}
	if !strings.Contains(string(a.ContentHTML), "<p>") {
		t.Errorf("ContentHTML not wrapped in <p>:\n%s", a.ContentHTML)
	}
	if a.TextLen == 0 {
		t.Error("TextLen is 0, want the article body length")
	}
	if a.Title != "Fallback Title" {
		t.Errorf("Title = %q, want Fallback Title", a.Title)
	}
}

func TestExtractPaywalledAndTitleFallback(t *testing.T) {
	raw := `<html><head><title>Page Title</title></head>` +
		`<body><article><p>` + strings.Repeat("words here ", 200) +
		`Subscribe to continue reading the rest of this story.</p></article></body></html>`
	base := mustURL(t, "https://site.example/article")
	a, err := Extract(raw, base, rules.Rule{PaywallMarkers: []string{"subscribe to continue"}}, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !a.Paywalled {
		t.Error("Paywalled = false, want true")
	}
	if a.Title == "" {
		t.Error("Title empty, want fallback to <title>")
	}
	if a.TextLen == 0 {
		t.Error("TextLen is 0, want > 0")
	}
}

func TestExtractRewritesLinks(t *testing.T) {
	raw := `<html><head><title>T</title></head><body><article><p>` +
		strings.Repeat("word ", 200) +
		`</p><p><a href="/next">next</a> <a href="https://other.example/x">far</a></p></article></body></html>`
	base := mustURL(t, "https://site.example/article")
	links := Links{Self: "https://bp.example", Target: base, View: "reader"}
	a, err := Extract(raw, base, rules.Rule{}, &links)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := "https://bp.example/read?url=" + url.QueryEscape("https://site.example/next") + "&amp;view=reader"
	if !strings.Contains(string(a.ContentHTML), want) {
		t.Errorf("ContentHTML missing rewritten link %q:\n%s", want, a.ContentHTML)
	}
	if !strings.Contains(string(a.ContentHTML), "https://other.example/x") {
		t.Errorf("ContentHTML lost the foreign link:\n%s", a.ContentHTML)
	}
}
