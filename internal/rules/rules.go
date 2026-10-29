// Package rules defines the rule schema, layered matching and the
// hot-reloadable rule store.
package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
)

// File is the local rules.json document.
type File struct {
	Profiles            map[string]map[string]string `json:"profiles"`
	Referers            map[string]string            `json:"referers"`
	ArchiveTodayMirrors []string                     `json:"archive_today_mirrors"`
	FreediumMirrors     []string                     `json:"freedium_mirrors"`
	ChallengeMarkers    []string                     `json:"challenge_markers"`
	Updates             Updates                      `json:"updates"`
	Defaults            Spec                         `json:"defaults"`
	Sites               []Site                       `json:"sites"`
}

// Updates configures the upstream list updater.
type Updates struct {
	Interval string   `json:"interval"` // Go duration; "" = "24h"; "0" = no schedule
	Sources  []Source `json:"sources"`
}

// Source is one upstream list.
type Source struct {
	Name string `json:"name"` // ^[a-z0-9-]+$, unique; file data/upstream/<name>.json
	Type string `json:"type"` // "bpc_zip" | "bpc_json" | "ubo"
	URL  string `json:"url"`  // http(s)
}

// Spec is a set of overrides, either a site's or the defaults'.
type Spec struct {
	Strategies      []string    `json:"strategies,omitempty"`
	Prefer          string      `json:"prefer,omitempty"`  // strategy moved to the front
	Referer         *string     `json:"referer,omitempty"` // Referers key, literal http(s) URL, or "" = none
	RandomIP        *bool       `json:"random_ip,omitempty"`
	KeepScripts     *bool       `json:"keep_scripts,omitempty"`
	BlockRegex      string      `json:"block_regex,omitempty"`
	RemoveSelectors []string    `json:"remove_selectors,omitempty"`
	UnhideSelectors []string    `json:"unhide_selectors,omitempty"`
	LDJSON          *bool       `json:"ld_json,omitempty"`
	PaywallMarkers  []string    `json:"paywall_markers,omitempty"`
	MinTextLength   *int        `json:"min_text_length,omitempty"`
	Scriptlets      []Scriptlet `json:"scriptlets,omitempty"`
}

// Scriptlet is one uBO +js(...) rule.
type Scriptlet struct {
	Name string   `json:"name"` // uBO scriptlet name, e.g. "set"
	Args []string `json:"args"` // raw arguments, surrounding quotes already stripped
}

// Site is a named rule with domains.
type Site struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Spec             // embedded: fields inline in JSON
}

// Upstream is the content of data/upstream/<name>.json.
type Upstream struct {
	Source      string    `json:"source"`
	URL         string    `json:"url"`
	GeneratedAt time.Time `json:"generated_at"`
	Generic     Spec      `json:"generic"` // only list fields + block_regex are emitted by converters
	Sites       []Site    `json:"sites"`
}

// Rule is the effective rule for one host.
type Rule struct {
	Name            string // local site name, else upstream site names joined " + ", else "default"
	Strategies      []string
	Referer         string // resolved URL, "" = none
	RandomIP        bool
	KeepScripts     bool
	LDJSON          bool
	BlockRegex      []*regexp.Regexp
	RemoveSelectors []string
	UnhideSelectors []string
	PaywallMarkers  []string // lowercased
	MinTextLength   int
	Scriptlets      []Scriptlet
}

// Builtin strategies, usable even though they are not header profiles.
const (
	StrategyArchiveOrg   = "archive_org"
	StrategyArchiveToday = "archive_today"
	StrategyFreedium     = "freedium"
)

