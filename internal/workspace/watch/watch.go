package watch

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxDepthLimit     = 64
	maxFileBytesLimit = int64(256 << 20)
	maxTreeBytesLimit = int64(8 << 30)
)

// §D14 file watcher: zero-dependency polling scanner (dependency whitelist
// rules out fsnotify). The poller fingerprints the workspace tree by
// (mtime,size,content), diffs against the previous complete pass, and reports
// create/modify/delete events. Depth and interval are bounded; the poller is
// opt-in per session so tests and constrained deployments stay deterministic.

// Event is one detected file-system change. Path is an absolute path — the
// server converts each event straight into a file URI, and a relative path
// would parse as a UNC host (audit finding 6).
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
	root         string
	interval     time.Duration
	maxDepth     int
	maxFileBytes int64
	maxTreeBytes int64
	scanMu       sync.Mutex

	mu             sync.Mutex
	prints         map[string]finger // nil until first Scan
	excludedRoots  []string          // absolute cleaned paths; immutable after first Scan
	lastScanErr    error
	errorHandler   func(error)
	reportedErr    string
	hadReportedErr bool
	stop           chan struct{}
	stopped        sync.WaitGroup
	started        bool
	scanStarted    bool
	closed         bool
	callbacks      int
	callbackDone   *sync.Cond
	closeOnce      sync.Once
	onChange       func([]Event)
}

type finger struct {
	mtime  int64
	size   int64
	ident  fileID
	digest [32]byte
}

// fileID is the kernel identity of one file: volume serial + file index on
// Windows, device + inode on Unix. It is what an atomic replace provably
// changes even when timestamp coalescing leaves the (mtime,size) signature
// untouched. valid=false means "identity unavailable" (platform support or a
// failed probe) and disables discrimination for that fingerprint rather than
// guessing.
type fileID struct {
	valid bool
	vol   uint64
	index uint64
}

// identityStable reports whether the file behind an unchanged (mtime,size)
// stat signature is still the same file the baseline fingerprinted. Only a
// provably different identity (an atomic replace whose replacement timestamps
// were coalesced onto the baseline's tick) may un-short-circuit the scan;
// anything undecidable — identity unavailable on this platform, a failed
// probe, a non-regular file that must not be opened (a FIFO could block) —
// degrades to the legacy stat-signature-only behavior rather than turning a
// quiet file into churn.
func identityStable(path string, info os.FileInfo, old fileID) bool {
	if !old.valid || !info.Mode().IsRegular() {
		return true
	}
	id, ok := statIdentity(path, info)
	if !ok {
		return true
	}
	return id == old
}

func New(root string, interval time.Duration, onChange func([]Event)) *Poller {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	poller := &Poller{
		root:         root,
		interval:     interval,
		maxDepth:     maxDepthLimit,
		maxFileBytes: maxFileBytesLimit,
		maxTreeBytes: maxTreeBytesLimit,
		stop:         make(chan struct{}),
		onChange:     onChange,
	}
	poller.callbackDone = sync.NewCond(&poller.mu)
	return poller
}

// SetExcludedRoots replaces the subtrees omitted from future scans. Paths are
// made absolute and cleaned without resolving symlinks. Configure exclusions
// before Start or the first Scan so the baseline always uses the same tree.
func (p *Poller) SetExcludedRoots(paths ...string) error {
	if p == nil {
		return errors.New("watch: cannot configure exclusions on a nil poller")
	}
	p.scanMu.Lock()
	defer p.scanMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.scanStarted || p.closed {
		return errors.New("watch: excluded roots must be configured before Start or Scan")
	}

	root, err := filepath.Abs(p.root)
	if err != nil {
		return fmt.Errorf("watch: resolve workspace root %q: %w", p.root, err)
	}
	root = filepath.Clean(root)
	roots := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			return errors.New("watch: excluded root must not be empty")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("watch: resolve excluded root %q: %w", path, err)
		}
		absolute = filepath.Clean(absolute)
		if rel, relErr := filepath.Rel(absolute, root); relErr == nil && relativePathWithin(rel) {
			return fmt.Errorf("watch: excluded root %q would exclude workspace root %q", absolute, root)
		}
		if _, duplicate := seen[absolute]; duplicate {
			continue
		}
		seen[absolute] = struct{}{}
		roots = append(roots, absolute)
	}
	// Anchor scans to the same absolute root used for exclusion validation.
	p.root = root
	p.excludedRoots = roots
	return nil
}

// relativePathWithin reports whether a filepath.Rel result names its root or
// one of its descendants. The component boundary avoids matching sibling
// prefixes such as "cache" and "cache-old".
func relativePathWithin(rel string) bool {
	return rel == "." || (rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func (p *Poller) excludedPath(path string) bool {
	for _, root := range p.excludedRoots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			// Different Windows volumes cannot contain one another.
			continue
		}
		if relativePathWithin(rel) {
			return true
		}
	}
	return false
}

