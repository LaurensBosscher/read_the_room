package rules

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store holds the current rule set and hot-reloads it when the local file or
// an upstream file changes.
type Store struct {
	mu        sync.Mutex
	path      string
	builtin   []byte
	dir       string
	current   *Set
	lastCheck time.Time
	sig       map[string]fileSig
}

type fileSig struct {
	mod    time.Time
	size   int64
	exists bool
}

// NewStore loads the local rules (falling back to builtin when the file is
// missing) and every configured upstream file.
func NewStore(rulesPath string, builtin []byte, upstreamDir string) (*Store, error) {
	s := &Store{path: rulesPath, builtin: builtin, dir: upstreamDir}
	set, err := s.loadLocal(true)
	if err != nil {
		return nil, err
	}
	s.current = set.WithUpstream(s.loadUpstreams(set))
	s.sig = s.collect()
	return s, nil
}

// Current returns the current set, attempting a reload at most every 2s.
func (s *Store) Current() *Set {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.lastCheck) < 2*time.Second {
		return s.current
	}
	s.lastCheck = now
	if s.unchanged() {
		return s.current
	}
	if err := s.rebuildLocked(); err != nil {
		log.Printf("rules: reload failed, keeping previous rules: %v", err)
		s.sig = s.collect()
	}
	return s.current
}

// Reload rebuilds immediately and returns any error.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuildLocked()
}

func (s *Store) rebuildLocked() error {
	set, err := s.loadLocal(false)
	if err != nil {
		return err
	}
	ups := s.loadUpstreams(set)
	s.current = set.WithUpstream(ups)
	n := 0
	for _, u := range ups {
		n += len(u.Sites)
	}
	log.Printf("rules: reloaded (%d local sites, %d upstream sites)", len(set.File.Sites), n)
	s.sig = s.collect()
	return nil
}

func (s *Store) loadLocal(logMissing bool) (*Set, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if logMissing {
			log.Printf("rules: %s not found, using built-in rules", s.path)
		}
		data = s.builtin
	}
	return Parse(data)
}

func (s *Store) loadUpstreams(set *Set) []*Upstream {
	var ups []*Upstream
	for _, src := range set.File.Updates.Sources {
		data, err := os.ReadFile(filepath.Join(s.dir, src.Name+".json"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			log.Printf("rules: ignoring upstream %s: %v", src.Name, err)
			continue
		}
		u, err := ParseUpstream(data)
		if err != nil {
			log.Printf("rules: ignoring upstream %s: %v", src.Name, err)
			continue
		}
		ups = append(ups, u)
	}
	return ups
}

func (s *Store) collect() map[string]fileSig {
	sig := map[string]fileSig{s.path: statFile(s.path)}
	sources := s.current.File.Updates.Sources
	for _, src := range sources {
		p := filepath.Join(s.dir, src.Name+".json")
		sig[p] = statFile(p)
	}
	return sig
}

func (s *Store) unchanged() bool {
	if s.sig == nil {
		return false
	}
	if len(s.sig) != len(s.current.File.Updates.Sources)+1 {
		return false
	}
	for p, old := range s.sig {
		if statFile(p) != old {
			return false
		}
	}
	return true
}

func statFile(p string) fileSig {
	fi, err := os.Stat(p)
	if err != nil {
		return fileSig{}
	}
	return fileSig{mod: fi.ModTime(), size: fi.Size(), exists: true}
}
