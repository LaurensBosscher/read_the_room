# Read the Room (self-hosted Go web app)

## Context
Build a self-hosted Go web app at `/home/laurensarendbosscher/dev/bypasser` (currently empty) usable from phone/tablet: paste an article URL, the server tries a cascade of known tricks (bot header profiles, referer, X-Forwarded-For, archive.org, archive.today, Freedium for Medium), cleans the HTML, and shows a **reader view** (default) with a toggle to the **cleaned original page**. Per-site tricks come from a hot-reloaded local `rules.json` (BPC-style keys) **plus automatically updated upstream lists**: Bypass Paywalls Clean (release zip: `sites.js`, `custom/sites_custom.json`, `sites_updated.json`, plus the live `sites_updated.json`) and liamengland1's uBO `antipaywall.txt`. Updates run on a schedule, via `readtheroom -update`, and via an "Update rules now" button. Mobile entry points: Android PWA share target, 13ft-style prefix route (`https://host/https://site/article`), bookmarklet. No authentication.

Toolchain: Go 1.27.2 at `~/.local/go/bin/go` (not on PATH; prefix commands with `export PATH=$HOME/.local/go/bin:$PATH`). Podman available. Module path `github.com/LaurensBosscher/read_the_room`; repo `LaurensBosscher/read_the_room`; working directory `/home/laurensarendbosscher/dev/bypasser`.

