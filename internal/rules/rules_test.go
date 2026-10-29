package rules

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func baseLocal() map[string]any {
	return map[string]any{
		"profiles": map[string]any{
			"browser":   map[string]string{"User-Agent": "browser"},
			"googlebot": map[string]string{"User-Agent": "googlebot"},
		},
		"referers": map[string]string{"google": "https://www.google.com/"},
		"updates":  map[string]any{},
		"defaults": map[string]any{
			"strategies":      []string{"googlebot", "browser", StrategyArchiveOrg},
			"referer":         "google",
			"random_ip":       false,
			"keep_scripts":    false,
			"ld_json":         true,
			"min_text_length": 1000,
		},
		"sites": []any{},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustParse(t *testing.T, v any) *Set {
	t.Helper()
	s, err := Parse(mustJSON(t, v))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func upstream(t *testing.T, generic map[string]any, sites ...map[string]any) *Upstream {
	t.Helper()
	doc := map[string]any{"source": "test", "sites": sites}
	if generic != nil {
		doc["generic"] = generic
	}
	u, err := ParseUpstream(mustJSON(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestShippedRulesParse(t *testing.T) {
	data, err := os.ReadFile("../../config/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
}

func TestMatchPrecedence(t *testing.T) {
	local := baseLocal()
	local["sites"] = []any{map[string]any{
		"name":             "Local",
		"domains":          []string{"x.com"},
		"remove_selectors": []string{".b"},
		"referer":          "",
	}}
	set := mustParse(t, local)
	up := upstream(t,
		map[string]any{"remove_selectors": []string{".g"}},
		map[string]any{"name": "Up", "domains": []string{"x.com"}, "prefer": "browser", "remove_selectors": []string{".a"}},
	)

	rule := set.WithUpstream([]*Upstream{up}).Match("www.x.com")
	if want := []string{"browser", "googlebot", StrategyArchiveOrg}; !reflect.DeepEqual(rule.Strategies, want) {
		t.Errorf("strategies = %v, want %v", rule.Strategies, want)
	}
	if want := []string{".g", ".a", ".b"}; !reflect.DeepEqual(rule.RemoveSelectors, want) {
		t.Errorf("remove = %v, want %v", rule.RemoveSelectors, want)
	}
	if rule.Referer != "" {
		t.Errorf("referer = %q, want none", rule.Referer)
	}
	if rule.Name != "Local" {
		t.Errorf("name = %q, want Local", rule.Name)
	}
}

func TestLocalStrategiesSuppressUpstreamPrefer(t *testing.T) {
	local := baseLocal()
	local["sites"] = []any{map[string]any{
		"name":       "Local",
		"domains":    []string{"x.com"},
		"strategies": []string{StrategyArchiveOrg},
	}}
	set := mustParse(t, local)
	up := upstream(t, nil,
		map[string]any{"name": "Up", "domains": []string{"x.com"}, "prefer": "browser"},
	)
	rule := set.WithUpstream([]*Upstream{up}).Match("x.com")
	if want := []string{StrategyArchiveOrg}; !reflect.DeepEqual(rule.Strategies, want) {
		t.Errorf("strategies = %v, want %v", rule.Strategies, want)
	}
}

func TestLongestSuffix(t *testing.T) {
	set := mustParse(t, baseLocal())
	up := upstream(t, nil,
		map[string]any{"name": "A", "domains": []string{"a.com"}},
		map[string]any{"name": "News", "domains": []string{"news.a.com"}},
	)
	set = set.WithUpstream([]*Upstream{up})

	if got := set.Match("x.news.a.com").Name; got != "News" {
		t.Errorf("name = %q, want News", got)
	}
	if got := set.Match("a.com").Name; got != "A" {
		t.Errorf("name = %q, want A", got)
	}
	if got := set.Match("notx.com").Name; got != "default" {
		t.Errorf("name = %q, want default", got)
	}
	if got := set.Match("x.notx.com").Name; got != "default" {
		t.Errorf("name = %q, want default", got)
	}
}

func TestScriptletLayering(t *testing.T) {
	local := baseLocal()
	local["sites"] = []any{map[string]any{
		"name":    "Local",
		"domains": []string{"x.com"},
		"scriptlets": []any{
			map[string]any{"name": "set", "args": []string{"local", "true"}},
		},
	}}
	set := mustParse(t, local)
	up := upstream(t,
		map[string]any{"scriptlets": []any{
			map[string]any{"name": "set", "args": []string{"generic", "false"}},
		}},
		map[string]any{
			"name":    "Up",
			"domains": []string{"x.com"},
			"scriptlets": []any{
				map[string]any{"name": "aopr", "args": []string{"ArcPOptions"}},
				map[string]any{"name": "set", "args": []string{"generic", "false"}},
			},
		},
	)
	rule := set.WithUpstream([]*Upstream{up}).Match("x.com")
	want := []Scriptlet{
		{Name: "set", Args: []string{"generic", "false"}},
		{Name: "aopr", Args: []string{"ArcPOptions"}},
		{Name: "set", Args: []string{"local", "true"}},
	}
	if !reflect.DeepEqual(rule.Scriptlets, want) {
		t.Errorf("scriptlets = %#v, want %#v", rule.Scriptlets, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		edit func(m map[string]any)
	}{
		{"unknown strategy", func(m map[string]any) {
			m["defaults"].(map[string]any)["strategies"] = []string{"nope"}
		}},
		{"bad regex", func(m map[string]any) {
			m["defaults"].(map[string]any)["block_regex"] = "("
		}},
		{"bad selector", func(m map[string]any) {
			m["defaults"].(map[string]any)["remove_selectors"] = []string{"["}
		}},
		{"unknown key", func(m map[string]any) {
			m["defaults"].(map[string]any)["bogus"] = 1
		}},
		{"profile named archive_org", func(m map[string]any) {
			m["profiles"].(map[string]any)["archive_org"] = map[string]string{"User-Agent": "x"}
		}},
		{"duplicate local domain", func(m map[string]any) {
			m["sites"] = []any{
				map[string]any{"name": "A", "domains": []string{"x.com"}},
				map[string]any{"name": "B", "domains": []string{"www.x.com"}},
			}
		}},
		{"unknown source type", func(m map[string]any) {
			m["updates"] = map[string]any{"sources": []any{
				map[string]any{"name": "a", "type": "nope", "url": "https://example.com/x"},
			}}
		}},
		{"bad scriptlet name", func(m map[string]any) {
			m["defaults"].(map[string]any)["scriptlets"] = []any{
				map[string]any{"name": "Upper", "args": []string{}},
			}
		}},
		{"control char in scriptlet arg", func(m map[string]any) {
			m["defaults"].(map[string]any)["scriptlets"] = []any{
				map[string]any{"name": "set", "args": []string{"a\x01b"}},
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := baseLocal()
			tc.edit(m)
			if _, err := Parse(mustJSON(t, m)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestUnknownUpstreamPreferIsIgnored(t *testing.T) {
	set := mustParse(t, baseLocal())
	up := upstream(t, nil,
		map[string]any{"name": "Up", "domains": []string{"x.com"}, "prefer": "nope"},
	)
	rule := set.WithUpstream([]*Upstream{up}).Match("x.com")
	if want := []string{"googlebot", "browser", StrategyArchiveOrg}; !reflect.DeepEqual(rule.Strategies, want) {
		t.Errorf("strategies = %v, want %v", rule.Strategies, want)
	}
}