// fingerprint walks the tree once and returns path→(mtime,size,content).
// A partial walk is not a valid basis for diffing or replacing the baseline.
func (p *Poller) fingerprint() (map[string]finger, error) {
	out := make(map[string]finger, 256)
	var scanErr error
	var totalBytes int64
	// Snapshot of the previous baseline for the (mtime,size) re-hash
	// short-circuit; maps are replaced, never mutated, once published.
	p.mu.Lock()
	prev := p.prints
	p.mu.Unlock()
	recordError := func(err error) {
		if scanErr == nil {
			scanErr = err
		}
	}
	walkErr := filepath.WalkDir(p.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			recordError(fmt.Errorf("watch: walk %q: %w", path, walkErr))
			return nil
		}
		if p.excludedPath(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if skipDirectory(p.root, path, name) {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(p.root, path)
			if relErr != nil {
				recordError(fmt.Errorf("watch: relative path %q: %w", path, relErr))
				return filepath.SkipDir
			}
			if strings.Count(filepath.ToSlash(rel), "/") >= p.maxDepth {
				recordError(fmt.Errorf("watch: depth limit %d exceeded at %q", p.maxDepth, path))
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			recordError(fmt.Errorf("watch: stat %q: %w", path, ierr))
			return nil
		}
		key := path
		if !filepath.IsAbs(key) {
			// New accepts a relative root (SetExcludedRoots anchors it
			// only when called), and WalkDir then yields relative paths.
			// Events feed the server's file-URI conversion directly, so
			// the fingerprint key — and every event derived from it —
			// must be absolute: a relative key parses as a UNC host and
			// loses the per-file invalidation for the real file (audit
			// finding 6).
			if abs, absErr := filepath.Abs(key); absErr == nil {
				key = abs
			}
		}
		// Size limits are evaluated for EVERY regular file from its stat,
		// including files whose fingerprint will be reused below: the tree
		// budget bounds the tree as a whole (baseline preservation for an
		// oversized tree is pinned by TestSizeLimitsReportIncompleteAnd-
		// PreserveBaseline), not merely the bytes this pass happens to read.
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > p.maxFileBytes {
				recordError(fmt.Errorf("watch: file size %d exceeds limit %d at %q", info.Size(), p.maxFileBytes, path))
				return nil
			}
			if totalBytes > p.maxTreeBytes-info.Size() {
				recordError(fmt.Errorf("watch: workspace exceeds %d bytes", p.maxTreeBytes))
				return nil
			}
			totalBytes += info.Size()
		}
		// (mtime,size) short-circuit (audit circle 2, finding 5): reuse the
		// previous digest whenever the stat signature is unchanged, so a
		// quiet tree costs a stat pass instead of a full read+hash every
		// interval. Contract cut line, amended by bug round 20 A#1: the
		// stat signature IS the change signal for an edit that keeps the
		// SAME file (a content edit preserving BOTH mtime and size stays
		// invisible — TestWatcherSkipsRereadForUnchangedStatSignature pins
		// that) — but it is NOT sufficient across a replace: Windows
		// timestamp coalescing empirically merges writes milliseconds apart
		// onto the same tick, so an atomic replace can land with mtime and
		// size equal to the baseline and the old "editors and atomic
		// replaces move mtime" cut line was falsified on that host (19% of
		// replaces; the change was then permanently swallowed because the
		// reused fingerprint kept the stale signature). The short-circuit
		// therefore also requires the kernel file identity (volume+index on
		// Windows, inode on Unix) to match; a provably different identity
		// falls through to the re-hash below. prev is only ever replaced
		// (never mutated) under scanMu, which this walk already holds.
		if prev != nil {
			if old, ok := prev[key]; ok && old.mtime == info.ModTime().UnixNano() && old.size == info.Size() {
				if identityStable(path, info, old.ident) {
					out[key] = old
					return nil
				}
			}
		}
		var digest [32]byte
		var ident fileID
		if info.Mode().IsRegular() {
			file, openErr := openReadShared(path)
			if openErr != nil {
				recordError(fmt.Errorf("watch: open %q: %w", path, openErr))
				return nil
			}
			hasher := sha256.New()
			readBytes, readErr := io.Copy(hasher, io.LimitReader(file, info.Size()))
			readInfo, statErr := file.Stat()
			closeErr := file.Close()
			if readErr != nil || statErr != nil || closeErr != nil {
				recordError(fmt.Errorf("watch: read %q: %w", path, errors.Join(readErr, statErr, closeErr)))
				return nil
			}
			if readBytes != info.Size() || readInfo.Size() != info.Size() {
				recordError(fmt.Errorf("watch: read %q: byte count changed from %d to %d", path, info.Size(), readBytes))
				return nil
			}
			copy(digest[:], hasher.Sum(nil))
			// Identity rides along with the digest so later rounds can
			// un-short-circuit a coalesced-timestamp replace. A failed probe
			// leaves ident invalid, degrading this file to legacy
			// stat-signature behavior instead of failing the scan.
			ident, _ = statIdentity(path, info)
		}
		out[key] = finger{mtime: info.ModTime().UnixNano(), size: info.Size(), ident: ident, digest: digest}
		return nil
	})
	if walkErr != nil {
		recordError(fmt.Errorf("watch: walk root %q: %w", p.root, walkErr))
	}
	return out, scanErr
}