Dependencies (verified on proxy.golang.org):
- `github.com/PuerkitoBio/goquery v1.13.0` (pulls `github.com/andybalholm/cascadia`, used directly for selector validation via `cascadia.ParseGroup`)
- `codeberg.org/readeck/go-readability/v2 v2.1.3`: `readability.FromDocument(doc *html.Node, pageURL *url.URL) (Article, error)`; `Article.Node *html.Node`; methods `Title()`, `Byline()`, `SiteName()`, `RenderHTML(w)`, `RenderText(w)`
- `github.com/dop251/goja` (latest pseudo-version; pure-Go JS engine, used only to evaluate BPC's `sites.js` object literal)
- `golang.org/x/net` (`html`, `html/charset`)
- Vendored, not a Go module: uBO 1.75.0's `js/resources/*.js` plus `js/arglist-parser.js`, `js/jsonpath.js`, `js/urlskip.js` (GPL-3.0). Entry: `import {builtinScriptlets} from './resources/scriptlets.js'; self.scriptletGlobals ??= {}; const m = Object.create(null); for (const s of builtinScriptlets) { m[s.name.slice(0,-3)] = s.fn; for (const a of s.aliases ?? []) m[a.slice(0,-3)] = s.fn; } self.__bp = m;` → `esbuild entry.js --bundle --format=iife --minify --outfile=scriptlets.min.js` (100 KB; aliases are required because the list uses `set`/`rpnt`/`acis`/`aopr`). Commit the output with its header plus uBO's `LICENSE.txt`; keep the sources under `internal/scriptlets/vendor/` and Node is not a build dependency.

Facts about upstream data (verified 2026-10-28):
- BPC zip `https://gitflic.ru/project/magnolia1234/bpc_uploads/blob/raw?file=bypass-paywalls-chrome-clean-master.zip` (~317 KB) has entries `bypass-paywalls-chrome-clean-master/sites.js` (JS: `var defaultSites = {` … closing `}` alone on a line, followed by extension code using `chrome` APIs), `…/custom/sites_custom.json`, `…/sites_updated.json`.
- `sites.js` entries: keys unquoted, `block_regex` as JS regex literals (all flagless; all 359 compile in Go RE2 after substituting `{domain}`), `group` arrays for multi-domain entries whose `domain` starts with `###`. 591 entries / 934 domains; `useragent` values `googlebot` (54), `facebookbot` (4); `referer` values `google`; `random_ip` values `"eu"`/truthy; `cs_code` arrays of `{hide_elem}` / `{cond, rm_attrib}` objects.
- `sites_custom.json`: JSON object title → entry; `block_regex` and `cs_code` are strings (`cs_code` = JSON text of `[{"cond":…, "rm_elem":1, "rm_attrib":"style", "elems":[…]}]`); `group` may be a comma-separated string; contains a `"###_remove_sites"` entry with `domain: "###"`.
- `https://gitflic.ru/project/magnolia1234/bpc_updates/blob/raw?file=sites_updated.json`: same JSON shape as `sites_custom.json` (array `group`).
- BPC's own googlebot mode sends UA + `Referer: https://www.google.com/` + `X-Forwarded-For: 66.249.66.1`.
- antipaywall.txt (~606 lines): `domains##selector` cosmetic rules, `##selector` generic, `||host^$opts` network rules, and `+js(...)` scriptlets. 62 scriptlet rules: `set` 23, `rpnt` 6, `json-prune` 5, `trusted-replace-xhr-response` 4, `ra`/`rc` 4, `aopr`/`acis`/`aeld` 9, cookie 5, `no-fetch-if` 2, `set-local-storage-item`/`trusted-replace-fetch-response`/`nano-stb`/`remove-attr` 1 each. Args are comma-split with quote awareness and surrounding quotes stripped (uBO's `ArglistParser`); the longest `rpnt` line contains 32 commas inside a quoted replacement. `:style(`/`:upward(`/`@@`/`#@#` still cannot be applied.
- uBO executes a scriptlet as `fn(<JSON args>)` in page context at `document_start`; its cookie scriptlets are `world: 'ISOLATED'` and read `document.cookie`, so in a page we serve they would act on our origin's cookies, not the publisher's.

## Layout
```
main.go                        flags/env, embeds config/rules.json, -update mode, starts server
config/rules.json              local rules (also embedded as fallback)
internal/rules/rules.go        schema, Parse/ParseUpstream, MergeSpec, layered Match
internal/rules/store.go        hot-reload Store (local file + upstream files)
internal/fetch/fetch.go        HTTP client, SSRF guard, strategies, challenge detection
internal/article/article.go    Prepare, reader extraction, ld+json, original view, link rewrite
internal/bypass/bypass.go      cascade, verdict, result cache
internal/update/update.go      Updater: download, convert, validate, atomic write, schedule, rate limit
internal/update/bpc.go         BPC zip/json → rules.Site (goja for sites.js)
internal/update/ubo.go         uBO list → rules.Spec generic + []rules.Site
internal/scriptlets/scriptlets.min.js  vendored uBO 1.75.0 bundle (GPL-3.0 header kept)
internal/scriptlets/vendor/            uBO 1.75.0 module sources + LICENSE.txt
internal/scriptlets/scriptlets.go      go:embed, /scriptlets.js body, Original-view dispatcher
internal/server/server.go      routes, handlers, manifest, icons; templates in internal/server/templates/*.html (go:embed)
Dockerfile, compose.yaml, README.md
```

## Approach
Steps 1→5 build the core; step 6 depends on 1; step 7 depends on 5 and 6. Each step compiles and keeps earlier tests green.

### 1. Rules schema, layering, matching (`internal/rules`)
```go
type File struct {
    Profiles            map[string]map[string]string `json:"profiles"`   // strategy name -> request headers
    Referers            map[string]string            `json:"referers"`
    ArchiveTodayMirrors []string                     `json:"archive_today_mirrors"`
    FreediumMirrors     []string                     `json:"freedium_mirrors"`
    ChallengeMarkers    []string                     `json:"challenge_markers"`
    Updates             Updates                      `json:"updates"`
    Defaults            Spec                         `json:"defaults"`
    Sites               []Site                       `json:"sites"`
}
type Updates struct {
    Interval string   `json:"interval"` // Go duration; "" = "24h"; "0" = no schedule
    Sources  []Source `json:"sources"`
}
type Source struct {
    Name string `json:"name"` // ^[a-z0-9-]+$, unique; file data/upstream/<name>.json
    Type string `json:"type"` // "bpc_zip" | "bpc_json" | "ubo"
    URL  string `json:"url"`  // http(s)
}
type Spec struct {
    Strategies      []string `json:"strategies,omitempty"`
    Prefer          string   `json:"prefer,omitempty"`      // strategy moved to the front
    Referer         *string  `json:"referer,omitempty"`     // Referers key, literal http(s) URL, or "" = none
    RandomIP        *bool    `json:"random_ip,omitempty"`
    KeepScripts     *bool    `json:"keep_scripts,omitempty"`
    BlockRegex      string   `json:"block_regex,omitempty"`
    RemoveSelectors []string `json:"remove_selectors,omitempty"`
    UnhideSelectors []string `json:"unhide_selectors,omitempty"`
    LDJSON          *bool    `json:"ld_json,omitempty"`
    PaywallMarkers  []string `json:"paywall_markers,omitempty"`
    MinTextLength   *int     `json:"min_text_length,omitempty"`
    Scriptlets      []Scriptlet `json:"scriptlets,omitempty"`
}
type Scriptlet struct {
    Name string   `json:"name"` // uBO scriptlet name, e.g. "set"
    Args []string `json:"args"` // raw arguments, surrounding quotes already stripped
}
type Site struct {
    Name    string   `json:"name"`
    Domains []string `json:"domains"`
    Spec             // embedded: fields inline in JSON
}
type Upstream struct { // content of data/upstream/<name>.json
    Source      string    `json:"source"`
    URL         string    `json:"url"`
    GeneratedAt time.Time `json:"generated_at"`
    Generic     Spec      `json:"generic"` // only list fields + block_regex are emitted by converters
    Sites       []Site    `json:"sites"`
}
type Rule struct { // effective rule for one host
    Name            string   // local site name, else upstream site names joined " + ", else "default"
    Strategies      []string
    Referer         string   // resolved URL, "" = none
    RandomIP, KeepScripts, LDJSON bool
    BlockRegex      []*regexp.Regexp
    RemoveSelectors, UnhideSelectors []string
    PaywallMarkers  []string // lowercased
    MinTextLength   int
    Scriptlets      []Scriptlet
}
const (StrategyArchiveOrg = "archive_org"; StrategyArchiveToday = "archive_today"; StrategyFreedium = "freedium")
func Parse(data []byte) (*Set, error)                  // local rules.json
func ParseUpstream(data []byte) (*Upstream, error)
func MergeSpec(dst *Spec, src Spec)                    // used by converters
func (s *Set) WithUpstream(ups []*Upstream) *Set       // copy of s with upstream layers (in Updates.Sources order)
func (s *Set) Match(host string) Rule
func (s *Set) KnownStrategies() []string               // sorted profile keys, then archive_org, archive_today, freedium
func (s *Set) Upstreams() []*Upstream
```
- Both decoders use `json.Decoder.DisallowUnknownFields` (typos fail loudly).
- `Parse` validation (errors name site/field): profile without `User-Agent`; missing `browser` profile (archive/freedium fetches use it); profile key equal to a builtin strategy; strategy or `prefer` that is neither a profile key nor builtin; referer not a `referers` key / `http(s)://` literal / `""`; `regexp.Compile` failure; `cascadia.ParseGroup` failure; site without domains; same domain in two local sites; `defaults` missing any of `strategies`, `referer`, `random_ip`, `keep_scripts`, `ld_json`, `min_text_length`; bad `updates.interval` (`time.ParseDuration`, `"0"` allowed); source with invalid/duplicate name, unknown type, non-http(s) URL; scriptlet `name` not matching `^[a-z][a-z0-9-]*$`, or an arg containing a control character. Header names are canonicalized with `http.CanonicalHeaderKey`.
- `ParseUpstream` validation: regexes, selectors, referer must be `""` or `http(s)://` literal (upstream can't depend on local `referers` keys), no duplicate domains. `prefer` is not validated (unknown values are ignored by `Match`). Scriptlets get the same name/arg validation; unknown names are allowed because the pinned bundle decides what runs.
- `MergeSpec`: non-empty `Prefer`/`Strategies` and non-nil pointers in `src` override `dst`; list fields are appended with duplicates removed (first occurrence kept; scriptlets dedupe by `name` + args); `BlockRegex`: if both non-empty, `dst = "(?:" + dst + ")|(?:" + src + ")"`, else the non-empty one.
- Domain index: per layer build `map[string]int` (domain → site index), domains lowercased with trailing dot and leading `www.` removed. Lookup: normalize host, try host, then repeatedly strip the leftmost label (stop before the bare TLD); first hit = longest suffix match.
- `Match` layering (regexes and referers precompiled at parse time, not per request):
  1. Start from `defaults`.
  2. Apply every upstream's `Generic` in `Updates.Sources` order.
  3. For each upstream in order, apply its longest-matching site.
  4. Apply the longest-matching local site.
  Scalars: later layer overrides. Lists and regexes: appended; scriptlets dedupe by `name` + args. Strategies: a local site's `strategies` override defaults. Prefer: the local site's `prefer` if set; else none if the local site set `strategies` (an explicit local list wins); else the last upstream site's `prefer`. If prefer is in `KnownStrategies()`, remove it from the list and insert it at index 0.
- Store (`store.go`):
  ```go
  func NewStore(rulesPath string, builtin []byte, upstreamDir string) (*Store, error)
  func (s *Store) Current() *Set
  func (s *Store) Reload() error
  ```
  - `NewStore`: if `rulesPath` exists, parse it (error = startup fails). If it doesn't exist (`os.IsNotExist`), parse `builtin` and log `rules: %s not found, using built-in rules`. Then load upstreams.
  - Loading upstreams: for each configured source, read `<upstreamDir>/<name>.json`. A missing file is skipped silently. A file that fails to parse is skipped with log `rules: ignoring upstream %s: %v` and never fails startup. Files for sources not in `updates.sources` are not loaded.
  - `Current()`: at most once per 2 s (mutex), stat the rules file and the configured upstream files. If any mtime changed, rebuild. On success, swap and log `rules: reloaded (%d local sites, %d upstream sites)`. If the local file fails to parse, log `rules: reload failed, keeping previous rules: %v` and remember the mtimes so the error isn't logged every 2 s.
  - `Reload()`: the same rebuild done immediately, returning the error.

### 2. Shipped local rules (`config/rules.json`)
Write exactly:
```json
{
  "profiles": {
    "browser": {"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"},
    "googlebot": {"User-Agent": "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Mobile Safari/537.36 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "Referer": "https://www.google.com/", "X-Forwarded-For": "66.249.66.1"},
    "bingbot": {"User-Agent": "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)"},
    "facebookbot": {"User-Agent": "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)"},
    "twitterbot": {"User-Agent": "Twitterbot/1.0"}
  },
  "referers": {"google": "https://www.google.com/", "facebook": "https://www.facebook.com/", "twitter": "https://t.co/"},
  "archive_today_mirrors": ["archive.ph", "archive.today", "archive.is", "archive.li"],
  "freedium_mirrors": ["freedium-mirror.cfd", "freedium.cfd"],
  "challenge_markers": [
    "just a moment...", "checking your browser", "cf-challenge", "cf_chl_", "ddos protection",
    "attention required! | cloudflare", "enable javascript and cookies to continue",
    "please verify you are a human", "_incapsula_resource", "incapsula incident",
    "captcha-delivery.com", "enable js and disable any ad blocker"
  ],
  "updates": {
    "interval": "24h",
    "sources": [
      {"name": "bpc", "type": "bpc_zip", "url": "https://gitflic.ru/project/magnolia1234/bpc_uploads/blob/raw?file=bypass-paywalls-chrome-clean-master.zip"},
      {"name": "bpc-updates", "type": "bpc_json", "url": "https://gitflic.ru/project/magnolia1234/bpc_updates/blob/raw?file=sites_updated.json"},
      {"name": "antipaywall", "type": "ubo", "url": "https://raw.githubusercontent.com/liamengland1/miscfilters/master/antipaywall.txt"}
    ]
  },
  "defaults": {
    "strategies": ["googlebot", "browser", "bingbot", "facebookbot", "archive_org", "archive_today"],
    "referer": "google",
    "random_ip": false,
    "keep_scripts": false,
    "block_regex": "(tinypass\\.com|piano\\.io|poool\\.fr|zephr\\.com|evolok\\.(com|net)|pelcro\\.com|wallkit\\.net|qiota\\.com|mediapass\\.com|onecount\\.net|mppglobal\\.com|mppapi\\.io|lightboxcdn\\.com|sophi\\.io|news\\.google\\.com/swg/|/amp-access-|leaky-paywall|tncms/api/access)",
    "unhide_selectors": [".subscriber-only"],
    "ld_json": true,
    "paywall_markers": [
      "subscribe to continue", "subscribe to read", "to continue reading", "this article is for subscribers",
      "create a free account to continue", "sign in to continue reading", "you have reached your limit",
      "become a member to read", "lees verder met", "dit artikel is exclusief voor abonnees",
      "log in om verder te lezen"
    ],
    "min_text_length": 1000
  },
  "sites": [
    {"name": "Medium", "domains": ["medium.com"], "strategies": ["freedium", "archive_today", "archive_org"]},
    {"name": "The New York Times", "domains": ["nytimes.com"], "strategies": ["archive_today", "archive_org", "googlebot"]}
  ]
}
```
Per-site selectors and bot preferences come from the upstream lists. The local file only holds overrides the lists don't express.

### 3. Fetching strategies (`internal/fetch`)
```go
type Fetcher struct {
    AllowPrivate  bool          // READTHEROOM_ALLOW_PRIVATE=1
    ArchiveOrgAPI string        // default "https://archive.org/wayback/available"
    Timeout       time.Duration // per attempt, default 20s
}
type Response struct {
    HTML     string   // UTF-8
    BaseURL  *url.URL // URL relative links/assets resolve against
    FinalURL *url.URL // where the HTML actually came from
}
func New(allowPrivate bool) *Fetcher
func (f *Fetcher) Fetch(ctx context.Context, set *rules.Set, rule rules.Rule, strategy string, target *url.URL) (*Response, error)
func IsChallenge(html string, markers []string) bool
```
- Transport and client:
  - One shared `http.Transport` with `Proxy: nil` and a `net.Dialer` whose `Control` rejects (unless `AllowPrivate`) loopback, private, link-local unicast/multicast, multicast, unspecified and `100.64.0.0/10` IPs, with error `blocked private address <ip>`.
  - Per attempt: a new `http.Client` on that transport, a fresh `cookiejar.New(nil)`, `context.WithTimeout(ctx, f.Timeout)`, and the default 10-redirect limit.
- `get(ctx, u, headers)`:
  - Request: always add `Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8` and `Accept-Language: en-US,en;q=0.9`. Don't set `Accept-Encoding` manually.
  - Status: non-2xx → `HTTP <code>`.
  - Content type: the media type must be `text/html` or `application/xhtml+xml`. An empty type is accepted only if `http.DetectContentType` returns `text/html…`. Anything else → `not an HTML page (<type>)`.
  - Body: `charset.NewReader(io.LimitReader(body, 10<<20), contentType)`.
  - Challenge: `IsChallenge` true → `anti-bot challenge page`.
  - `FinalURL = resp.Request.URL`.
- `IsChallenge`: lowercase the first 8000 bytes and count the markers present. It's a challenge if `hits >= 1 && len(html) < 20000`, or if `hits >= 2`.
- Strategy dispatch:
  - **Profile key**: set every header of `set.File.Profiles[strategy]`. Then, only if the profile has no `Referer`, set `Referer: rule.Referer` when non-empty. Only if the profile has no `X-Forwarded-For` and `rule.RandomIP` is true, set a random public IPv4 (loop random 4 bytes until it passes the dialer's public check). `BaseURL = FinalURL`.
  - **archive_org**:
    - GET `ArchiveOrgAPI?url=<target>` with the `browser` profile and decode `archived_snapshots.closest.{available,url}`. Not available → `no archive.org snapshot`.
    - Convert the snapshot URL with `waybackRawURL(s string) string`: regexp `^https?://web\.archive\.org/web/(\d+)/` → `https://web.archive.org/web/${1}id_/`. A URL that already has `id_/` only gets https forced.
    - `get` it with the `browser` profile. `BaseURL = target`, because the raw snapshot is unrewritten and its assets load from the live origin.
  - **archive_today**: for each mirror, `get("https://<mirror>/newest/" + target.String())` with the `browser` profile.
    - Skip the mirror on error, if `FinalURL` doesn't match `^https?://archive\.[a-z]+/[A-Za-z0-9]{4,}$`, or if the lowercased first 5000 bytes contain `no results`.
    - First success wins, with `BaseURL = FinalURL`.
    - If nothing succeeds: `no archive.today snapshot`, or the last error if every mirror errored.
  - **freedium**: for each mirror, `get("https://<mirror>/" + target with query and fragment removed)` with the `browser` profile.
    - Accept only if the page is at least 2000 bytes, has no `ARTICLE_NOT_FOUND`, and contains `main-content` or `<main`.
    - `BaseURL = FinalURL`. If no mirror works: `no freedium copy`.

### 4. HTML processing (`internal/article`)
```go
type Article struct {
    Title, Byline, SiteName string
    ContentHTML template.HTML
    TextLen     int  // rune count of whitespace-normalized text
    Paywalled   bool // a paywall marker occurs in the text
}
type Links struct { Self string /* our origin, e.g. "https://bypass.example" */; Target *url.URL; View string }
func Prepare(rawHTML string, rule rules.Rule) (*html.Node, error)
func Extract(rawHTML string, base *url.URL, rule rules.Rule, links *Links) (*Article, error)
func Original(rawHTML string, base *url.URL, rule rules.Rule, links Links, banner string) (string, error)
```
`internal/scriptlets`: `func JS() []byte` (the embedded bundle) and `func Calls(ss []rules.Scriptlet) string` (the inline dispatcher source; `""` when `ss` is empty).
- **Prepare** (shared by both views):
  - Parse with `html.ParseWithOptions(r, html.ParseOptionEnableScripting(false))` so `<noscript>` content becomes real elements, and wrap the tree with `goquery.NewDocumentFromNode`.
  - Remove `head noscript`. Replace each `body noscript` with its children.
  - Remove `rule.RemoveSelectors` matches.
  - On `rule.UnhideSelectors` matches, remove the `style`, `hidden` and `aria-hidden` attributes.
- **Extract**:
  - Run `Prepare`, then `readability.FromDocument(node, base)`. A nil `Node` or an error means empty text, not failure.
  - Text = `strings.Join(strings.Fields(RenderText), " ")`.
  - If `rule.LDJSON`: parse rawHTML again (Readability mutates the tree) and `json.Unmarshal` every `script[type="application/ld+json"]` into `any`, skipping invalid ones. Walk maps and slices recursively (this covers arrays and `@graph`) and take the longest string under key `articleBody`.
  - If that body's normalized rune count beats Readability's, use it: if it contains `<p>` or `<p `, `html.ParseFragment` and render it; otherwise split on `\n`, trim, drop empty lines, and wrap each line as `<p>` + `html.EscapeString(line)` + `</p>`.
  - Title: Readability's `Title()`, then `<title>` text, then the target URL.
  - `Paywalled` = any lowercase marker appears in the lowercased text.
  - If `links != nil`, run `rewriteLinks` before rendering `ContentHTML` (the verdict pass in step 5 passes nil).
- **Original**: run `Prepare`, then:
  - When `keep_scripts == false`: remove every `script` except `type="application/ld+json"`, and remove `link[rel=preload][as=script]` and `link[rel=modulepreload]`.
  - When `keep_scripts == true`:
    - Remove `script[src]` whose resolved src matches any `rule.BlockRegex`.
    - Remove inline scripts containing `location.hostname` together with `location.href` or `location.replace`.
    - Remove inline scripts containing `top.location` or `window.top`.
    - When `len(rule.Scriptlets) > 0`: emit, immediately after `<base>` and before every page script, `<script src="{Self}/scriptlets.js"></script>` plus an inline dispatcher that calls each rule scriptlet. Build the calls in Go as `[[name, [args…]], …]`, `json.Marshal` them (which escapes `<`, `>`, `&`, so no `</script>` breakout) and have the dispatcher run `self.__bp[name].apply(undefined, args)` inside `try/catch`, skipping names the bundle does not define. The `src` must be absolute `{Self}` because `<base>` points at the article.
  - Always remove `link[rel=manifest]`, all existing `base` tags, and `meta[http-equiv]` whose value is `refresh` or `content-security-policy` (case-insensitive).
  - Prepend to `head` in this final order: `<base href="{BaseURL}">`, then the scriptlet pair from the bullet above when enabled, then `<meta name="referrer" content="no-referrer">`, `<meta name="robots" content="noindex">`, `<style>html,body{overflow:auto!important;position:static!important;height:auto!important}</style>`.
  - Run `rewriteLinks`, prepend `banner` as the first child of `body`, and render with `goquery.OuterHtml(doc.Selection)`.
- `rewriteLinks(sel, base, l)`:
  - For each `a[href]`, resolve it against base.
  - If the scheme is http(s) and the host (lowercased, `www.` stripped) equals the target host (normalized the same way), set href = `l.Self + "/read?url=" + url.QueryEscape(abs) + "&view=" + l.View`.
  - Links must be absolute with `l.Self`, because in original view the injected `<base>` would resolve `/read` against the article's site.

### 5. Cascade, verdict, cache (`internal/bypass`)
```go
type Attempt struct { Strategy, Status string } // "ok", "weak (523 chars, paywall text)", "error: HTTP 403"
type Result struct {
    Target   *url.URL
    Rule     rules.Rule
    Strategy string
    Resp     *fetch.Response
    Attempts []Attempt
    Complete bool // false = best weak candidate shown
}
type FailedError struct{ Attempts []Attempt }
type Bypasser struct { Rules *rules.Store; Fetcher *fetch.Fetcher /* + cache */ }
func (b *Bypasser) Run(ctx context.Context, target *url.URL, only string) (*Result, error)
```
- Setup:
  - `set := b.Rules.Current(); rule := set.Match(target.Hostname())`.
  - Strategies: `[]string{only}` if `only != ""` (the caller validated it), else `rule.Strategies`.
  - Overall `context.WithTimeout(ctx, 60*time.Second)`.
- Per strategy, in order:
  - Call `Fetch`. On error, record `error: <msg>` and continue.
  - Otherwise run `article.Extract(resp.HTML, resp.BaseURL, rule, nil)`. It's weak if `TextLen < rule.MinTextLength` or `Paywalled`.
  - Weak: record the status and keep the response as the candidate if its `TextLen` is strictly greater than the current best.
  - Not weak: record `ok` and return immediately with `Complete: true`.
- When all strategies are exhausted: return the best candidate with `Complete: false`, or `*FailedError` if there is none.
- Cache: `map[string]entry`, key `target.String()+"\x00"+only`, TTL 10 min, max 64 entries (drop expired, then oldest). Only successful results are cached; this makes the view toggle free.
- Log one line per run: `bypass %s via=%s rule=%q complete=%t %s attempts=[%s]` (attempts as `strategy:status` joined by `; `).

### 6. Upstream list updater (`internal/update`), depends on step 1
```go
type Updater struct {
    Store  *rules.Store
    Dir    string       // <data>/upstream
    Client *http.Client // Timeout 30s
    // mu sync.Mutex (held during a run); state mutex guarding lastRun and errs map[string]string
}
var ErrBusy = errors.New("update already running")
type RateLimitError struct{ Wait time.Duration }
type SourceStatus struct { Name, Type string; Sites int; UpdatedAt time.Time; LastError string }
func New(store *rules.Store, dataDir string) *Updater
func (u *Updater) Run(ctx context.Context) (map[string]error, error)       // ErrBusy if a run is active
func (u *Updater) RunManual(ctx context.Context) (map[string]error, error) // also *RateLimitError if last run started < 5 min ago
func (u *Updater) Start(ctx context.Context)                               // background schedule
func (u *Updater) Status() []SourceStatus                                  // configured sources, upstream metadata + last error
```
- **Run**:
  - `mu.TryLock()` (fail → `ErrBusy`), set `lastRun = now`, then `os.MkdirAll(Dir, 0o755)`.
  - For each source in `Store.Current().File.Updates.Sources`:
    - Download: GET with the `browser` profile headers. Non-2xx → `HTTP %d`. Read through `io.LimitReader` at 32 MiB.
    - Convert by type into `rules.Upstream{Source: name, URL: url, GeneratedAt: time.Now().UTC(), Generic, Sites}`.
    - If there are zero sites and an empty generic → `no usable rules` (protects against garbage downloads).
    - `json.MarshalIndent`, then round-trip through `rules.ParseUpstream` to validate.
    - Write `<name>.json.tmp`, then `os.Rename` over `<name>.json`.
    - Log `update: %s ok (%d sites)` or `update: %s failed: %v`, and record the per-source error (cleared on success).
    - A failed source leaves its previous file untouched.
  - After the loop, call `Store.Reload()`.
- **RunManual**: if `time.Since(lastRun) < 5*time.Minute`, return `&RateLimitError{Wait: 5m - elapsed}`; else call `Run`.
- **Start**:
  - Parse `updates.interval` once at startup (`""` → 24h). `0` → return; the button and CLI still work.
  - In a goroutine: if any configured source's upstream is missing, or its `GeneratedAt` is older than the interval, call `Run`. Then `time.NewTicker(interval)` calls `Run` on each tick until ctx is done.
  - Changing the interval requires a restart.
- **Status**: one entry per configured source. `Sites`/`UpdatedAt` come from `Store.Current().Upstreams()` (zero = never). `LastError` comes from the in-memory errors map.
- **BPC conversion (`bpc.go`)**:
  ```go
  func convertBPCZip(data []byte) ([]rules.Site, error)
  func convertBPCJSON(data []byte) ([]rules.Site, error)
  func sitesJSToJSON(js string) ([]byte, error)
  ```
  - `sitesJSToJSON`:
    - Find `var defaultSites = {` (missing → `bpc: defaultSites not found in sites.js`). The end is the first match of `(?m)^\}\s*;?\s*$` after it (missing → `bpc: end of defaultSites not found`).
    - With `goja.New()`, `RunString(literal + "\n;JSON.stringify(defaultSites, function (k, v) { return v instanceof RegExp ? v.source : v; })")`. A `time.AfterFunc(10*time.Second, …)` calls `vm.Interrupt("timeout")`.
    - Return the resulting string as bytes.
  - Entry decoding: non-strict `map[string]bpcEntry`, iterated in sorted title order.
    - Fields used: `domain`, `group` (array, or comma-separated string), `useragent`, `referer`, `referer_custom`, `random_ip` (raw JSON), `block_regex` (string), `cs_code` (raw: a JSON array, or a string holding JSON array text).
    - Domains: the `group` entries if present, else `domain` unless empty or starting with `###`. Trim, lowercase, strip `www.`. Entries with no domains are skipped.
  - Field mapping into a `rules.Spec`:
    - `useragent` ∈ {googlebot, bingbot, facebookbot} → `Prefer`.
    - `referer` google/facebook/twitter → `Referer` = `https://www.google.com/`, `https://www.facebook.com/`, `https://t.co/`. Otherwise an http(s) `referer_custom` → `Referer`.
    - `random_ip` that isn't absent, `null`, `false`, `0` or `""` → `RandomIP` = true.
    - `block_regex`: replace `{domain}` with `(?:` + `regexp.QuoteMeta` of the entry's domains joined by `|` + `)`. If it doesn't compile, drop it.
    - `cs_code` objects, walking nested `elems` recursively: `hide_elem` → RemoveSelectors; `cond` with truthy `rm_elem` → RemoveSelectors; `cond` with `rm_attrib` of `style` or `hidden` → UnhideSelectors. Drop selectors that fail `cascadia.ParseGroup`, and drop the whole field if it isn't valid JSON.
    - Everything else (cookies, dompurify, ld_json, add_ext_link, …) is ignored. An entry whose mapped Spec is empty is skipped.
  - Accumulation: domain → `*rules.Site{Name: title, Domains: [domain]}`. A later contribution for the same domain uses `rules.MergeSpec` and keeps the first name. Output is sorted by domain.
  - `convertBPCZip`:
    - `zip.NewReader`, then find entries by name suffix: `/sites.js` (required, else `bpc: sites.js not in zip`), `/custom/sites_custom.json` (optional), `/sites_updated.json` (optional).
    - Accumulate `sites.js` entries, then custom entries only for domains not yet present, then `sites_updated.json` entries merged on top.
  - `convertBPCJSON`: the same entry pipeline on a JSON object.
- **uBO conversion (`ubo.go`)**: `func convertUBO(data []byte) (rules.Spec, []rules.Site, error)`, line by line after `strings.TrimSpace`:
  - Skip empty lines, lines starting with `!` or `[`, lines starting with `@@`, and lines containing `#@#`, `#?#`, `#$#` or `#%#`.
  - Cosmetic rules (the line contains `##`):
    - Split at the first `##`. Selectors starting with `^` (regex cosmetics) are skipped.
    - Selectors starting with `+js(` and ending with `)`: parse the body with a comma splitter that mirrors uBO's `ArglistParser` — a trimmed arg starting with `'`, `"` or `` ` `` runs to the matching unescaped quote and loses the surrounding quotes; `\,` unescapes to `,`; everything else splits on commas. The first arg is the scriptlet name, the rest are its args → `Scriptlet{Name, Args}`. Unknown names stay in the data and are skipped at render.
    - Any other selector that fails `cascadia.ParseGroup` is skipped (this filters `:style(`, `:upward(`, …).
    - An empty domain part → generic `RemoveSelectors`/`Scriptlets`.
    - Otherwise split domains on `,` and drop entries that start with `~`, contain `*` or `/`, or end with `.*`. Lowercase and strip `www.`. If none remain, skip the line; else add the rule to each domain's site (`Name: "antipaywall"`).
  - Network rules matching `^\|\|([a-z0-9.-]+)\^(?:\$(.*))?$` whose options don't contain `domain=` → collect the host.
  - Generic `BlockRegex` = `^https?://([^/]*\.)?(?:` + quoted hosts joined by `|` + `)(?::\d+)?/` (omitted if there are no hosts).
  - Log `update: ubo: %d selector rules, %d scriptlet rules, %d hosts, %d lines skipped`. Sites are sorted by domain.

### 7. HTTP server and UI (`internal/server`, `main.go`), depends on steps 5 and 6
- **`main.go`**:
  - `//go:embed config/rules.json`.
  - Env vars, each overridden by the matching flag (`-addr`, `-rules`, `-data`): `READTHEROOM_ADDR` (default `:8080`), `READTHEROOM_RULES` (default `config/rules.json`), `READTHEROOM_DATA` (default `data`). `READTHEROOM_ALLOW_PRIVATE=1` sets AllowPrivate.
  - Build `rules.NewStore(rulesPath, builtin, filepath.Join(data, "upstream"))` and `update.New(store, data)`.
  - `-update` flag: `Run` with a 5-minute timeout, print one line per source (`<name>: ok (<n> sites)` or `<name>: error: <msg>`), exit 1 if any source failed, and don't start the server.
  - Otherwise: `updater.Start(ctx)`, `fetch.New`, `bypass.Bypasser`, `server.New(...)`, then `http.Server{ReadHeaderTimeout: 10s, WriteTimeout: 90s}` and log `listening on %s`.
- **Routes** (Go 1.22+ `ServeMux` patterns):
  - **`GET /{$}`** → `index.html`:
    - A form `GET /read` with URL input `name=url` (`type=url`, `inputmode=url`, autofocus) and a view radio (Reader default / Original).
    - Bookmarklet `javascript:location.href='{origin}/read?url='+encodeURIComponent(location.href)` (as `template.URL`) with the iOS hint "bookmark any page, then edit its URL to this", plus the prefix-route hint `{origin}/https://…`.
    - `<link rel="manifest" href="/manifest.webmanifest">` and an inline script registering `/sw.js`.
    - Optional `error` / `notice` message. `?updated=1` → notice `Rules updated`.
    - **Rules section**: a table from `updater.Status()` (source name, sites, `updated 2006-01-02 15:04 UTC` or `never`, last error in red) and a `<form method="post" action="/update">` with the button "Update rules now".
  - **`POST /update`** → `RunManual` with an 80 s timeout (inside WriteTimeout):
    - `ErrBusy` → 409 index with error `Update already running`.
    - `*RateLimitError` → 429 index with error `Updated recently; try again in <N> min` (N = ceil of Wait in minutes).
    - Otherwise 303 to `/?updated=1`. Per-source failures appear in the status table.
  - **`GET /read?url=&view=reader|original&strategy=`**:
    - `parseTarget`: trim; no `://` → prefix `https://`; require http(s) scheme and a host, else 400 index `Invalid URL`. An unknown `view` → `reader`. A `strategy` not in `KnownStrategies()` → 400 index `Unknown strategy`.
    - `Run`. `*FailedError` → 502 `error.html` with the target, an attempts table and retry links for each known strategy. Any other error → 502 with the message.
    - `origin(r)`: `https` if `r.TLS != nil` or `X-Forwarded-Proto == "https"`, else `http`, then `://` + `r.Host`.
    - Banner (`banner.html`, all inline styles with `position:sticky;top:0;z-index:2147483647;font:14px system-ui`):
      - home link, `via <strategy>` (plus ` · may be incomplete` when not complete) and `rule: <Rule.Name>`;
      - toggle to the other view (same url and strategy), `Source` link, and `Try:` links `{origin}/read?url=…&view=…&strategy=<s>` for each known strategy;
      - a `<details>` element listing the attempts.
    - **Reader**: `article.Extract(..., &Links{..., View: "reader"})` rendered into `reader.html`.
      - Styles: `max-width:42rem`, system font, `font-size:1.125rem;line-height:1.6`, `img,video,iframe{max-width:100%;height:auto}`, dark mode via `prefers-color-scheme`, and a viewport meta.
      - Header `Content-Security-Policy: default-src 'none'; img-src * data: blob:; media-src *; style-src 'unsafe-inline'; frame-src https:; base-uri 'none'; form-action 'self'`.
    - **Original**: `article.Original(..., banner)`. When not `KeepScripts`, add `Content-Security-Policy: script-src 'none'; object-src 'none'`.
    - Both views: `Content-Type: text/html; charset=utf-8`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`.
  - **`GET /share?url=&text=&title=`**: take the `url` param, else the first `https?://\S+` in `text`, else in `title`. None → 400 index `No link found in shared content`. Otherwise 303 to `/read?url=<escaped>`.
  - **Prefix route, `GET /` catch-all** (only non-root paths reach it):
    - `ServeMux` cleans `//`, so `targetFromPath(escapedPath, rawQuery string) (string, bool)` strips the leading `/`, requires `^(https?):/+` and replaces it with `$1://`, applies `url.PathUnescape`, and appends `"?"+rawQuery` if non-empty.
    - No match → 404. Match → 303 to `/read?url=<escaped>`.
  - **`GET /manifest.webmanifest`** (`application/manifest+json`): `{"name":"Read the Room","short_name":"ReadRoom","start_url":"/","scope":"/","display":"standalone","background_color":"#111827","theme_color":"#6d28d9","icons":[{"src":"/icon-192.png","sizes":"192x192","type":"image/png"},{"src":"/icon-512.png","sizes":"512x512","type":"image/png","purpose":"any maskable"}],"share_target":{"action":"/share","method":"GET","params":{"title":"title","text":"text","url":"url"}}}`.
  - **`GET /sw.js`** (`text/javascript`): `self.addEventListener('install',()=>self.skipWaiting());self.addEventListener('fetch',()=>{});`.
  - **`GET /scriptlets.js`** (`application/javascript; charset=utf-8`): the embedded `internal/scriptlets/scriptlets.min.js`, `Cache-Control: public, max-age=86400`.
  - **`GET /icon-192.png`, `/icon-512.png`, `/favicon.ico`** (favicon serves the 192 PNG):
    - Generated at startup with `image/png`: background `#6d28d9`; two white rails at 30% and 70% width (8% wide, 15%–85% height) and four 6%-tall rungs evenly spaced between them.
    - No asset exists, and generating avoids binary files in the repo.

### 8. Container
- `Dockerfile`:
  - Stage 1: `golang:1.27-alpine`, `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /readtheroom .`.
  - Stage 2: `gcr.io/distroless/static-debian12`, `WORKDIR /app`; copy the binary and `config/rules.json` to `/app/config/rules.json`; `ENV READTHEROOM_DATA=/app/data`; `EXPOSE 8080`; `ENTRYPOINT ["/readtheroom"]`.
- `compose.yaml`:
  - Service `readtheroom`: `build: .`, `container_name: readtheroom`, `ports: ["8080:8080"]`, `restart: unless-stopped`.
  - `volumes: ["./config:/app/config:ro", "./data:/app/data"]`. The config is a directory mount so rename-on-save editors still hot-reload; data is writable for upstream lists.
- README: running, env vars/flags, mobile setup (share target needs HTTPS, e.g. `tailscale serve` or Caddy), `rules.json` reference (profiles, strategies, layering and precedence from step 1, update sources and types, `scriptlets` as `{"name":…,"args":[…]}` which only run in Original view with `keep_scripts: true`), the uBO GPL-3.0 attribution and re-vendoring note, and the manual update options (button, `readtheroom -update`, `podman exec readtheroom /readtheroom -update`).

## Critical files & anchors
- `internal/rules/rules.go` `Match`: layer order and the prefer/strategies precedence; the most subtle logic.
- `internal/update/bpc.go` `sitesJSToJSON`: literal extraction plus goja, which depends on the `sites.js` layout described in Context.
- `internal/fetch/fetch.go` `waybackRawURL`, `IsChallenge`, dialer `Control`.
- `internal/article/article.go` `rewriteLinks`: absolute `{origin}/read?...` links, needed because of the injected `<base>`.
- `internal/article/article.go` scriptlet injection and `internal/scriptlets/scriptlets.go`: ordering after `<base>`/before page scripts, JSON escaping, unknown-name skip.
- `internal/scriptlets/scriptlets.min.js`: third-party GPL-3.0, never edit; regenerate from the pinned uBO tag and keep the header.
- `internal/server/server.go` `targetFromPath`: undoes ServeMux's `//` cleaning.

## Verification
All commands run from `/home/laurensarendbosscher/dev/bypasser` with `export PATH=$HOME/.local/go/bin:$PATH`.

1. `go mod tidy && go vet ./... && go test ./...`, with these tests:
   - **`internal/rules`**:
     - Shipped `config/rules.json` parses.
     - Precedence: defaults strategies `[googlebot,browser,archive_org]`; an upstream site `x.com` with `prefer:"browser"` and remove `.a`; upstream generic remove `.g`; a local site `x.com` with remove `.b` and `referer:""`. `Match("www.x.com")` → strategies `[browser,googlebot,archive_org]`, remove `[.g,.a,.b]`, referer `""`, Name = local name.
     - A local site with `strategies` set suppresses an upstream `prefer`.
     - Scriptlets layer like other lists: generic `set` survives, a site-level duplicate of the same name+args appears once, and a different arg list appends.
     - `Parse` rejects a scriptlet name `Upper` and an arg containing a control character.
     - Longest suffix wins (`a.com` vs `news.a.com` for host `x.news.a.com`), and `notx.com` gets the defaults.
     - `Parse` errors for: an unknown strategy, a bad regex, a bad selector, an unknown JSON key, a profile named `archive_org`, a duplicate local domain, and a source with an unknown type.
   - **`internal/update`**:
     - `sitesJSToJSON` on a fixture `var defaultSites = {\n "A": {\n domain: "###_g",\n group: ["dn.se","hd.se"],\n useragent: "googlebot",\n block_regex: /\.piano\.io\//\n },\n "B": {\n domain: "bt.no",\n useragent: "facebookbot"\n },\n "C": {\n domain: "ft.com",\n referer: "google",\n cs_code: [{"hide_elem": "div.ad"}]\n }\n}\nvar ext_api = chrome || browser;` produces JSON without evaluating `ext_api`. Converting it yields sites `bt.no` (prefer facebookbot), `dn.se`/`hd.se` (prefer googlebot, block_regex `\.piano\.io\/`) and `ft.com` (referer `https://www.google.com/`, remove `div.ad`).
     - `{domain}` substitution with group `["a.com","b.com"]` → `(?:a\.com|b\.com)`.
     - A string `cs_code` with nested `elems` + `rm_attrib:"style"` maps to unhide.
     - `convertUBO` on lines `nytimes.com##.magnoliaDock`, `##.subscriber-ad`, `richmond.com##+js(acis, $, reg-wall)`, `x.com##+js(rpnt, script, /a/, 'c,d', sedCount, 1)`, `*##html:has(>head) .x:style(display:block!important)`, `||piano.io^$third-party`, `/x$domain=a.com`, `~a.com##.y` → nytimes.com site with `.magnoliaDock`, generic `.subscriber-ad`, `richmond.com` scriptlet `acis` args `["$","reg-wall"]`, `x.com` scriptlet `rpnt` args `["script","/a/","c,d","sedCount","1"]` (the quoted comma stays in one arg), a block regex matching `https://cdn.piano.io/x.js` and not `https://notpiano.io/`, and 3 skipped lines.
     - Updater against `httptest`: a zip built in-test containing `pkg/sites.js` plus a working `ubo` source → files written, and `Store.Current().Match("dn.se")` has googlebot first. Then the ubo endpoint returns 500 and `Run` runs again → that source's error is recorded and its previous file is byte-identical. Calling `RunManual` twice → the second returns `*RateLimitError`.
   - **`internal/fetch`**:
     - `waybackRawURL("http://web.archive.org/web/20240101000000/https://x.com/a")` → `https://web.archive.org/web/20240101000000id_/https://x.com/a`.
     - `IsChallenge` is true for a 3 KB "Just a moment..." page and false for a 50 KB article with one marker.
     - With `AllowPrivate=false`, fetching an `httptest` URL fails with `blocked private address`.
     - The googlebot profile sends `X-Forwarded-For: 66.249.66.1` and its own Referer even when `rule.Referer` is facebook (header echo server, AllowPrivate=true).
   - **`internal/article`**:
     - A body `<noscript><img src=a.jpg></noscript>` becomes an `<img>`.
     - Remove and unhide selectors are applied.
     - Original with keep_scripts=false keeps only ld+json scripts, overwrites `<base href="/">`, rewrites a same-host link to `{Self}/read?url=…` and leaves a foreign link alone. keep_scripts=true drops only `script[src]` that match a block regex.
     - Original with keep_scripts=true and `Scriptlets` emits `{Self}/scriptlets.js` and the dispatcher right after `<base>`; keep_scripts=false emits neither; an arg containing `</script>` comes out as `\u003c/script\u003e`.
     - An ld+json `@graph` `articleBody` longer than the visible text becomes `ContentHTML`.
   - **`internal/bypass`** (`httptest`, AllowPrivate=true, in-memory rules): the server returns a 300-char page with "Subscribe to continue" unless the UA contains `Googlebot`, in which case it returns a 3000-char article.
     - `["browser","googlebot"]` → googlebot result, `Complete`, `Attempts[0].Status` starts with `weak`.
     - Both weak → the longest is returned with `Complete=false`.
     - Both 403 → `*FailedError` with 2 attempts.
   - **`internal/server`**:
     - `targetFromPath("/https:/x.com/a/b", "id=1")` → `https://x.com/a/b?id=1`; `/foo` → no match.
     - Share `text="Read this https://x.com/a"` → `https://x.com/a`.
     - `GET /scriptlets.js` → 200, `application/javascript`, body starts with the uBO GPL header.
2. **Live update smoke**:
   - `go run . -update` prints `bpc: ok (N sites)` with N > 500, `bpc-updates: ok (…)` and `antipaywall: ok (…)`.
   - `grep -A6 '"dn.se"' data/upstream/bpc.json` shows `"prefer": "googlebot"`; the `bt.no` entry shows `facebookbot`; the `ft.com` entry shows `"referer": "https://www.google.com/"`; `grep -c magnoliaDock data/upstream/antipaywall.json` is ≥ 1.
3. **Local end-to-end** (fixture):
   - Create `/tmp/bp-fixture/article.html` with a `<title>`, a `div.paywall-modal`, an inline `<script>document.body.innerHTML=''</script>`, a 2000+ char `<article><p>…`, and `<noscript><img src="/pic.jpg"></noscript>`. Serve it with `python3 -m http.server 9000 -d /tmp/bp-fixture`, then run `READTHEROOM_ALLOW_PRIVATE=1 go run . -addr :8080`.
   - `curl -s 'localhost:8080/read?url=http://127.0.0.1:9000/article.html'` → article text and `via googlebot`.
   - `curl -s '…&view=original'` → no `<script>`, `<base href="http://127.0.0.1:9000/article.html">`, `<img src="/pic.jpg">` outside noscript, and the banner.
   - `curl -siL 'localhost:8080/http://127.0.0.1:9000/article.html'` → 301, then 303 to `/read?url=http%3A%2F%2F127.0.0.1%3A9000%2Farticle.html`.
   - `curl -si 'localhost:8080/share?text=look%20http://127.0.0.1:9000/article.html'` → 303 to `/read?url=…`.
   - `curl -s localhost:8080/` → the status table lists bpc, bpc-updates and antipaywall with site counts.
   - `curl -si -X POST localhost:8080/update` → 303 to `/?updated=1`. An immediate second POST → 429 with body containing `try again in`.
   - `curl -s localhost:8080/manifest.webmanifest` contains `share_target`; `curl -s localhost:8080/icon-512.png | file -` reports a 512x512 PNG.
   - Add `<script>window.__page={paywall:true}</script>` to the fixture, add a local site for `127.0.0.1` with `"keep_scripts": true, "scriptlets": [{"name":"set","args":["__page__.paywall","false"]}]`, then `curl -s '…&view=original'` contains `/scriptlets.js` and `__page__.paywall`, while the reader view contains neither.
   - Hot reload: add a local site to `config/rules.json` → log `rules: reloaded`. Break the JSON → log `reload failed, keeping previous rules`, and requests still work. Restore the file.
4. **Live bypass smoke** (no private override): `curl -s 'localhost:8080/read?url=https://example.com/&strategy=archive_org'` → `via archive_org`. Also try one Medium article and one real paywalled article and report the strategy or attempts. Third-party outcomes are reported, not gated.
5. **Container**: `podman build -t readtheroom . && podman run --rm -p 8080:8080 -v ./config:/app/config:ro -v ./data:/app/data readtheroom`, then `curl -s localhost:8080/ | grep -c manifest` → 1, and `podman exec` with `/readtheroom -update` succeeds.

## Assumptions & contingencies
- No auth by user choice. The update button is protected only by the 5-minute rate limit. The SSRF guard still blocks private networks unless `READTHEROOM_ALLOW_PRIVATE=1`.
- Scriptlets run only in the Original view with `keep_scripts: true`; Reader never runs them. Because the document is served from our origin, `document.cookie`/`localStorage` scriptlets act on our origin (no-op for the publisher), `trusted-replace-*`/`no-fetch-if` are limited by CORS on publisher APIs, and only DOM/global scriptlets are reliable. Unknown names are skipped silently.
- The scripted Original view is same-origin by user choice: publisher and injected scripts can call the unauthenticated `POST /update` and use `/read` as a fetch proxy. Accepted; the 5-minute limit is the only guard.
- The vendored uBO bundle is GPL-3.0. Keep its header and `LICENSE.txt`, credit it in the README, and re-vendor on uBO updates; committing the bundle avoids a Node build step.
- If gitflic is unreachable at update time, that source keeps its previous file and the status table shows the error. No mirror fallback; users can change `url` in `rules.json`.
- If goja fails on a future `sites.js` syntax, the bpc source reports the error and keeps its previous file. Don't add a hand-written JS parser.
- If the `readability.FromDocument` signature differs in v2.1.3, use `readability.FromReader(strings.NewReader(rendered), base)` on the prepared tree rendered via `html.Render`.
- If `golang:1.27-alpine` fails to pull, use `golang:1.27`.
- If Chrome won't offer PWA install because of the no-op fetch handler, replace it with `self.addEventListener('fetch',e=>{if(e.request.mode==='navigate')e.respondWith(fetch(e.request))})`.
