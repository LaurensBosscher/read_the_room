package update

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(entries) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.WriteString(w, entries[name]); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

const uboBody = `nytimes.com##.magnoliaDock
##.subscriber-ad
||piano.io^$third-party
`

func TestUpdater(t *testing.T) {
	zipData := buildZip(t, map[string]string{
		"pkg/sites.js": bpcFixture,
	})
	var uboOK atomic.Bool
	uboOK.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/bpc.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipData)
	})
	mux.HandleFunc("/antipaywall.txt", func(w http.ResponseWriter, r *http.Request) {
		if !uboOK.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, uboBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	upstreamDir := filepath.Join(dir, "upstream")
	rulesPath := filepath.Join(dir, "rules.json")
	doc := `{
	 "profiles": {
	   "browser": {"User-Agent": "Mozilla/5.0"},
	   "googlebot": {"User-Agent": "Googlebot/2.1"},
	   "facebookbot": {"User-Agent": "facebookexternalhit/1.1"}
	 },
	 "defaults": {
	   "strategies": ["browser"],
	   "referer": "",
	   "random_ip": false,
	   "keep_scripts": false,
	   "ld_json": false,
	   "min_text_length": 0
	 },
	 "updates": {
	   "interval": "0",
	   "sources": [
	     {"name": "bpc", "type": "bpc_zip", "url": "` + srv.URL + `/bpc.zip"},
	     {"name": "antipaywall", "type": "ubo", "url": "` + srv.URL + `/antipaywall.txt"}
	   ]
	 }
	}`
	if err := os.WriteFile(rulesPath, []byte(doc), 0o644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	store, err := rules.NewStore(rulesPath, nil, upstreamDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	u := New(store, dir)

	if _, err := u.RunManual(context.Background()); err != nil {
		t.Fatalf("first RunManual: %v", err)
	}
	if _, err := u.RunManual(context.Background()); !isRateLimit(err) {
		t.Fatalf("second RunManual: want *RateLimitError, got %v", err)
	}

	bpcPath := filepath.Join(upstreamDir, "bpc.json")
	uboPath := filepath.Join(upstreamDir, "antipaywall.json")
	bpcBefore, err := os.ReadFile(bpcPath)
	if err != nil {
		t.Fatalf("bpc.json: %v", err)
	}
	uboBefore, err := os.ReadFile(uboPath)
	if err != nil {
		t.Fatalf("antipaywall.json: %v", err)
	}

	rule := store.Current().Match("dn.se")
	if len(rule.Strategies) == 0 || rule.Strategies[0] != "googlebot" {
		t.Fatalf("dn.se strategies = %v, want googlebot first", rule.Strategies)
	}

	uboOK.Store(false)
	errs, err := u.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if errs["bpc"] != nil {
		t.Errorf("bpc error on second run: %v", errs["bpc"])
	}
	if errs["antipaywall"] == nil {
		t.Fatal("antipaywall: want error on 500, got none")
	} else if !strings.Contains(errs["antipaywall"].Error(), "HTTP 500") {
		t.Errorf("antipaywall error = %v, want HTTP 500", errs["antipaywall"])
	}
	uboAfter, err := os.ReadFile(uboPath)
	if err != nil {
		t.Fatalf("antipaywall.json after failed run: %v", err)
	}
	if !bytes.Equal(uboBefore, uboAfter) {
		t.Error("antipaywall.json changed after a failed update")
	}
	bpcAfter, err := os.ReadFile(bpcPath)
	if err != nil {
		t.Fatalf("bpc.json after second run: %v", err)
	}
	if len(bpcAfter) == 0 || bytes.Equal(bpcAfter, nil) {
		t.Error("bpc.json empty")
	}
	_ = bpcBefore

	status := u.Status()
	if len(status) != 2 {
		t.Fatalf("Status() = %+v, want 2 entries", status)
	}
	for _, st := range status {
		if st.Name == "antipaywall" {
			if !strings.Contains(st.LastError, "HTTP 500") {
				t.Errorf("antipaywall LastError = %q, want HTTP 500", st.LastError)
			}
		} else if st.LastError != "" {
			t.Errorf("bpc LastError = %q, want empty", st.LastError)
		}
		if st.Sites == 0 || st.UpdatedAt.IsZero() {
			t.Errorf("%s status = %+v, want sites and UpdatedAt", st.Name, st)
		}
	}
}

func TestUpdaterBusy(t *testing.T) {
	zipData := buildZip(t, map[string]string{"pkg/sites.js": bpcFixture})
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Write(zipData)
			return
		}
		once.Do(func() { close(inFlight) })
		<-release
		io.WriteString(w, uboBody)
	}))
	defer srv.Close()

	dir := t.TempDir()
	upstreamDir := filepath.Join(dir, "upstream")
	rulesPath := filepath.Join(dir, "rules.json")
	doc := `{
	 "profiles": {"browser": {"User-Agent": "Mozilla/5.0"}},
	 "defaults": {"strategies": ["browser"], "referer": "", "random_ip": false, "keep_scripts": false, "ld_json": false, "min_text_length": 0},
	 "updates": {"interval": "0", "sources": [
	   {"name": "bpc", "type": "bpc_zip", "url": "` + srv.URL + `/bpc.zip"},
	   {"name": "antipaywall", "type": "ubo", "url": "` + srv.URL + `/antipaywall.txt"}
	 ]}
	}`
	if err := os.WriteFile(rulesPath, []byte(doc), 0o644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	store, err := rules.NewStore(rulesPath, nil, upstreamDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	u := New(store, dir)

	// The first run blocks in the ubo download; a concurrent Run must be ErrBusy.
	done := make(chan error, 1)
	go func() {
		_, err := u.Run(context.Background())
		done <- err
	}()
	<-inFlight

	if _, err := u.Run(context.Background()); !errors.Is(err, ErrBusy) {
		close(release)
		<-done
		t.Fatalf("concurrent Run: got %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func isRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl) && rl.Wait > 0 && rl.Wait <= 5*time.Minute
}
