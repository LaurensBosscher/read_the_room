// Package bypass runs the strategy cascade and caches results.
package bypass

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/article"
	"github.com/LaurensBosscher/read_the_room/internal/fetch"
	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

// Attempt is one strategy's outcome.
type Attempt struct {
	Strategy string
	Status   string // "ok", "weak (523 chars, paywall text)", "error: HTTP 403"
}

// Result is a successful or best-effort cascade outcome.
type Result struct {
	Target   *url.URL
	Rule     rules.Rule
	Strategy string
	Resp     *fetch.Response
	Attempts []Attempt
	Complete bool // false = best weak candidate shown
}

// FailedError means no strategy produced a page.
type FailedError struct {
	Attempts []Attempt
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("all strategies failed (%d attempts)", len(e.Attempts))
}

// Bypasser runs the cascade.
type Bypasser struct {
	Rules   *rules.Store
	Fetcher *fetch.Fetcher

	cache cache
}

type cacheEntry struct {
	res     *Result
	created time.Time
	expires time.Time
}

type cache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

const (
	cacheTTL   = 10 * time.Minute
	cacheLimit = 64
)

func (c *cache) get(key string) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(c.m, key)
		return nil, false
	}
	return e.res, true
}

func (c *cache) put(key string, res *Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cacheEntry{}
	}
	now := time.Now()
	for k, e := range c.m {
		if now.After(e.expires) {
			delete(c.m, k)
		}
	}
	for len(c.m) >= cacheLimit {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.m {
			if oldestKey == "" || e.created.Before(oldest) {
				oldestKey, oldest = k, e.created
			}
		}
		delete(c.m, oldestKey)
	}
	c.m[key] = cacheEntry{res: res, created: now, expires: now.Add(cacheTTL)}
}

// Run executes the cascade for target. only, when non-empty, is the sole
// strategy to try.
func (b *Bypasser) Run(ctx context.Context, target *url.URL, only string) (*Result, error) {
	key := target.String() + "\x00" + only
	if res, ok := b.cache.get(key); ok {
		return res, nil
	}

	set := b.Rules.Current()
	rule := set.Match(target.Hostname())
	strategies := rule.Strategies
	if only != "" {
		strategies = []string{only}
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var attempts []Attempt
	var best *Result
	bestLen := -1
	for _, strategy := range strategies {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		resp, err := b.Fetcher.Fetch(ctx, set, rule, strategy, target)
		if err != nil {
			attempts = append(attempts, Attempt{Strategy: strategy, Status: "error: " + err.Error()})
			continue
		}
		art, err := article.Extract(resp.HTML, resp.BaseURL, rule, nil)
		if err != nil {
			attempts = append(attempts, Attempt{Strategy: strategy, Status: "error: " + err.Error()})
			continue
		}
		if art.TextLen < rule.MinTextLength || art.Paywalled {
			attempts = append(attempts, Attempt{Strategy: strategy, Status: weakStatus(art, rule.MinTextLength)})
			if art.TextLen > bestLen {
				bestLen = art.TextLen
				best = &Result{
					Target:   target,
					Rule:     rule,
					Strategy: strategy,
					Resp:     resp,
				}
			}
			continue
		}
		attempts = append(attempts, Attempt{Strategy: strategy, Status: "ok"})
		res := &Result{
			Target:   target,
			Rule:     rule,
			Strategy: strategy,
			Resp:     resp,
			Attempts: attempts,
			Complete: true,
		}
		logResult(res)
		b.cache.put(key, res)
		return res, nil
	}

	if best != nil {
		best.Attempts = attempts
		logResult(best)
		b.cache.put(key, best)
		return best, nil
	}
	return nil, &FailedError{Attempts: attempts}
}

func weakStatus(a *article.Article, min int) string {
	var reasons []string
	if a.TextLen < min {
		reasons = append(reasons, fmt.Sprintf("%d chars", a.TextLen))
	}
	if a.Paywalled {
		reasons = append(reasons, "paywall text")
	}
	return "weak (" + strings.Join(reasons, ", ") + ")"
}

func logResult(res *Result) {
	parts := make([]string, 0, len(res.Attempts))
	for _, a := range res.Attempts {
		parts = append(parts, a.Strategy+":"+a.Status)
	}
	log.Printf("bypass %s via=%s rule=%q complete=%t attempts=[%s]",
		res.Target, res.Strategy, res.Rule.Name, res.Complete, strings.Join(parts, "; "))
}
