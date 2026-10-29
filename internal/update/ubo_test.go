package update

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"testing"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

const uboFixture = `! antipaywall fixture
nytimes.com##.magnoliaDock
##.subscriber-ad
richmond.com##+js(acis, $, reg-wall)
x.com##+js(rpnt, script, /a/, 'c,d', sedCount, 1)
*##html:has(>head) .x:style(display:block!important)
||piano.io^$third-party
/x$domain=a.com
~a.com##.y`

func TestConvertUBO(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	generic, sites, err := convertUBO([]byte(uboFixture))
	if err != nil {
		t.Fatalf("convertUBO: %v", err)
	}

	wantLog := "update: ubo: 2 selector rules, 2 scriptlet rules, 1 hosts, 3 lines skipped"
	if !strings.Contains(buf.String(), wantLog) {
		t.Errorf("log = %q, want it to contain %q", buf.String(), wantLog)
	}

	if strings.Join(generic.RemoveSelectors, ",") != ".subscriber-ad" {
		t.Errorf("generic remove = %q, want [.subscriber-ad]", generic.RemoveSelectors)
	}
	if len(generic.Scriptlets) != 0 {
		t.Errorf("generic scriptlets = %+v, want none", generic.Scriptlets)
	}
	if generic.BlockRegex == "" {
		t.Fatal("generic block_regex is empty")
	}
	re, err := regexp.Compile(generic.BlockRegex)
	if err != nil {
		t.Fatalf("block_regex: %v", err)
	}
	if !re.MatchString("https://cdn.piano.io/x.js") {
		t.Errorf("block_regex %q should match https://cdn.piano.io/x.js", generic.BlockRegex)
	}
	if re.MatchString("https://notpiano.io/") {
		t.Errorf("block_regex %q should not match https://notpiano.io/", generic.BlockRegex)
	}

	want := []string{"nytimes.com", "richmond.com", "x.com"}
	if len(sites) != len(want) {
		t.Fatalf("got %d sites, want %d: %+v", len(sites), len(want), sites)
	}
	byDomain := make(map[string]rules.Site, len(sites))
	for i, s := range sites {
		if len(s.Domains) != 1 || s.Domains[0] != want[i] {
			t.Fatalf("site %d = %q, want %q (sorted)", i, s.Domains, want[i])
		}
		if s.Name != "antipaywall" {
			t.Errorf("site %q name = %q, want antipaywall", s.Domains[0], s.Name)
		}
		byDomain[s.Domains[0]] = s
	}
	if _, ok := byDomain["a.com"]; ok {
		t.Error("~a.com should be skipped")
	}

	nyt := byDomain["nytimes.com"]
	if strings.Join(nyt.RemoveSelectors, ",") != ".magnoliaDock" {
		t.Errorf("nytimes remove = %q, want [.magnoliaDock]", nyt.RemoveSelectors)
	}

	richmond := byDomain["richmond.com"]
	if len(richmond.Scriptlets) != 1 {
		t.Fatalf("richmond scriptlets = %+v, want 1", richmond.Scriptlets)
	}
	sc := richmond.Scriptlets[0]
	if sc.Name != "acis" || strings.Join(sc.Args, "|") != "$|reg-wall" {
		t.Errorf("richmond scriptlet = %+v, want acis [\"$\" \"reg-wall\"]", sc)
	}

	x := byDomain["x.com"]
	if len(x.Scriptlets) != 1 {
		t.Fatalf("x.com scriptlets = %+v, want 1", x.Scriptlets)
	}
	sc = x.Scriptlets[0]
	wantArgs := []string{"script", "/a/", "c,d", "sedCount", "1"}
	if sc.Name != "rpnt" || strings.Join(sc.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("x.com scriptlet = %+v, want rpnt %q", sc, wantArgs)
	}
}