var (
	scriptletNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	sourceNameRE    = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// Set is an immutable set of rules: local rules plus upstream layers.
type Set struct {
	File *File

	referers  map[string]string
	known     map[string]bool
	knownList []string
	local     *layer
	upstreams []*Upstream
	upLayers  []*layer
}

type layer struct {
	generic compiledSpec
	sites   []Site
	specs   []compiledSpec
	index   map[string]int
}

type compiledSpec struct {
	spec  Spec
	block []*regexp.Regexp
}

func (l *layer) match(host string) (int, bool) {
	if l == nil {
		return 0, false
	}
	h := normalizeDomain(host)
	for h != "" {
		if i, ok := l.index[h]; ok {
			return i, true
		}
		i := strings.IndexByte(h, '.')
		if i < 0 {
			return 0, false
		}
		h = h[i+1:]
		if !strings.Contains(h, ".") {
			return 0, false
		}
	}
	return 0, false
}

// Parse decodes and validates the local rules.json.
func Parse(data []byte) (*Set, error) {
	var f File
	if err := decode(data, &f); err != nil {
		return nil, err
	}
	if f.Profiles == nil {
		f.Profiles = map[string]map[string]string{}
	}
	profileNames := map[string]bool{}
	for name, headers := range f.Profiles {
		if name == "" {
			return nil, fmt.Errorf("rules: profile with empty name")
		}
		if isBuiltin(name) {
			return nil, fmt.Errorf("rules: profile %q uses a builtin strategy name", name)
		}
		out := make(map[string]string, len(headers))
		for k, v := range headers {
			out[http.CanonicalHeaderKey(k)] = v
		}
		if strings.TrimSpace(out["User-Agent"]) == "" {
			return nil, fmt.Errorf("rules: profile %q has no User-Agent", name)
		}
		f.Profiles[name] = out
		profileNames[name] = true
	}
	if !profileNames["browser"] {
		return nil, fmt.Errorf("rules: missing browser profile")
	}
	if f.Referers == nil {
		f.Referers = map[string]string{}
	}
	for name, v := range f.Referers {
		if !isHTTPURL(v) {
			return nil, fmt.Errorf("rules: referer %q is not an http(s) URL", name)
		}
	}

	if f.Defaults.Strategies == nil {
		return nil, fmt.Errorf("rules: defaults: strategies is required")
	}
	if f.Defaults.Referer == nil {
		return nil, fmt.Errorf("rules: defaults: referer is required")
	}
	if f.Defaults.RandomIP == nil {
		return nil, fmt.Errorf("rules: defaults: random_ip is required")
	}
	if f.Defaults.KeepScripts == nil {
		return nil, fmt.Errorf("rules: defaults: keep_scripts is required")
	}
	if f.Defaults.LDJSON == nil {
		return nil, fmt.Errorf("rules: defaults: ld_json is required")
	}
	if f.Defaults.MinTextLength == nil {
		return nil, fmt.Errorf("rules: defaults: min_text_length is required")
	}
	if err := checkSpec("defaults", &f.Defaults, profileNames, f.Referers, false); err != nil {
		return nil, err
	}

	if f.Updates.Interval != "" && f.Updates.Interval != "0" {
		d, err := time.ParseDuration(f.Updates.Interval)
		if err != nil {
			return nil, fmt.Errorf("rules: updates.interval: %w", err)
		}
		if d < 0 {
			return nil, fmt.Errorf("rules: updates.interval: must not be negative")
		}
	}
	names := map[string]bool{}
	for _, src := range f.Updates.Sources {
		if !sourceNameRE.MatchString(src.Name) {
			return nil, fmt.Errorf("rules: source name %q is invalid", src.Name)
		}
		if names[src.Name] {
			return nil, fmt.Errorf("rules: duplicate source %q", src.Name)
		}
		names[src.Name] = true
		switch src.Type {
		case "bpc_zip", "bpc_json", "ubo":
		default:
			return nil, fmt.Errorf("rules: source %q: unknown type %q", src.Name, src.Type)
		}
		if !isHTTPURL(src.URL) {
			return nil, fmt.Errorf("rules: source %q: url is not http(s)", src.Name)
		}
	}

	seen := map[string]bool{}
	for i := range f.Sites {
		where := fmt.Sprintf("site %q", f.Sites[i].Name)
		if err := checkSpec(where, &f.Sites[i].Spec, profileNames, f.Referers, false); err != nil {
			return nil, err
		}
		if len(f.Sites[i].Domains) == 0 {
			return nil, fmt.Errorf("rules: %s has no domains", where)
		}
		for j, d := range f.Sites[i].Domains {
			d = normalizeDomain(d)
			f.Sites[i].Domains[j] = d
			if d == "" {
				return nil, fmt.Errorf("rules: %s has an empty domain", where)
			}
			if seen[d] {
				return nil, fmt.Errorf("rules: duplicate local domain %q", d)
			}
			seen[d] = true
		}
	}

	return newSet(&f), nil
}

// ParseUpstream decodes and validates one converted upstream file.
func ParseUpstream(data []byte) (*Upstream, error) {
	var u Upstream
	if err := decode(data, &u); err != nil {
		return nil, err
	}
	if err := checkSpec("generic", &u.Generic, nil, nil, true); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range u.Sites {
		where := fmt.Sprintf("site %q", u.Sites[i].Name)
		if err := checkSpec(where, &u.Sites[i].Spec, nil, nil, true); err != nil {
			return nil, err
		}
		for j, d := range u.Sites[i].Domains {
			d = normalizeDomain(d)
			u.Sites[i].Domains[j] = d
			if d == "" {
				continue
			}
			if seen[d] {
				return nil, fmt.Errorf("rules: upstream duplicate domain %q", d)
			}
			seen[d] = true
		}
	}
	return &u, nil
}

// MergeSpec applies src over dst: scalars override, lists append and dedupe.
func MergeSpec(dst *Spec, src Spec) {
	if src.Prefer != "" {
		dst.Prefer = src.Prefer
	}
	if len(src.Strategies) > 0 {
		dst.Strategies = append([]string(nil), src.Strategies...)
	}
	if src.Referer != nil {
		dst.Referer = src.Referer
	}
	if src.RandomIP != nil {
		dst.RandomIP = src.RandomIP
	}
	if src.KeepScripts != nil {
		dst.KeepScripts = src.KeepScripts
	}
	if src.LDJSON != nil {
		dst.LDJSON = src.LDJSON
	}
	if src.MinTextLength != nil {
		dst.MinTextLength = src.MinTextLength
	}
	if src.BlockRegex != "" {
		if dst.BlockRegex == "" {
			dst.BlockRegex = src.BlockRegex
		} else {
			dst.BlockRegex = "(?:" + dst.BlockRegex + ")|(?:" + src.BlockRegex + ")"
		}
	}
	dst.RemoveSelectors = appendDedup(dst.RemoveSelectors, src.RemoveSelectors)
	dst.UnhideSelectors = appendDedup(dst.UnhideSelectors, src.UnhideSelectors)
	dst.PaywallMarkers = appendDedup(dst.PaywallMarkers, src.PaywallMarkers)
	dst.Scriptlets = appendScriptlets(dst.Scriptlets, src.Scriptlets)
}

// WithUpstream returns a copy of s with upstream layers applied in order.
func (s *Set) WithUpstream(ups []*Upstream) *Set {
	ns := &Set{
		File:      s.File,
		referers:  s.referers,
		known:     s.known,
		knownList: s.knownList,
		local:     s.local,
	}
	for _, u := range ups {
		ns.upstreams = append(ns.upstreams, u)
		ns.upLayers = append(ns.upLayers, buildLayer(u.Generic, u.Sites))
	}
	return ns
}

// Upstreams returns the layers this set was built with.
func (s *Set) Upstreams() []*Upstream { return s.upstreams }

// KnownStrategies lists profile keys sorted, then the builtin strategies.
func (s *Set) KnownStrategies() []string {
	return append([]string(nil), s.knownList...)
}

// Match resolves the effective rule for host.
func (s *Set) Match(host string) Rule {
	d := s.File.Defaults
	r := Rule{
		Name:            "default",
		Strategies:      append([]string(nil), d.Strategies...),
		RandomIP:        boolValue(d.RandomIP),
		KeepScripts:     boolValue(d.KeepScripts),
		LDJSON:          boolValue(d.LDJSON),
		MinTextLength:   intValue(d.MinTextLength),
		RemoveSelectors: appendDedup(nil, d.RemoveSelectors),
		UnhideSelectors: appendDedup(nil, d.UnhideSelectors),
		PaywallMarkers:  appendLower(nil, d.PaywallMarkers),
		Scriptlets:      appendScriptlets(nil, d.Scriptlets),
		BlockRegex:      compileRegex(d.BlockRegex),
	}
	if d.Referer != nil {
		r.Referer = s.resolveReferer(*d.Referer)
	}

	prefer := d.Prefer
	upPrefer := ""
	upMatched := false

	apply := func(cs *compiledSpec) {
		sp := cs.spec
		if len(sp.Strategies) > 0 {
			r.Strategies = append([]string(nil), sp.Strategies...)
		}
		if sp.Referer != nil {
			r.Referer = s.resolveReferer(*sp.Referer)
		}
		if sp.RandomIP != nil {
			r.RandomIP = *sp.RandomIP
		}
		if sp.KeepScripts != nil {
			r.KeepScripts = *sp.KeepScripts
		}
		if sp.LDJSON != nil {
			r.LDJSON = *sp.LDJSON
		}
		if sp.MinTextLength != nil {
			r.MinTextLength = *sp.MinTextLength
		}
		if len(cs.block) > 0 {
			r.BlockRegex = append(r.BlockRegex, cs.block...)
		}
		r.RemoveSelectors = appendDedup(r.RemoveSelectors, sp.RemoveSelectors)
		r.UnhideSelectors = appendDedup(r.UnhideSelectors, sp.UnhideSelectors)
		r.PaywallMarkers = appendLower(r.PaywallMarkers, sp.PaywallMarkers)
		r.Scriptlets = appendScriptlets(r.Scriptlets, sp.Scriptlets)
	}

	for _, l := range s.upLayers {
		apply(&l.generic)
		if l.generic.spec.Prefer != "" {
			prefer = l.generic.spec.Prefer
		}
	}
	var names []string
	for _, l := range s.upLayers {
		i, ok := l.match(host)
		if !ok {
			continue
		}
		apply(&l.specs[i])
		names = append(names, l.sites[i].Name)
		upPrefer = l.sites[i].Prefer
		upMatched = true
	}
	if i, ok := s.local.match(host); ok {
		sp := s.local.sites[i]
		apply(&s.local.specs[i])
		r.Name = sp.Name
		if sp.Prefer != "" {
			prefer = sp.Prefer
		} else if len(sp.Strategies) > 0 {
			prefer = ""
		} else if upMatched {
			prefer = upPrefer
		}
	} else if upMatched {
		prefer = upPrefer
		if len(names) > 0 {
			r.Name = strings.Join(names, " + ")
		}
	}

	if prefer != "" && s.known[prefer] {
		r.Strategies = preferFirst(r.Strategies, prefer)
	}
	return r
}

func (s *Set) resolveReferer(v string) string {
	if v == "" {
		return ""
	}
	if u, ok := s.referers[v]; ok {
		return u
	}
	return v
}

func newSet(f *File) *Set {
	s := &Set{
		File:     f,
		referers: f.Referers,
		known:    map[string]bool{},
		local:    buildLayer(Spec{}, f.Sites),
	}
	keys := make([]string, 0, len(f.Profiles))
	for k := range f.Profiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.knownList = append(s.knownList, keys...)
	for _, k := range keys {
		s.known[k] = true
	}
	for _, k := range []string{StrategyArchiveOrg, StrategyArchiveToday, StrategyFreedium} {
		s.knownList = append(s.knownList, k)
		s.known[k] = true
	}
	return s
}

func buildLayer(generic Spec, sites []Site) *layer {
	l := &layer{
		generic: compileSpec(generic),
		sites:   sites,
		specs:   make([]compiledSpec, len(sites)),
		index:   make(map[string]int, len(sites)),
	}
	for i := range sites {
		l.specs[i] = compileSpec(sites[i].Spec)
		for _, d := range sites[i].Domains {
			d = normalizeDomain(d)
			if d == "" {
				continue
			}
			if _, ok := l.index[d]; !ok {
				l.index[d] = i
			}
		}
	}
	return l
}

func compileSpec(sp Spec) compiledSpec {
	return compiledSpec{spec: sp, block: compileRegex(sp.BlockRegex)}
}

func compileRegex(s string) []*regexp.Regexp {
	if s == "" {
		return nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return nil
	}
	return []*regexp.Regexp{re}
}

func checkSpec(where string, sp *Spec, profileNames map[string]bool, referers map[string]string, upstream bool) error {
	for _, st := range sp.Strategies {
		if upstream {
			continue
		}
		if !profileNames[st] && !isBuiltin(st) {
			return fmt.Errorf("rules: %s: unknown strategy %q", where, st)
		}
	}
	if !upstream && sp.Prefer != "" && !profileNames[sp.Prefer] && !isBuiltin(sp.Prefer) {
		return fmt.Errorf("rules: %s: unknown prefer %q", where, sp.Prefer)
	}
	if sp.Referer != nil && *sp.Referer != "" {
		if upstream {
			if !isHTTPURL(*sp.Referer) {
				return fmt.Errorf("rules: %s: referer must be an http(s) URL", where)
			}
		} else if _, ok := referers[*sp.Referer]; !ok && !isHTTPURL(*sp.Referer) {
			return fmt.Errorf("rules: %s: referer %q is not a referers key or http(s) URL", where, *sp.Referer)
		}
	}
	if sp.BlockRegex != "" {
		if _, err := regexp.Compile(sp.BlockRegex); err != nil {
			return fmt.Errorf("rules: %s: block_regex: %w", where, err)
		}
	}
	for _, sel := range sp.RemoveSelectors {
		if _, err := cascadia.ParseGroup(sel); err != nil {
			return fmt.Errorf("rules: %s: remove selector %q: %w", where, sel, err)
		}
	}
	for _, sel := range sp.UnhideSelectors {
		if _, err := cascadia.ParseGroup(sel); err != nil {
			return fmt.Errorf("rules: %s: unhide selector %q: %w", where, sel, err)
		}
	}
	for _, sc := range sp.Scriptlets {
		if !scriptletNameRE.MatchString(sc.Name) {
			return fmt.Errorf("rules: %s: bad scriptlet name %q", where, sc.Name)
		}
		for _, a := range sc.Args {
			for _, r := range a {
				if r < 0x20 || r == 0x7f {
					return fmt.Errorf("rules: %s: scriptlet %q has a control character in its args", where, sc.Name)
				}
			}
		}
	}
	return nil
}

func decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	return nil
}

