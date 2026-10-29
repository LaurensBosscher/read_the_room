package update

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"github.com/dop251/goja"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

var bpcEndRE = regexp.MustCompile(`(?m)^\}\s*;?\s*$`)

type bpcEntry struct {
	Domain        string          `json:"domain"`
	Group         json.RawMessage `json:"group"`
	UserAgent     string          `json:"useragent"`
	Referer       string          `json:"referer"`
	RefererCustom string          `json:"referer_custom"`
	RandomIP      json.RawMessage `json:"random_ip"`
	BlockRegex    string          `json:"block_regex"`
	CSCode        json.RawMessage `json:"cs_code"`
}

type bpcCS struct {
	HideElem json.RawMessage `json:"hide_elem"`
	Cond     json.RawMessage `json:"cond"`
	RMElem   json.RawMessage `json:"rm_elem"`
	RMAttrib string          `json:"rm_attrib"`
	Elems    json.RawMessage `json:"elems"`
}

// sitesJSToJSON extracts BPC's defaultSites object literal and serializes it
// as JSON, with regex literals replaced by their source strings. Only the
// literal is evaluated; the extension code after it is never run.
func sitesJSToJSON(js string) ([]byte, error) {
	const marker = "var defaultSites = {"
	start := strings.Index(js, marker)
	if start < 0 {
		return nil, errors.New("bpc: defaultSites not found in sites.js")
	}
	loc := bpcEndRE.FindStringIndex(js[start:])
	if loc == nil {
		return nil, errors.New("bpc: end of defaultSites not found")
	}
	literal := js[start : start+loc[1]]

	vm := goja.New()
	timer := time.AfterFunc(10*time.Second, func() { vm.Interrupt("timeout") })
	defer timer.Stop()

	v, err := vm.RunString(literal + "\n;JSON.stringify(defaultSites, function (k, v) { return v instanceof RegExp ? v.source : v; })")
	if err != nil {
		return nil, fmt.Errorf("bpc: evaluating sites.js: %w", err)
	}
	s, ok := v.Export().(string)
	if !ok {
		return nil, errors.New("bpc: sites.js did not produce JSON")
	}
	return []byte(s), nil
}

// convertBPCZip converts BPC's release zip: sites.js, then sites_custom.json
// for domains not covered yet, then sites_updated.json merged on top.
func convertBPCZip(data []byte) ([]rules.Site, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("bpc: %w", err)
	}

	var sitesJS, custom, updated []byte
	for _, f := range zr.File {
		name := f.Name
		switch {
		case name == "sites.js" || strings.HasSuffix(name, "/sites.js"):
			sitesJS, err = readZipFile(f)
		case strings.HasSuffix(name, "/custom/sites_custom.json"):
			custom, err = readZipFile(f)
		case strings.HasSuffix(name, "/sites_updated.json"):
			updated, err = readZipFile(f)
		}
		if err != nil {
			return nil, err
		}
	}
	if sitesJS == nil {
		return nil, errors.New("bpc: sites.js not in zip")
	}

	js, err := sitesJSToJSON(string(sitesJS))
	if err != nil {
		return nil, err
	}
	sites, err := convertBPCJSON(js)
	if err != nil {
		return nil, err
	}
	if custom != nil {
		extra, err := convertBPCJSON(custom)
		if err != nil {
			return nil, err
		}
		sites = mergeBPCSites(sites, extra, true)
	}
	if updated != nil {
		extra, err := convertBPCJSON(updated)
		if err != nil {
			return nil, err
		}
		sites = mergeBPCSites(sites, extra, false)
	}
	return sites, nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("bpc: %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxDownload))
	if err != nil {
		return nil, fmt.Errorf("bpc: %s: %w", f.Name, err)
	}
	return data, nil
}

// convertBPCJSON converts a BPC title -> entry object (sites.js JSON output,
// sites_custom.json or sites_updated.json) into per-domain sites.
func convertBPCJSON(data []byte) ([]rules.Site, error) {
	var entries map[string]bpcEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("bpc: %w", err)
	}
	return bpcSites(entries), nil
}

func bpcSites(entries map[string]bpcEntry) []rules.Site {
	titles := make([]string, 0, len(entries))
	for title := range entries {
		titles = append(titles, title)
	}
	sort.Strings(titles)

	index := make(map[string]int)
	var sites []rules.Site
	for _, title := range titles {
		entry := entries[title]
		domains := entry.domains()
		if len(domains) == 0 {
			continue
		}
		sp := bpcSpec(entry, domains)
		if specEmpty(sp) {
			continue
		}
		for _, d := range domains {
			if i, ok := index[d]; ok {
				rules.MergeSpec(&sites[i].Spec, sp)
				continue
			}
			index[d] = len(sites)
			sites = append(sites, rules.Site{Name: title, Domains: []string{d}, Spec: cloneSpec(sp)})
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Domains[0] < sites[j].Domains[0] })
	return sites
}

