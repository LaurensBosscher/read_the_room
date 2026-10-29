// Package update downloads the configured upstream rule lists and converts
// them into <data>/upstream/<name>.json files.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

const (
	maxDownload      = 32 << 20
	manualRateWindow = 5 * time.Minute
	defaultInterval  = 24 * time.Hour
)

// Updater downloads and converts the upstream lists configured in the rules
// file into Dir.
type Updater struct {
	Store  *rules.Store
	Dir    string       // <data>/upstream
	Client *http.Client // Timeout 30s

	runMu sync.Mutex // held for the duration of a run

	mu      sync.Mutex // guards lastRun and errs
	lastRun time.Time
	errs    map[string]string
}

// ErrBusy is returned by Run when another run is already in progress.
var ErrBusy = errors.New("update already running")

// RateLimitError is returned by RunManual when a run started less than five
// minutes ago.
type RateLimitError struct {
	Wait time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("update rate limited; try again in %s", e.Wait.Round(time.Second))
}

// SourceStatus describes one configured upstream source.
type SourceStatus struct {
	Name      string
	Type      string
	Sites     int
	UpdatedAt time.Time
	LastError string
}

// New returns an Updater writing into dataDir.
func New(store *rules.Store, dataDir string) *Updater {
	return &Updater{
		Store:  store,
		Dir:    filepath.Join(dataDir, "upstream"),
		Client: &http.Client{Timeout: 30 * time.Second},
		errs:   map[string]string{},
	}
}

// Run downloads and converts every configured source. A failing source keeps
// its previous file; the returned map holds the per-source errors.
func (u *Updater) Run(ctx context.Context) (map[string]error, error) {
	if !u.runMu.TryLock() {
		return nil, ErrBusy
	}
	defer u.runMu.Unlock()

	u.mu.Lock()
	u.lastRun = time.Now()
	u.mu.Unlock()

	if err := os.MkdirAll(u.Dir, 0o755); err != nil {
		return nil, err
	}

	set := u.Store.Current()
	errs := make(map[string]error)
	for _, src := range set.File.Updates.Sources {
		n, err := u.updateSource(ctx, set, src)
		u.mu.Lock()
		if err != nil {
			errs[src.Name] = err
			if u.errs == nil {
				u.errs = map[string]string{}
			}
			u.errs[src.Name] = err.Error()
			log.Printf("update: %s failed: %v", src.Name, err)
		} else {
			delete(u.errs, src.Name)
			log.Printf("update: %s ok (%d sites)", src.Name, n)
		}
		u.mu.Unlock()
	}
	if err := u.Store.Reload(); err != nil {
		log.Printf("update: reload failed: %v", err)
	}
	return errs, nil
}

// RunManual is Run with a five minute rate limit.
func (u *Updater) RunManual(ctx context.Context) (map[string]error, error) {
	u.mu.Lock()
	last := u.lastRun
	u.mu.Unlock()

	if !last.IsZero() {
		if elapsed := time.Since(last); elapsed < manualRateWindow {
			return nil, &RateLimitError{Wait: manualRateWindow - elapsed}
		}
	}
	return u.Run(ctx)
}

// Start runs the updater now when a configured upstream is missing or stale,
// then on the configured interval until ctx is done. A zero interval disables
// the schedule; manual runs still work.
func (u *Updater) Start(ctx context.Context) {
	set := u.Store.Current()
	interval := defaultInterval
	if raw := set.File.Updates.Interval; raw == "" {
		// default
	} else if raw == "0" {
		return
	} else if d, err := time.ParseDuration(raw); err != nil {
		log.Printf("update: invalid interval %q, using %s", raw, defaultInterval)
	} else if d <= 0 {
		return
	} else {
		interval = d
	}

	go func() {
		if u.stale(set, interval) {
			if _, err := u.Run(ctx); err != nil && !errors.Is(err, ErrBusy) {
				log.Printf("update: initial run failed: %v", err)
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := u.Run(ctx); err != nil && !errors.Is(err, ErrBusy) {
					log.Printf("update: scheduled run failed: %v", err)
				}
			}
		}
	}()
}

