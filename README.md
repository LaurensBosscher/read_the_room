# Read the Room

Self-hosted paywall bypasser: paste an article URL, the server tries a cascade of
known tricks (bot header profiles, referer, X-Forwarded-For, archive.org,
archive.today, Freedium for Medium), cleans the HTML, and shows a reader view with
a toggle to the cleaned original page.

## Running

```sh
export PATH=$HOME/.local/go/bin:$PATH
go run .
```

Open <http://localhost:8080>. The first start schedules an upstream rules update
(BPC and the antipaywall uBO list); you can also trigger it with the button,
`go run . -update`, or `podman exec readtheroom /readtheroom -update`.

Container:

```sh
podman build -t readtheroom .
podman run --rm -p 8080:8080 -v ./config:/app/config:ro -v ./data:/app/data readtheroom
```

`compose.yaml` does the same with a writable `./data` for upstream lists; keep
`./config` a directory mount so rename-on-save editors still hot-reload.

## Configuration

Flags override environment variables:

| Flag | Env | Default |
|---|---|---|
| `-addr` | `READTHEROOM_ADDR` | `:8080` |
| `-rules` | `READTHEROOM_RULES` | `config/rules.json` |
| `-data` | `READTHEROOM_DATA` | `data` |
| `-update` | — | update and exit |

`READTHEROOM_ALLOW_PRIVATE=1` disables the SSRF guard (needed only for local
fixtures). There is no authentication.

## Mobile / share target

Install the site as a PWA. The share target and service worker need HTTPS, e.g.
`tailscale serve` or Caddy in front of the server. Without installing, use the
bookmarklet on the index page, or prefix a URL: `https://your-host/https://…`.

## `rules.json`

- `profiles`: strategy name → request headers. `browser` is required; a profile
  must have a `User-Agent`. `googlebot`, `bingbot`, `facebookbot`, `twitterbot`
  are shipped.
- `referers`: name → http(s) URL, usable as `"referer": "google"` or as a literal
  URL. `""` means no Referer.
- `defaults`: required `strategies`, `referer`, `random_ip`, `keep_scripts`,
  `ld_json`, `min_text_length`.
- `sites`: `{name, domains, ...spec}`. The longest matching domain suffix wins.

Layering: defaults → each upstream `generic` → each upstream site → the local
site, in source order. Scalars override, lists append, `prefer` moves a strategy
to the front unless a local site sets its own `strategies`.

`scriptlets` are uBO `+js(...)` rules, e.g.
`{"name":"set","args":["__page__.isPremium","false"]}`. They are only injected in
the **Original** view when the rule also sets `"keep_scripts": true`; the Reader
never runs page scripts. Unknown scriptlet names are skipped. Upstream scriptlet
rules therefore need a local site such as:

```json
{"name": "Example", "domains": ["example.com"], "keep_scripts": true}
```

Caveats, because the page is served from our origin: cookie/localStorage
scriptlets act on our origin (no-op for the publisher) and
`trusted-replace-*`/`no-fetch-if` are limited by CORS. Scripted pages run
publisher and injected JavaScript same-origin, so it can reach the
unauthenticated `POST /update` and `/read` endpoints.

## Upstream updates

`updates.interval` is a Go duration (`""` = 24h, `"0"` disables the schedule) and
`updates.sources` are `{name, type, url}` with type `bpc_zip`, `bpc_json`, or
`ubo`. Converted lists are written atomically to `data/upstream/<name>.json` and
hot-reloaded. A failed download keeps the previous file and shows the error on
the index page. Manual updates are rate-limited to one per five minutes.

## Licensing

`internal/scriptlets/scriptlets.min.js` is a bundle of uBlock Origin 1.75.0
scriptlets, licensed GPL-3.0 (see `internal/scriptlets/vendor/uBO-LICENSE.txt`).
The bundle is committed; `internal/scriptlets/vendor/README.md` records the
rebuild command. Re-vendor when uBO updates.
