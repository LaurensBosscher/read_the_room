package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

func testSet(profiles map[string]map[string]string, markers ...string) *rules.Set {
	return &rules.Set{File: &rules.File{
		Profiles:         profiles,
		ChallengeMarkers: markers,
	}}
}

func TestWaybackRawURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{
			"http://web.archive.org/web/20240101000000/https://example.com/a",
			"https://web.archive.org/web/20240101000000id_/https://example.com/a",
		},
		{
			"https://web.archive.org/web/20240101000000/https://example.com/a",
			"https://web.archive.org/web/20240101000000id_/https://example.com/a",
		},
		{
			"https://web.archive.org/web/20240101000000id_/https://example.com/a",
			"https://web.archive.org/web/20240101000000id_/https://example.com/a",
		},
		{
			"http://web.archive.org/web/20240101000000id_/https://example.com/a",
			"https://web.archive.org/web/20240101000000id_/https://example.com/a",
		},
		{
			"https://example.com/a",
			"https://example.com/a",
		},
	}
	for _, tc := range tests {
		if got := waybackRawURL(tc.in); got != tc.want {
			t.Errorf("waybackRawURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsChallenge(t *testing.T) {
	markers := []string{"just a moment...", "cf-challenge"}

	challenge := strings.Repeat("x", 3*1024) + "Just a moment..."
	if !IsChallenge(challenge, markers) {
		t.Errorf("IsChallenge(3KB with one marker) = false, want true")
	}

	oneMarker := "just a moment..." + strings.Repeat("x", 50*1024)
	if IsChallenge(oneMarker, markers) {
		t.Errorf("IsChallenge(50KB with one marker) = true, want false")
	}

	twoMarkers := "just a moment... cf-challenge" + strings.Repeat("x", 50*1024)
	if !IsChallenge(twoMarkers, markers) {
		t.Errorf("IsChallenge(50KB with two markers) = false, want true")
	}

	plain := strings.Repeat("x", 3*1024) + "an ordinary article body"
	if IsChallenge(plain, markers) {
		t.Errorf("IsChallenge(plain page) = true, want false")
	}
}

func htmlServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAllowPrivateBlocksLoopback(t *testing.T) {
	srv := htmlServer(t, "<html><body>hello</body></html>")
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	set := testSet(map[string]map[string]string{
		"browser": {"User-Agent": "fetch-test"},
	})

	_, err = New(false).Fetch(context.Background(), set, rules.Rule{}, "browser", target)
	if err == nil || !strings.Contains(err.Error(), "blocked private address") {
		t.Fatalf("AllowPrivate=false: got %v, want error containing %q", err, "blocked private address")
	}

	f := New(true)
	f.Timeout = 5 * time.Second
	resp, err := f.Fetch(context.Background(), set, rules.Rule{}, "browser", target)
	if err != nil {
		t.Fatalf("AllowPrivate=true: %v", err)
	}
	if !strings.Contains(resp.HTML, "hello") {
		t.Errorf("AllowPrivate=true: HTML = %q, want page contents", resp.HTML)
	}
}

func TestGooglebotProfileHeaders(t *testing.T) {
	type captured struct {
		userAgent string
		referer   string
		xff       string
		accept    string
		language  string
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{
			userAgent: r.Header.Get("User-Agent"),
			referer:   r.Header.Get("Referer"),
			xff:       r.Header.Get("X-Forwarded-For"),
			accept:    r.Header.Get("Accept"),
			language:  r.Header.Get("Accept-Language"),
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>ok</body></html>")
	}))
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	set := testSet(map[string]map[string]string{
		"browser": {"User-Agent": "fetch-test"},
		"googlebot": {
			"User-Agent":      "googlebot-test",
			"Referer":         "https://www.google.com/",
			"X-Forwarded-For": "66.249.66.1",
		},
	})
	rule := rules.Rule{Referer: "https://www.facebook.com/", RandomIP: true}

	f := New(true)
	f.Timeout = 5 * time.Second
	resp, err := f.Fetch(context.Background(), set, rule, "googlebot", target)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.HTML == "" {
		t.Errorf("Fetch returned empty HTML")
	}
	if resp.BaseURL.String() != resp.FinalURL.String() {
		t.Errorf("profile strategy: BaseURL = %s, want FinalURL %s", resp.BaseURL, resp.FinalURL)
	}
	if resp.FinalURL.String() != target.String() {
		t.Errorf("FinalURL = %s, want %s", resp.FinalURL, target)
	}

	headers := <-got
	if headers.xff != "66.249.66.1" {
		t.Errorf("X-Forwarded-For = %q, want the profile's 66.249.66.1", headers.xff)
	}
	if headers.referer != "https://www.google.com/" {
		t.Errorf("Referer = %q, want the profile's own referer, not the rule's facebook one", headers.referer)
	}
	if headers.userAgent != "googlebot-test" {
		t.Errorf("User-Agent = %q, want profile value", headers.userAgent)
	}
	if headers.accept != acceptHeader {
		t.Errorf("Accept = %q, want %q", headers.accept, acceptHeader)
	}
	if headers.language != acceptLanguage {
		t.Errorf("Accept-Language = %q, want %q", headers.language, acceptLanguage)
	}
}
