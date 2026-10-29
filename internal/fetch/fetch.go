// Package fetch retrieves paywalled pages through header profiles and
// third-party archives.
package fetch

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/rules"

	"golang.org/x/net/html/charset"
)

const (
	acceptHeader   = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	acceptLanguage = "en-US,en;q=0.9"

	defaultTimeout    = 20 * time.Second
	defaultArchiveOrg = "https://archive.org/wayback/available"
	maxBodyBytes      = 10 << 20
)

var (
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

	waybackRE = regexp.MustCompile(`^https?://web\.archive\.org/web/(\d+)/`)
	// archive.today snapshot pages look like https://archive.ph/AbCdE.
	archiveTodayRE = regexp.MustCompile(`^https?://archive\.[a-z]+/[A-Za-z0-9]{4,}$`)
)

// Fetcher retrieves pages through header profiles and archive mirrors.
type Fetcher struct {
	AllowPrivate  bool
	ArchiveOrgAPI string
	Timeout       time.Duration

	transport *http.Transport
}

// Response is one fetched page.
type Response struct {
	HTML     string
	BaseURL  *url.URL
	FinalURL *url.URL
}

// New returns a Fetcher. Unless allowPrivate is set, connections to
// loopback, private, link-local, multicast, unspecified and carrier-grade
// NAT addresses are refused.
func New(allowPrivate bool) *Fetcher {
	f := &Fetcher{
		AllowPrivate:  allowPrivate,
		ArchiveOrgAPI: defaultArchiveOrg,
		Timeout:       defaultTimeout,
	}
	f.transport = newTransport(f.control)
	return f
}

func newTransport(control func(network, address string, c syscall.RawConn) error) *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: control,
	}
	return &http.Transport{
		Proxy:       nil,
		DialContext: dialer.DialContext,
	}
}

func (f *Fetcher) control(network, address string, _ syscall.RawConn) error {
	if f.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	if blockedIP(ip) {
		return fmt.Errorf("blocked private address %s", ip)
	}
	return nil
}

func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnatPrefix.Contains(ip)
}

// randomPublicIPv4 returns a random address that passes blockedIP.
func randomPublicIPv4() (string, error) {
	var b [4]byte
	for range 1000 {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if ip := netip.AddrFrom4(b); !blockedIP(ip) {
			return ip.String(), nil
		}
	}
	return "", errors.New("could not generate a public IPv4 address")
}

// IsChallenge reports whether html looks like an anti-bot interstitial.
// A single marker is enough on short pages; two are required on longer ones.
func IsChallenge(html string, markers []string) bool {
	head := html
	if len(head) > 8000 {
		head = head[:8000]
	}
	head = strings.ToLower(head)
	hits := 0
	for _, marker := range markers {
		if marker == "" {
			continue
		}
		if strings.Contains(head, strings.ToLower(marker)) {
			hits++
		}
	}
	return (hits >= 1 && len(html) < 20000) || hits >= 2
}

// waybackRawURL converts a Wayback replay URL into its raw id_ variant.
func waybackRawURL(s string) string {
	if strings.Contains(s, "id_/") {
		if strings.HasPrefix(s, "http://") {
			return "https://" + strings.TrimPrefix(s, "http://")
		}
		return s
	}
	return waybackRE.ReplaceAllString(s, "https://web.archive.org/web/${1}id_/")
}

func (f *Fetcher) attemptTimeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaultTimeout
}

// request performs one GET. The returned cancel func must be called after the
// body has been consumed so the per-attempt timeout stays alive while reading.
func (f *Fetcher) request(ctx context.Context, rawURL string, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, f.attemptTimeout())

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("Accept-Language", acceptLanguage)

	jar, _ := cookiejar.New(nil)
	transport := f.transport
	if transport == nil {
		transport = newTransport(f.control)
	}
	client := &http.Client{Transport: transport, Jar: jar}

	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, cancel, nil
}

func (f *Fetcher) get(ctx context.Context, rawURL string, headers map[string]string, markers []string) (*Response, error) {
	resp, cancel, err := f.request(ctx, rawURL, headers)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	mediaType := ""
	if contentType != "" {
		mt, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return nil, fmt.Errorf("not an HTML page (%s)", contentType)
		}
		mediaType = mt
	}

	body := bufio.NewReader(io.LimitReader(resp.Body, maxBodyBytes))
	switch mediaType {
	case "text/html", "application/xhtml+xml":
	case "":
		peek, _ := body.Peek(512)
		detected := http.DetectContentType(peek)
		if !strings.HasPrefix(detected, "text/html") {
			return nil, fmt.Errorf("not an HTML page (%s)", detected)
		}
	default:
		return nil, fmt.Errorf("not an HTML page (%s)", mediaType)
	}

	reader, err := charset.NewReader(body, contentType)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	html := string(raw)
	if IsChallenge(html, markers) {
		return nil, errors.New("anti-bot challenge page")
	}
	return &Response{HTML: html, FinalURL: resp.Request.URL}, nil
}

