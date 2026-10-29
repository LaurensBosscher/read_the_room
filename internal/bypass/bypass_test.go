package bypass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LaurensBosscher/read_the_room/internal/fetch"
	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

func storeFor(t *testing.T, strategies []string) *rules.Store {
	t.Helper()
	quoted, err := json.Marshal(strategies)
	if err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{
		"profiles": {
			"browser": {"User-Agent": "Mozilla/5.0"},
			"googlebot": {"User-Agent": "Googlebot/2.1"}
		},
		"referers": {},
		"updates": {},
		"defaults": {
			"strategies": %s,
			"referer": "",
			"random_ip": false,
			"keep_scripts": false,
			"ld_json": false,
			"min_text_length": 1000
		},
		"sites": []
	}`, quoted)
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := rules.NewStore(path, nil, filepath.Join(dir, "upstream"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCascadeFallsBackToGooglebot(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.UserAgent(), "Googlebot") {
			fmt.Fprint(w, "<html><body><article><p>"+strings.Repeat("hello ", 600)+"</p></article></body></html>")
			return
		}
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("subscribe ", 30)+"Subscribe to continue</p></body></html>")
	}))
	defer ts.Close()

	target, _ := url.Parse(ts.URL + "/a")
	b := &Bypasser{Rules: storeFor(t, []string{"browser", "googlebot"}), Fetcher: fetch.New(true)}
	res, err := b.Run(context.Background(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.Strategy != "googlebot" {
		t.Fatalf("complete=%v strategy=%q, want complete via googlebot", res.Complete, res.Strategy)
	}
	if len(res.Attempts) != 2 || !strings.HasPrefix(res.Attempts[0].Status, "weak") {
		t.Fatalf("attempts = %#v", res.Attempts)
	}
}

func TestCascadeReturnsBestWeak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("subscribe ", 30)+"Subscribe to continue</p></body></html>")
	}))
	defer ts.Close()

	target, _ := url.Parse(ts.URL + "/a")
	b := &Bypasser{Rules: storeFor(t, []string{"browser", "googlebot"}), Fetcher: fetch.New(true)}
	res, err := b.Run(context.Background(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatal("expected Complete=false")
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %#v", res.Attempts)
	}
}

func TestCascadeAllFail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer ts.Close()

	target, _ := url.Parse(ts.URL + "/a")
	b := &Bypasser{Rules: storeFor(t, []string{"browser", "googlebot"}), Fetcher: fetch.New(true)}
	_, err := b.Run(context.Background(), target, "")
	var failed *FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v, want *FailedError", err)
	}
	if len(failed.Attempts) != 2 {
		t.Fatalf("attempts = %#v", failed.Attempts)
	}
}

func TestRunCachesResult(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.Contains(r.UserAgent(), "Googlebot") {
			fmt.Fprint(w, "<html><body><article><p>"+strings.Repeat("hello ", 600)+"</p></article></body></html>")
			return
		}
		fmt.Fprint(w, "<html><body><p>Subscribe to continue</p></body></html>")
	}))
	defer ts.Close()

	target, _ := url.Parse(ts.URL + "/a")
	b := &Bypasser{Rules: storeFor(t, []string{"browser", "googlebot"}), Fetcher: fetch.New(true)}
	if _, err := b.Run(context.Background(), target, ""); err != nil {
		t.Fatal(err)
	}
	before := hits
	if _, err := b.Run(context.Background(), target, ""); err != nil {
		t.Fatal(err)
	}
	if hits != before {
		t.Fatalf("cache miss: hits %d -> %d", before, hits)
	}
}
