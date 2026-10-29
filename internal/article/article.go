// Package article turns fetched HTML into a reader view or a cleaned-up
// original page.
package article

import (
	"encoding/json"
	"html/template"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
	"github.com/LaurensBosscher/read_the_room/internal/scriptlets"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Article is the reader-view extraction of a page.
type Article struct {
	Title, Byline, SiteName string
	ContentHTML             template.HTML
	TextLen                 int  // rune count of whitespace-normalized text
	Paywalled               bool // a paywall marker occurs in the text
}

// Links describes how same-site links are rewritten to us.
type Links struct {
	Self   string // our origin, e.g. "https://bypass.example"
	Target *url.URL
	View   string
}

// Prepare parses rawHTML with scripting disabled, unwraps noscript content and
// applies the rule's selector tweaks. The returned node is the document node.
func Prepare(rawHTML string, rule rules.Rule) (*html.Node, error) {
	node, err := html.ParseWithOptions(strings.NewReader(rawHTML), html.ParseOptionEnableScripting(false))
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, nil
	}
	doc := goquery.NewDocumentFromNode(node)

	doc.Find("head noscript").Remove()
	doc.Find("body noscript").Each(func(_ int, sel *goquery.Selection) {
		for _, n := range sel.Nodes {
			parent := n.Parent
			if parent == nil {
				continue
			}
			for c := n.FirstChild; c != nil; {
				next := c.NextSibling
				n.RemoveChild(c)
				parent.InsertBefore(c, n)
				c = next
			}
			parent.RemoveChild(n)
		}
	})

	for _, selector := range rule.RemoveSelectors {
		if strings.TrimSpace(selector) == "" {
			continue
		}
		doc.Find(selector).Remove()
	}

	for _, selector := range rule.UnhideSelectors {
		if strings.TrimSpace(selector) == "" {
			continue
		}
		doc.Find(selector).Each(func(_ int, sel *goquery.Selection) {
			sel.RemoveAttr("style")
			sel.RemoveAttr("hidden")
			sel.RemoveAttr("aria-hidden")
		})
	}

	return node, nil
}

// Extract runs readability over a prepared page and optionally falls back to a
// longer ld+json articleBody.
func Extract(rawHTML string, base *url.URL, rule rules.Rule, links *Links) (*Article, error) {
	node, err := Prepare(rawHTML, rule)
	if err != nil {
		return nil, err
	}

	var title string
	if node != nil {
		title = normalizeText(goquery.NewDocumentFromNode(node).Find("head title").First().Text())
	}

	art := readability.Article{}
	if node != nil {
		art, err = readability.FromDocument(node, base)
	}

	var text string
	if err == nil && art.Node != nil {
		var b strings.Builder
		if rerr := art.RenderText(&b); rerr == nil {
			text = normalizeText(b.String())
		}
	}

	a := &Article{TextLen: utf8.RuneCountInString(text)}

	var content string
	if err == nil && art.Node != nil {
		if links != nil {
			rewriteLinks(art.Node, base, links)
		}
		var b strings.Builder
		if rerr := art.RenderHTML(&b); rerr == nil {
			content = b.String()
		}
	}

	if rule.LDJSON {
		if body := ldArticleBody(rawHTML); body != "" {
			if utf8.RuneCountInString(normalizeText(body)) > a.TextLen {
				c, t := ldContent(body, base, links)
				content = c
				text = t
				a.TextLen = utf8.RuneCountInString(text)
			}
		}
	}

	a.Title = strings.TrimSpace(art.Title())
	if a.Title == "" {
		a.Title = title
	}
	if a.Title == "" {
		if links != nil && links.Target != nil {
			a.Title = links.Target.String()
		} else if base != nil {
			a.Title = base.String()
		}
	}
	a.Byline = strings.TrimSpace(art.Byline())
	a.SiteName = strings.TrimSpace(art.SiteName())
	a.ContentHTML = template.HTML(content)

	lower := strings.ToLower(text)
	for _, marker := range rule.PaywallMarkers {
		m := strings.ToLower(strings.TrimSpace(marker))
		if m != "" && strings.Contains(lower, m) {
			a.Paywalled = true
			break
		}
	}

	return a, nil
}

