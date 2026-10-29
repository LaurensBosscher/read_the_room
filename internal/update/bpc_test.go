package update

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

const bpcFixture = `var defaultSites = {
 "A": {
 domain: "###_g",
 group: ["dn.se","hd.se"],
 useragent: "googlebot",
 block_regex: /\.piano\.io\//
 },
 "B": {
 domain: "bt.no",
 useragent: "facebookbot"
 },
 "C": {
 domain: "ft.com",
 referer: "google",
 cs_code: [{"hide_elem": "div.ad"}]
 }
}
var ext_api = chrome || browser;`

func TestSitesJSToJSON(t *testing.T) {
	out, err := sitesJSToJSON(bpcFixture)
	if err != nil {
		t.Fatalf("sitesJSToJSON: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "ext_api") || strings.Contains(s, "chrome") {
		t.Fatalf("literal includes code after defaultSites: %s", s)
	}
	var entries map[string]bpcEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries["A"].BlockRegex != `\.piano\.io\/` {
		t.Errorf("A block_regex = %q, want regex source", entries["A"].BlockRegex)
	}
}

func TestSitesJSToJSONMissing(t *testing.T) {
	if _, err := sitesJSToJSON("var x = 1;"); err == nil {
		t.Fatal("want error when defaultSites is missing")
	}
}

func TestConvertBPCFixture(t *testing.T) {
	js, err := sitesJSToJSON(bpcFixture)
	if err != nil {
		t.Fatalf("sitesJSToJSON: %v", err)
	}
	sites, err := convertBPCJSON(js)
	if err != nil {
		t.Fatalf("convertBPCJSON: %v", err)
	}

	want := []string{"bt.no", "dn.se", "ft.com", "hd.se"}
	if len(sites) != len(want) {
		t.Fatalf("got %d sites, want %d", len(sites), len(want))
	}
	byDomain := make(map[string]rules.Site, len(sites))
	for i, s := range sites {
		if len(s.Domains) != 1 {
			t.Fatalf("site %d has %d domains, want 1", i, len(s.Domains))
		}
		if s.Domains[0] != want[i] {
			t.Errorf("site %d = %q, want %q (sorted)", i, s.Domains[0], want[i])
		}
		byDomain[s.Domains[0]] = s
	}

	bt := byDomain["bt.no"]
	if bt.Name != "B" || bt.Prefer != "facebookbot" {
		t.Errorf("bt.no = name %q prefer %q, want B/facebookbot", bt.Name, bt.Prefer)
	}
	for _, d := range []string{"dn.se", "hd.se"} {
		s := byDomain[d]
		if s.Name != "A" || s.Prefer != "googlebot" {
			t.Errorf("%s = name %q prefer %q, want A/googlebot", d, s.Name, s.Prefer)
		}
		if s.BlockRegex != `\.piano\.io\/` {
			t.Errorf("%s block_regex = %q, want %q", d, s.BlockRegex, `\.piano\.io\/`)
		}
	}
	ft := byDomain["ft.com"]
	if ft.Referer == nil || *ft.Referer != "https://www.google.com/" {
		t.Errorf("ft.com referer = %v, want https://www.google.com/", ft.Referer)
	}
	if len(ft.RemoveSelectors) != 1 || ft.RemoveSelectors[0] != "div.ad" {
		t.Errorf("ft.com remove = %q, want [div.ad]", ft.RemoveSelectors)
	}
}

func TestBPCDomainSubstitution(t *testing.T) {
	js := `var defaultSites = {
 "G": {
 domain: "###_g",
 group: ["a.com","b.com"],
 block_regex: /{domain}/
 }
}
var ext_api = chrome;`
	out, err := sitesJSToJSON(js)
	if err != nil {
		t.Fatalf("sitesJSToJSON: %v", err)
	}
	sites, err := convertBPCJSON(out)
	if err != nil {
		t.Fatalf("convertBPCJSON: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("got %d sites, want 2", len(sites))
	}
	for _, s := range sites {
		if s.BlockRegex != `(?:a\.com|b\.com)` {
			t.Errorf("%s block_regex = %q, want (?:a\\.com|b\\.com)", s.Domains[0], s.BlockRegex)
		}
	}
}

func TestBPCStringCSCode(t *testing.T) {
	const doc = `{"X": {
	 "domain": "example.com",
	 "cs_code": "[{\"cond\": \".promo\", \"rm_elem\": 1, \"elems\": [\".promo\", {\"hide_elem\": \".ad\"}]}, {\"cond\": \".paywall\", \"rm_attrib\": \"style\", \"elems\": [\".paywall\"]}, {\"cond\": \"div[style*='height: 0;']\", \"rm_attrib\": \"style\"}, {\"cond\": \".gate\", \"rm_elem\": 1}]"
	}}`
	sites, err := convertBPCJSON([]byte(doc))
	if err != nil {
		t.Fatalf("convertBPCJSON: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	s := sites[0]
	if strings.Join(s.RemoveSelectors, ",") != ".promo,.ad,.gate" {
		t.Errorf("remove = %q, want [.promo .ad .gate]", s.RemoveSelectors)
	}
	if strings.Join(s.UnhideSelectors, ",") != ".paywall,div[style*='height: 0;']" {
		t.Errorf("unhide = %q, want [.paywall div[style*='height: 0;']]", s.UnhideSelectors)
	}
}

func TestBPCSkipsEmptyAndHashDomains(t *testing.T) {
	const doc = `{
	 "keep": {"domain": "a.com", "useragent": "googlebot"},
	 "hash": {"domain": "###"},
	 "empty": {"domain": ""},
	 "garbage": {"domain": "b.com", "block_regex": "["}
	}`
	sites, err := convertBPCJSON([]byte(doc))
	if err != nil {
		t.Fatalf("convertBPCJSON: %v", err)
	}
	if len(sites) != 1 || sites[0].Domains[0] != "a.com" {
		t.Fatalf("got %+v, want only a.com", sites)
	}
}