// mergeBPCSites adds extra on top of base. When skipExisting is set, extra
// entries never override a domain that is already present.
func mergeBPCSites(base, extra []rules.Site, skipExisting bool) []rules.Site {
	index := make(map[string]int, len(base))
	for i, s := range base {
		index[s.Domains[0]] = i
	}
	for _, s := range extra {
		if len(s.Domains) == 0 {
			continue
		}
		d := s.Domains[0]
		if i, ok := index[d]; ok {
			if skipExisting {
				continue
			}
			rules.MergeSpec(&base[i].Spec, s.Spec)
			continue
		}
		index[d] = len(base)
		base = append(base, s)
	}
	sort.Slice(base, func(i, j int) bool { return base[i].Domains[0] < base[j].Domains[0] })
	return base
}

func (e bpcEntry) domains() []string {
	var raw []string
	if len(bytes.TrimSpace(e.Group)) > 0 {
		raw = rawStrings(e.Group)
	}
	if len(raw) == 0 {
		d := strings.TrimSpace(e.Domain)
		if d == "" || strings.HasPrefix(d, "###") {
			return nil
		}
		raw = []string{d}
	}

	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		d = cleanDomain(d)
		if d == "" || strings.HasPrefix(d, "###") || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func cleanDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "www.")
	return d
}

// rawStrings decodes a JSON string (comma separated), array of strings, or
// nested array of strings.
func rawStrings(raw json.RawMessage) []string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return nil
		}
		return strings.Split(s, ",")
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil
		}
		var out []string
		for _, it := range arr {
			out = append(out, rawStrings(it)...)
		}
		return out
	}
	return nil
}

// selectorStrings decodes a selector string, a string array, or nested arrays.
func selectorStrings(raw json.RawMessage) []string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return nil
		}
		return []string{s}
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil
		}
		var out []string
		for _, it := range arr {
			out = append(out, selectorStrings(it)...)
		}
		return out
	}
	return nil
}

func truthyRaw(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", `""`:
		return false
	}
	return true
}

func rawEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func bpcReferer(e bpcEntry) *string {
	switch strings.ToLower(strings.TrimSpace(e.Referer)) {
	case "google":
		v := "https://www.google.com/"
		return &v
	case "facebook":
		v := "https://www.facebook.com/"
		return &v
	case "twitter":
		v := "https://t.co/"
		return &v
	}
	custom := strings.TrimSpace(e.RefererCustom)
	if strings.HasPrefix(custom, "http://") || strings.HasPrefix(custom, "https://") {
		return &custom
	}
	return nil
}

func bpcSpec(e bpcEntry, domains []string) rules.Spec {
	var sp rules.Spec
	switch ua := strings.ToLower(strings.TrimSpace(e.UserAgent)); ua {
	case "googlebot", "bingbot", "facebookbot":
		sp.Prefer = ua
	}
	sp.Referer = bpcReferer(e)
	if truthyRaw(e.RandomIP) {
		v := true
		sp.RandomIP = &v
	}
	if br := strings.TrimSpace(e.BlockRegex); br != "" {
		if strings.Contains(br, "{domain}") {
			br = strings.ReplaceAll(br, "{domain}", "(?:"+quotedDomains(domains)+")")
		}
		if _, err := regexp.Compile(br); err == nil {
			sp.BlockRegex = br
		}
	}
	sp.RemoveSelectors, sp.UnhideSelectors = bpcCSRules(e.CSCode)
	return sp
}

func quotedDomains(domains []string) string {
	parts := make([]string, len(domains))
	for i, d := range domains {
		parts[i] = regexp.QuoteMeta(d)
	}
	return strings.Join(parts, "|")
}

func bpcCSRules(raw json.RawMessage) (remove, unhide []string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return nil, nil
		}
		t = []byte(s)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(t, &items); err != nil {
		return nil, nil
	}
	walkCS(items, &remove, &unhide)
	return filterSelectors(remove), filterSelectors(unhide)
}

func walkCS(items []json.RawMessage, remove, unhide *[]string) {
	for _, raw := range items {
		t := bytes.TrimSpace(raw)
		if len(t) == 0 {
			continue
		}
		switch t[0] {
		case '{':
			var obj bpcCS
			if err := json.Unmarshal(t, &obj); err != nil {
				continue
			}
			if !rawEmpty(obj.HideElem) {
				*remove = append(*remove, selectorStrings(obj.HideElem)...)
			}
			cond := truthyRaw(obj.Cond)
			if cond && truthyRaw(obj.RMElem) {
				*remove = append(*remove, selectorStrings(obj.Cond)...)
			}
			if cond && (obj.RMAttrib == "style" || obj.RMAttrib == "hidden") {
				*unhide = append(*unhide, selectorStrings(obj.Cond)...)
			}
			walkCSElems(obj.Elems, remove, unhide)
		case '[':
			var nested []json.RawMessage
			if err := json.Unmarshal(t, &nested); err == nil {
				walkCS(nested, remove, unhide)
			}
		}
	}
}

func walkCSElems(raw json.RawMessage, remove, unhide *[]string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '[' {
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(t, &items); err != nil {
		return
	}
	walkCS(items, remove, unhide)
}

func filterSelectors(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, sel := range in {
		sel = strings.TrimSpace(sel)
		if sel == "" || seen[sel] {
			continue
		}
		if !validSelector(sel) {
			continue
		}
		seen[sel] = true
		out = append(out, sel)
	}
	return out
}

func validSelector(sel string) bool {
	_, err := cascadia.ParseGroup(sel)
	return err == nil
}