// Original cleans a page for the original view, injecting a base tag, the
// scriptlet runtime and the banner.
func Original(rawHTML string, base *url.URL, rule rules.Rule, links Links, banner string) (string, error) {
	node, err := Prepare(rawHTML, rule)
	if err != nil {
		return "", err
	}
	if node == nil {
		return "", nil
	}
	doc := goquery.NewDocumentFromNode(node)

	if !rule.KeepScripts {
		doc.Find("script").Each(func(_ int, sel *goquery.Selection) {
			if isLDJSONScript(sel) {
				return
			}
			sel.Remove()
		})
		doc.Find(`link[rel="preload"][as="script"], link[rel="modulepreload"]`).Remove()
	} else {
		doc.Find("script").Each(func(_ int, sel *goquery.Selection) {
			if isLDJSONScript(sel) {
				return
			}
			if src, ok := sel.Attr("src"); ok {
				u, perr := url.Parse(strings.TrimSpace(src))
				if perr != nil {
					return
				}
				abs := src
				if base != nil {
					abs = base.ResolveReference(u).String()
				}
				for _, re := range rule.BlockRegex {
					if re == nil {
						continue
					}
					if re.MatchString(abs) || re.MatchString(src) {
						sel.Remove()
						return
					}
				}
				return
			}
			body := sel.Text()
			if strings.Contains(body, "location.hostname") &&
				(strings.Contains(body, "location.href") || strings.Contains(body, "location.replace")) {
				sel.Remove()
				return
			}
			if strings.Contains(body, "top.location") || strings.Contains(body, "window.top") {
				sel.Remove()
			}
		})
	}

	doc.Find("link[rel]").Each(func(_ int, sel *goquery.Selection) {
		for _, rel := range strings.Fields(sel.AttrOr("rel", "")) {
			if strings.EqualFold(rel, "manifest") {
				sel.Remove()
				return
			}
		}
	})
	doc.Find("base").Remove()
	doc.Find("meta[http-equiv]").Each(func(_ int, sel *goquery.Selection) {
		v := strings.ToLower(strings.TrimSpace(sel.AttrOr("http-equiv", "")))
		if v == "refresh" || v == "content-security-policy" {
			sel.Remove()
		}
	})

	headSel := doc.Find("head").First()
	if headSel.Length() == 0 {
		root := doc.Find("html").First()
		if root.Length() == 0 {
			return "", nil
		}
		head := newElement("head", atom.Head)
		root.Nodes[0].InsertBefore(head, root.Nodes[0].FirstChild)
		headSel = doc.Find("head").First()
	}
	head := headSel.Nodes[0]

	baseHref := ""
	if base != nil {
		baseHref = base.String()
	} else if links.Target != nil {
		baseHref = links.Target.String()
	}

	baseNode := newElement("base", atom.Base)
	setAttr(baseNode, "href", baseHref)

	nodes := []*html.Node{baseNode}
	if rule.KeepScripts && len(rule.Scriptlets) > 0 {
		src := newElement("script", atom.Script)
		setAttr(src, "src", links.Self+"/scriptlets.js")
		dispatcher := newElement("script", atom.Script)
		dispatcher.AppendChild(&html.Node{Type: html.TextNode, Data: scriptlets.Calls(rule.Scriptlets)})
		nodes = append(nodes, src, dispatcher)
	}

	referrer := newElement("meta", atom.Meta)
	setAttr(referrer, "name", "referrer")
	setAttr(referrer, "content", "no-referrer")

	robots := newElement("meta", atom.Meta)
	setAttr(robots, "name", "robots")
	setAttr(robots, "content", "noindex")

	style := newElement("style", atom.Style)
	style.AppendChild(&html.Node{Type: html.TextNode,
		Data: "html,body{overflow:auto!important;position:static!important;height:auto!important}"})

	nodes = append(nodes, referrer, robots, style)
	for i := len(nodes) - 1; i >= 0; i-- {
		prependChild(head, nodes[i])
	}

	rewriteLinks(node, base, &links)

	if strings.TrimSpace(banner) != "" {
		bodySel := doc.Find("body").First()
		if bodySel.Length() > 0 {
			frag, ferr := html.ParseFragment(strings.NewReader(banner), fragmentContext())
			if ferr == nil {
				body := bodySel.Nodes[0]
				for i := len(frag) - 1; i >= 0; i-- {
					prependChild(body, frag[i])
				}
			}
		}
	}

	return goquery.OuterHtml(doc.Selection)
}

