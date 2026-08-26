package watch

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// §D14 file watcher: zero-dependency polling scanner (dependency whitelist
// rules out fsnotify). The poller fingerprints the workspace tree by
// (mtime,size), diffs against the previous pass, and reports create/modify/
// delete events. Depth and interval are bounded; the poller is opt-in per
// session so tests and constrained deployments stay deterministic.

// Event is one detected file-system change.
type Event struct {
	Path string
	Kind Kind
}

type Kind int

const (
	Created Kind = iota
	Modified
	Deleted
)

// Poller scans a rooted tree on an interval.
type Poller struct {
	root     string
	interval time.Duration
	maxDepth int

	mu       sync.Mutex
	prints   map[string]finger // nil until first Scan
	stop     chan struct{}
	stopped  sync.WaitGroup
	onChange func([]Event)
}

type finger struct {
	mtime int64
	size  int64
}

func New(root string, interval time.Duration, onChange func([]Event)) *Poller {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Poller{
		root:     root,
		interval: interval,
		maxDepth: 16, // bounded walk: runaway trees cannot stall the loop
		stop:     make(chan struct{}),
		onChange: onChange,
	}
}

// fingerprint walks the tree once and returns path→(mtime,size).
func (p *Poller) fingerprint() map[string]finger {
	out := make(map[string]finger, 256)
	base := len(p.root) + 1
	depthBase := strings.Count(p.root, string(os.PathSeparator))
	_ = filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, never fatal
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == ".hg" || name == ".svn" {
				return filepath.SkipDir
			}
			if strings.Count(path, string(os.PathSeparator))-depthBase > p.maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		key := path
		if len(path) > base && strings.HasPrefix(path, p.root+string(os.PathSeparator)) {
			key = path[base:]
		} else {
			key = filepath.Base(path)
		}
		out[key] = finger{mtime: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	return out
}

// Scan performs one diff pass and reports events through the callback.
func (p *Poller) Scan() []Event {
	now := p.fingerprint()
	p.mu.Lock()
	prev := p.prints
	p.prints = now
	p.mu.Unlock()
	if prev == nil {
		return nil // baseline pass establishes the map, reports nothing
	}
	var evs []Event
	for path, f := range now {
		old, ok := prev[path]
		if !ok {
			evs = append(evs, Event{Path: path, Kind: Created})
		} else if old != f {
			evs = append(evs, Event{Path: path, Kind: Modified})
		}
	}
	for path := range prev {
		if _, ok := now[path]; !ok {
			evs = append(evs, Event{Path: path, Kind: Deleted})
		}
	}
	if len(evs) > 0 && p.onChange != nil {
		p.onChange(evs)
	}
	return evs
}

// Start launches the background loop. Idempotent per Poller instance.
func (p *Poller) Start() *Poller {
	p.stopped.Add(1)
	go func() {
		defer p.stopped.Done()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.Scan()
			}
		}
	}()
	return p
}

// Close stops the loop and waits for it.
func (p *Poller) Close() {
	close(p.stop)
	p.stopped.Wait()
}
