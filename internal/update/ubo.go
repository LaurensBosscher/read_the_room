package update

import (
	"bufio"
	"bytes"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

const uboName = "antipaywall"

var uboNetworkRE = regexp.MustCompile(`^\|\|([a-z0-9.-]+)\^(?:\$(.*))?$`)

// convertUBO converts liamengland1's uBO antipaywall list into a generic spec
// (generic selectors, scriptlets and the host block regex) plus per-domain
// sites.
func convertUBO(data []byte) (rules.Spec, []rules.Site, error) {
	var generic rules.Spec
	index := make(map[string]int)
	var sites []rules.Site
	hosts := make(map[string]bool)
	selectorRules, scriptletRules, skipped := 0, 0, 0

	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "[") ||
			strings.HasPrefix(line, "@@") ||
			strings.Contains(line, "#@#") || strings.Contains(line, "#?#") ||
			strings.Contains(line, "#$#") || strings.Contains(line, "#%#") {
			continue
		}

		if i := strings.Index(line, "##"); i >= 0 {
			rawDomain := strings.TrimSpace(line[:i])
			sel := line[i+2:]

			var domains []string
			if rawDomain != "" {
				domains = uboDomains(rawDomain)
				if len(domains) == 0 {
					skipped++
					continue
				}
			}
			if strings.HasPrefix(sel, "^") {
				skipped++
				continue
			}
			if strings.HasPrefix(sel, "+js(") && strings.HasSuffix(sel, ")") {
				args := splitScriptletArgs(sel[4 : len(sel)-1])
				if len(args) == 0 || args[0] == "" {
					skipped++
					continue
				}
				sc := rules.Scriptlet{Name: args[0], Args: args[1:]}
				if len(domains) == 0 {
					generic.Scriptlets = append(generic.Scriptlets, sc)
				} else {
					for _, d := range domains {
						addUBOSite(&sites, index, d, rules.Spec{Scriptlets: []rules.Scriptlet{sc}})
					}
				}
				scriptletRules++
				continue
			}
			if !validSelector(sel) {
				skipped++
				continue
			}
			if len(domains) == 0 {
				generic.RemoveSelectors = appendUnique(generic.RemoveSelectors, sel)
			} else {
				for _, d := range domains {
					addUBOSite(&sites, index, d, rules.Spec{RemoveSelectors: []string{sel}})
				}
			}
			selectorRules++
			continue
		}

		if m := uboNetworkRE.FindStringSubmatch(line); m != nil {
			if strings.Contains(m[2], "domain=") {
				skipped++
				continue
			}
			hosts[strings.ToLower(m[1])] = true
			continue
		}
		skipped++
	}
	if err := scan.Err(); err != nil {
		return generic, nil, err
	}

	sort.Slice(sites, func(i, j int) bool { return sites[i].Domains[0] < sites[j].Domains[0] })

	if len(hosts) > 0 {
		hostList := make([]string, 0, len(hosts))
		for h := range hosts {
			hostList = append(hostList, h)
		}
		sort.Strings(hostList)
		quoted := make([]string, len(hostList))
		for i, h := range hostList {
			quoted[i] = regexp.QuoteMeta(h)
		}
		generic.BlockRegex = `^https?://([^/]*\.)?(?:` + strings.Join(quoted, "|") + `)(?::\d+)?/`
	}

	log.Printf("update: ubo: %d selector rules, %d scriptlet rules, %d hosts, %d lines skipped",
		selectorRules, scriptletRules, len(hosts), skipped)
	return generic, sites, nil
}

func addUBOSite(sites *[]rules.Site, index map[string]int, domain string, sp rules.Spec) {
	if i, ok := index[domain]; ok {
		rules.MergeSpec(&(*sites)[i].Spec, sp)
		return
	}
	index[domain] = len(*sites)
	*sites = append(*sites, rules.Site{Name: uboName, Domains: []string{domain}, Spec: sp})
}

func uboDomains(s string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(s, ",") {
		d := strings.ToLower(strings.TrimSpace(part))
		if d == "" || strings.HasPrefix(d, "~") || strings.ContainsAny(d, "*/") || strings.HasSuffix(d, ".*") {
			continue
		}
		d = strings.TrimPrefix(d, "www.")
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// splitScriptletArgs mirrors uBO's ArglistParser: a trimmed argument starting
// with a quote runs to the matching unescaped quote and loses the quotes; \, is
// an escaped comma; everything else splits on commas.
func splitScriptletArgs(s string) []string {
	var args []string
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}

		var b strings.Builder
		if q := s[i]; q == '\'' || q == '"' || q == '`' {
			i++
			for i < len(s) && s[i] != q {
				if s[i] == '\\' && i+1 < len(s) {
					if s[i+1] == ',' {
						b.WriteByte(',')
					} else {
						b.WriteByte(s[i])
						b.WriteByte(s[i+1])
					}
					i += 2
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
			args = append(args, b.String())
			for i < len(s) && s[i] != ',' {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}

		for i < len(s) && s[i] != ',' {
			if s[i] == '\\' && i+1 < len(s) && s[i+1] == ',' {
				b.WriteByte(',')
				i += 2
				continue
			}
			b.WriteByte(s[i])
			i++
		}
		args = append(args, strings.TrimSpace(b.String()))
		if i < len(s) {
			i++
		}
	}
	return args
}