func profileHeaders(set *rules.Set, rule rules.Rule, name string) (map[string]string, error) {
	profile := set.File.Profiles[name]
	headers := make(map[string]string, len(profile)+2)
	for k, v := range profile {
		headers[http.CanonicalHeaderKey(k)] = v
	}
	if _, ok := headers["Referer"]; !ok && rule.Referer != "" {
		headers["Referer"] = rule.Referer
	}
	if _, ok := headers["X-Forwarded-For"]; !ok && rule.RandomIP {
		ip, err := randomPublicIPv4()
		if err != nil {
			return nil, err
		}
		headers["X-Forwarded-For"] = ip
	}
	return headers, nil
}

// Fetch retrieves target using the named profile or builtin strategy.
func (f *Fetcher) Fetch(ctx context.Context, set *rules.Set, rule rules.Rule, strategy string, target *url.URL) (*Response, error) {
	if set == nil || set.File == nil {
		return nil, errors.New("nil rule set")
	}
	if target == nil {
		return nil, errors.New("nil target")
	}
	markers := set.File.ChallengeMarkers

	switch strategy {
	case rules.StrategyArchiveOrg:
		return f.archiveOrg(ctx, set, rule, target, markers)
	case rules.StrategyArchiveToday:
		return f.archiveToday(ctx, set, rule, target, markers)
	case rules.StrategyFreedium:
		return f.freedium(ctx, set, rule, target, markers)
	}

	if _, ok := set.File.Profiles[strategy]; !ok {
		return nil, fmt.Errorf("unknown strategy %s", strategy)
	}
	headers, err := profileHeaders(set, rule, strategy)
	if err != nil {
		return nil, err
	}
	resp, err := f.get(ctx, target.String(), headers, markers)
	if err != nil {
		return nil, err
	}
	resp.BaseURL = resp.FinalURL
	return resp, nil
}

type archiveOrgResponse struct {
	ArchivedSnapshots struct {
		Closest struct {
			Available bool   `json:"available"`
			URL       string `json:"url"`
		} `json:"closest"`
	} `json:"archived_snapshots"`
}

func (f *Fetcher) archiveOrg(ctx context.Context, set *rules.Set, rule rules.Rule, target *url.URL, markers []string) (*Response, error) {
	headers, err := profileHeaders(set, rule, "browser")
	if err != nil {
		return nil, err
	}
	api := f.ArchiveOrgAPI + "?url=" + url.QueryEscape(target.String())
	resp, cancel, err := f.request(ctx, api, headers)
	if err != nil {
		return nil, err
	}
	var snapshot archiveOrgResponse
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&snapshot)
	resp.Body.Close()
	cancel()
	if decodeErr != nil {
		return nil, decodeErr
	}
	closest := snapshot.ArchivedSnapshots.Closest
	if !closest.Available || closest.URL == "" {
		return nil, errors.New("no archive.org snapshot")
	}
	page, err := f.get(ctx, waybackRawURL(closest.URL), headers, markers)
	if err != nil {
		return nil, err
	}
	page.BaseURL = target
	return page, nil
}

func (f *Fetcher) archiveToday(ctx context.Context, set *rules.Set, rule rules.Rule, target *url.URL, markers []string) (*Response, error) {
	headers, err := profileHeaders(set, rule, "browser")
	if err != nil {
		return nil, err
	}
	mirrors := set.File.ArchiveTodayMirrors
	var lastErr error
	errs := 0
	for _, mirror := range mirrors {
		rawURL := "https://" + mirror + "/newest/" + target.String()
		resp, err := f.get(ctx, rawURL, headers, markers)
		if err != nil {
			lastErr = err
			errs++
			continue
		}
		if !archiveTodayRE.MatchString(resp.FinalURL.String()) {
			continue
		}
		head := resp.HTML
		if len(head) > 5000 {
			head = head[:5000]
		}
		if strings.Contains(strings.ToLower(head), "no results") {
			continue
		}
		resp.BaseURL = resp.FinalURL
		return resp, nil
	}
	if len(mirrors) > 0 && errs == len(mirrors) && lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no archive.today snapshot")
}

func (f *Fetcher) freedium(ctx context.Context, set *rules.Set, rule rules.Rule, target *url.URL, markers []string) (*Response, error) {
	headers, err := profileHeaders(set, rule, "browser")
	if err != nil {
		return nil, err
	}
	stripped := *target
	stripped.RawQuery = ""
	stripped.ForceQuery = false
	stripped.Fragment = ""
	rawTarget := stripped.String()
	for _, mirror := range set.File.FreediumMirrors {
		resp, err := f.get(ctx, "https://"+mirror+"/"+rawTarget, headers, markers)
		if err != nil {
			continue
		}
		if len(resp.HTML) < 2000 ||
			strings.Contains(resp.HTML, "ARTICLE_NOT_FOUND") ||
			(!strings.Contains(resp.HTML, "main-content") && !strings.Contains(resp.HTML, "<main")) {
			continue
		}
		resp.BaseURL = resp.FinalURL
		return resp, nil
	}
	return nil, errors.New("no freedium copy")
}