// ldArticleBody walks every script[type="application/ld+json"] and returns the
// longest articleBody value found, including inside @graph arrays.
func ldArticleBody(rawHTML string) string {
	root, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil || root == nil {
		return ""
	}
	longest := ""
	goquery.NewDocumentFromNode(root).Find("script").Each(func(_ int, sel *goquery.Selection) {
		if !isLDJSONScript(sel) {
			return
		}
		var v any
		if json.Unmarshal([]byte(sel.Text()), &v) != nil {
			return
		}
		walkLD(v, &longest)
	})
	return longest
}

func walkLD(v any, longest *string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "articleBody" {
				if s, ok := val.(string); ok && len(s) > len(*longest) {
					*longest = s
				}
			}
			walkLD(val, longest)
		}
	case []any:
		for _, val := range t {
			walkLD(val, longest)
		}
	}
}

// ldContent turns an ld+json articleBody into HTML plus its plain text.
func ldContent(body string, base *url.URL, links *Links) (string, string) {
	if strings.Contains(body, "<p>") || strings.Contains(body, "<p ") {
		frag, err := html.ParseFragment(strings.NewReader(body), fragmentContext())
		if err != nil {
			return body, normalizeText(body)
		}
		if links != nil {
			for _, n := range frag {
				rewriteLinks(n, base, links)
			}
		}
		var b strings.Builder
		for _, n := range frag {
			_ = html.Render(&b, n)
		}
		return b.String(), normalizeText(textContent(frag))
	}

	var b strings.Builder
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b.WriteString("<p>")
		b.WriteString(html.EscapeString(line))
		b.WriteString("</p>")
		text.WriteString(line)
		text.WriteByte(' ')
	}
	return b.String(), normalizeText(text.String())
}

// rewriteLinks points same-host anchors at our reader.
func rewriteLinks(root *html.Node, base *url.URL, links *Links) {
	if root == nil || base == nil || links == nil {
		return
	}
	host := normalizeHost(base.Host)
	if host == "" {
		return
	}
	goquery.NewDocumentFromNode(root).Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, ok := sel.Attr("href")
		if !ok {
			return
		}
		u, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			return
		}
		abs := base.ResolveReference(u)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			return
		}
		if normalizeHost(abs.Host) != host {
			return
		}
		sel.SetAttr("href", links.Self+"/read?url="+url.QueryEscape(abs.String())+"&view="+links.View)
	})
}

func normalizeHost(h string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "www.")
}

func normalizeText(s string) string { return strings.Join(strings.Fields(s), " ") }

func isLDJSONScript(sel *goquery.Selection) bool {
	return strings.EqualFold(strings.TrimSpace(sel.AttrOr("type", "")), "application/ld+json")
}

func newElement(name string, a atom.Atom) *html.Node {
	return &html.Node{Type: html.ElementNode, Data: name, DataAtom: a}
}

func setAttr(n *html.Node, key, val string) {
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func prependChild(parent, child *html.Node) {
	if child.Parent != nil {
		child.Parent.RemoveChild(child)
	}
	parent.InsertBefore(child, parent.FirstChild)
}

func fragmentContext() *html.Node {
	return &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
}

func textContent(nodes []*html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return b.String()
}