func skipDirectory(root, path, name string) bool {
	switch name {
	case ".git", ".hg", ".svn":
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	switch filepath.ToSlash(rel) {
	case ".tmp-gocache", "dist", "dist-smoke", "test/acceptance/evidence",
		"test/acceptance/tools/bin", "test/acceptance/tools/node_modules", "test/acceptance/tools/.npm-cache",
		"editors/vscode/node_modules":
		return true
	default:
		return false
	}
}

// Scan performs one diff pass and reports events through the callback.
func (p *Poller) Scan() []Event {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	p.mu.Lock()
	p.scanStarted = true
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil
	}
	now, scanErr := p.fingerprint()
	p.mu.Lock()
	p.lastScanErr = scanErr
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	errorHandler, notifyError := p.scanErrorNotificationLocked(scanErr)
	if notifyError {
		p.callbacks++
	}
	if scanErr != nil {
		p.mu.Unlock()
		if notifyError {
			p.runErrorCallback(errorHandler, scanErr)
		}
		return nil
	}
	prev := p.prints
	p.prints = now
	p.mu.Unlock()
	if notifyError {
		p.runErrorCallback(errorHandler, nil)
	}
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
	if len(evs) > 0 {
		p.mu.Lock()
		callback := p.onChange
		if p.closed || callback == nil {
			p.mu.Unlock()
			return evs
		}
		p.callbacks++
		p.mu.Unlock()
		p.runCallback(func() { callback(evs) })
	}
	return evs
}

func (p *Poller) scanErrorNotificationLocked(err error) (func(error), bool) {
	if p.errorHandler == nil {
		return nil, false
	}
	if err == nil {
		if !p.hadReportedErr {
			return nil, false
		}
		p.hadReportedErr = false
		p.reportedErr = ""
		return p.errorHandler, true
	}
	message := err.Error()
	if p.hadReportedErr && p.reportedErr == message {
		return nil, false
	}
	p.hadReportedErr = true
	p.reportedErr = message
	return p.errorHandler, true
}

func (p *Poller) runErrorCallback(callback func(error), err error) {
	p.runCallback(func() { callback(err) })
}

func (p *Poller) runCallback(callback func()) {
	defer func() {
		p.mu.Lock()
		p.callbacks--
		p.callbackDone.Broadcast()
		p.mu.Unlock()
	}()
	callback()
}

// LastScanError returns the most recent scan error, or nil after a successful scan.
func (p *Poller) LastScanError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastScanErr
}

// SetErrorHandler sets a callback for scan errors and recovery. Set it before
// Start when possible; callbacks already in progress use their captured handler.
func (p *Poller) SetErrorHandler(handler func(error)) {
	p.mu.Lock()
	p.errorHandler = handler
	p.hadReportedErr = false
	p.reportedErr = ""
	p.mu.Unlock()
}

// HasBaseline reports whether a complete initial scan has established the
// comparison baseline.
func (p *Poller) HasBaseline() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prints != nil
}

// Start launches the background loop. Idempotent per Poller instance.
func (p *Poller) Start() *Poller {
	p.mu.Lock()
	if p.started || p.closed {
		p.mu.Unlock()
		return p
	}
	p.started = true
	p.stopped.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.stopped.Done()
		p.Scan()
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
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

// Close requests loop shutdown. When called from a callback, it returns without
// waiting for the callback that is currently executing.
func (p *Poller) Close() {
	p.close(false)
}

// CloseAndWait stops the loop and waits for it and any active callbacks.
// Call Close instead from a callback; waiting there would require the callback
// to return before its own completion can be seen.
func (p *Poller) CloseAndWait() {
	p.close(true)
}

func (p *Poller) close(waitForCallbacks bool) {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.stop)
	})
	if !waitForCallbacks {
		return
	}
	p.stopped.Wait()
	p.mu.Lock()
	for p.callbacks > 0 {
		p.callbackDone.Wait()
	}
	p.mu.Unlock()
}