// Status returns one entry per configured source.
func (u *Updater) Status() []SourceStatus {
	set := u.Store.Current()
	ups := make(map[string]*rules.Upstream)
	for _, up := range set.Upstreams() {
		ups[up.Source] = up
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	out := make([]SourceStatus, 0, len(set.File.Updates.Sources))
	for _, src := range set.File.Updates.Sources {
		st := SourceStatus{Name: src.Name, Type: src.Type}
		if up, ok := ups[src.Name]; ok {
			st.Sites = len(up.Sites)
			st.UpdatedAt = up.GeneratedAt
		}
		st.LastError = u.errs[src.Name]
		out = append(out, st)
	}
	return out
}

func (u *Updater) stale(set *rules.Set, interval time.Duration) bool {
	ups := make(map[string]*rules.Upstream)
	for _, up := range set.Upstreams() {
		ups[up.Source] = up
	}
	for _, src := range set.File.Updates.Sources {
		up, ok := ups[src.Name]
		if !ok || up.GeneratedAt.IsZero() || time.Since(up.GeneratedAt) > interval {
			return true
		}
	}
	return false
}

func (u *Updater) updateSource(ctx context.Context, set *rules.Set, src rules.Source) (int, error) {
	data, err := u.download(ctx, set, src.URL)
	if err != nil {
		return 0, err
	}

	up := &rules.Upstream{
		Source:      src.Name,
		URL:         src.URL,
		GeneratedAt: time.Now().UTC(),
	}
	switch src.Type {
	case "bpc_zip":
		sites, err := convertBPCZip(data)
		if err != nil {
			return 0, err
		}
		up.Sites = sites
	case "bpc_json":
		sites, err := convertBPCJSON(data)
		if err != nil {
			return 0, err
		}
		up.Sites = sites
	case "ubo":
		generic, sites, err := convertUBO(data)
		if err != nil {
			return 0, err
		}
		up.Generic = generic
		up.Sites = sites
	default:
		return 0, fmt.Errorf("unknown source type %q", src.Type)
	}

	if len(up.Sites) == 0 && specEmpty(up.Generic) {
		return 0, errors.New("no usable rules")
	}

	out, err := json.MarshalIndent(up, "", "  ")
	if err != nil {
		return 0, err
	}
	if _, err := rules.ParseUpstream(out); err != nil {
		return 0, err
	}
	if err := u.writeFile(src.Name, out); err != nil {
		return 0, err
	}
	return len(up.Sites), nil
}

func (u *Updater) download(ctx context.Context, set *rules.Set, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range set.File.Profiles["browser"] {
		req.Header.Set(name, value)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}

	client := u.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (u *Updater) writeFile(name string, data []byte) error {
	path := filepath.Join(u.Dir, name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func specEmpty(sp rules.Spec) bool {
	return len(sp.Strategies) == 0 &&
		sp.Prefer == "" &&
		sp.Referer == nil &&
		sp.RandomIP == nil &&
		sp.KeepScripts == nil &&
		sp.BlockRegex == "" &&
		len(sp.RemoveSelectors) == 0 &&
		len(sp.UnhideSelectors) == 0 &&
		sp.LDJSON == nil &&
		len(sp.PaywallMarkers) == 0 &&
		sp.MinTextLength == nil &&
		len(sp.Scriptlets) == 0
}

func cloneSpec(sp rules.Spec) rules.Spec {
	out := sp
	out.Strategies = append([]string(nil), sp.Strategies...)
	out.RemoveSelectors = append([]string(nil), sp.RemoveSelectors...)
	out.UnhideSelectors = append([]string(nil), sp.UnhideSelectors...)
	out.PaywallMarkers = append([]string(nil), sp.PaywallMarkers...)
	out.Scriptlets = append([]rules.Scriptlet(nil), sp.Scriptlets...)
	return out
}

func appendUnique(dst []string, vals ...string) []string {
	seen := make(map[string]bool, len(dst)+len(vals))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		dst = append(dst, v)
	}
	return dst
}