func isBuiltin(s string) bool {
	switch s {
	case StrategyArchiveOrg, StrategyArchiveToday, StrategyFreedium:
		return true
	}
	return false
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	d = strings.TrimPrefix(d, "www.")
	return d
}

func boolValue(v *bool) bool { return v != nil && *v }

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func appendDedup(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst)+len(src))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		if seen[s] {
			continue
		}
		seen[s] = true
		dst = append(dst, s)
	}
	return dst
}

func appendLower(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst)+len(src))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		s = strings.ToLower(s)
		if seen[s] {
			continue
		}
		seen[s] = true
		dst = append(dst, s)
	}
	return dst
}

func appendScriptlets(dst, src []Scriptlet) []Scriptlet {
	if len(src) == 0 {
		return dst
	}
	key := func(s Scriptlet) string {
		return s.Name + "\x00" + strings.Join(s.Args, "\x00")
	}
	seen := make(map[string]bool, len(dst)+len(src))
	for _, s := range dst {
		seen[key(s)] = true
	}
	for _, s := range src {
		k := key(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		args := append([]string(nil), s.Args...)
		dst = append(dst, Scriptlet{Name: s.Name, Args: args})
	}
	return dst
}

func preferFirst(list []string, p string) []string {
	out := make([]string, 0, len(list)+1)
	out = append(out, p)
	for _, s := range list {
		if s != p {
			out = append(out, s)
		}
	}
	return out
}
