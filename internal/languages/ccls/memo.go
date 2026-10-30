package ccls

// Hot-path memoization for the two clangd requests that pay a full frontend
// invocation per request on this bridge (out/perf2-cpp-hotpath-diagnosis.md):
//
//   - textDocument/completion: clangd always runs a standalone frontend parse
//     with the completion point, even for byte-identical requests (~40-70ms
//     per request on the Windows reference host).
//   - textDocument/documentSymbol after any content change: the first query
//     following a didChange pays the full AST rebuild.
//
// Both are pure functions of (document bytes, compilation context, index
// state), so a bounded memo keyed on content — not on the snapshot revision,
// which the S18 sampler bumps on every edit — turns repeated identical
// requests into ~microsecond answers without changing any answer.
//
// Correctness rules, all enforced below:
//
//  1. Keys carry the document content hash, the supervisor epoch (a worker
//     replacement changes clangd's view of the world) and a bridge-side
//     invalidation counter bumped on external source changes and worker
//     recovery. The completion key additionally carries the nested workspace
//     generation: no editor edit can happen between two identical completion
//     requests without the generation advancing, so completion entries are
//     dropped on every workspace mutation. The documentSymbol key must NOT
//     carry the generation — every editor didChange advances it — so
//     cross-document staleness is covered by the snapshot-diff drops of
//     observeWorkspaceSnapshot instead (a foreign C/C++ document change
//     removes entries; a document's own change leaves its content-keyed
//     entries in place).
//  2. Quiet admission (anti "frozen prewarm list"): clangd answers
//     completions with a fast, context-incomplete list while its first
//     preamble is still building. Serving such a list from a memo would
//     freeze it. An entry therefore serves only after (a) a second forward
//     returned an equivalent result — the same candidate multiset; clangd
//     breaks ties between same-scored candidates nondeterministically, so
//     byte-identical requests can legitimately differ in candidate order
//     alone (out/perf2-cpp-completion-memo-miss.md) — and (b) the entry's
//     value has been stable for the quiet period, which bounds any hidden
//     async warmup (first preamble build, index settling) from both sides.
//     Equivalence is decided by completionListsEquivalent below: order is
//     normalized away for the comparison only; every other difference
//     (a field inside a candidate, an added or removed candidate, the
//     incompleteness bit) still counts as a value change and restarts
//     admission.
//  3. Capacity is bounded; the least recently used entry is evicted.
//  4. While the S18 nested RPC trace is enabled the memo stands down: that
//     evidence mode requires exactly one nested timing sample per candidate
//     request, and a memo hit issues no nested request at all.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// cclsNestedRPCTimingEnv mirrors nested.nestedRPCTimingEnv: when set, the
// nested bridge records one timing sample per completion/documentSymbol
// request and the S18 stable-ABBA evidence correlates every candidate request
// against those samples. Memo hits cannot produce a sample, so the memo is
// disabled while this trace is armed.
const cclsNestedRPCTimingEnv = "OMNILSP_S18_NESTED_RPC_TRACE"

// cclsMemoBudgetOwner is the §F14 backend-buffer budget owner for the memo
// tables (DEF-BUDWIRE). The memo has no nested.Conn of its own, so it uses a
// dedicated owner: OMNILSP_BACKEND_QUOTA_CCLS_MEMO_BYTES caps it, and the
// global limit always binds.
const cclsMemoBudgetOwner = "ccls-memo"

const (
	// cclsMemoCapacity bounds each table; the S18 sampler needs two live
	// documentSymbol entries (marker A/B alternation), everything beyond is
	// headroom for real navigation patterns.
	cclsMemoCapacity = 32
	// cclsMemoQuietPeriod is how long an entry's value must have been stable
	// before it may serve. The diagnosed prewarm window (32 fast completions
	// while the first preamble builds) spans ~130ms; 250ms covers it with
	// margin. It is a field on the memo so tests can tighten or relax it.
	cclsMemoQuietPeriodDefault = 250 * time.Millisecond
)

type completionMemoKey struct {
	uri           string
	contentHash   string
	line          uint32
	column        uint32
	encoding      int
	epoch         uint64
	generation    uint64
	invalidations uint64
}

type symbolMemoKey struct {
	uri           string
	contentHash   string
	epoch         uint64
	invalidations uint64
}

// completionMemoKeyFor is pure so key-equality semantics (which guard fields
// invalidate which) can be pinned by tests without driving a live worker.
func completionMemoKeyFor(uri string, content []byte, line, column uint32, encoding int, epoch, generation, invalidations uint64) completionMemoKey {
	sum := sha256.Sum256(content)
	return completionMemoKey{
		uri:           canonicalMemoURI(uri),
		contentHash:   hex.EncodeToString(sum[:]),
		line:          line,
		column:        column,
		encoding:      encoding,
		epoch:         epoch,
		generation:    generation,
		invalidations: invalidations,
	}
}

func symbolMemoKeyFor(uri string, content []byte, epoch, invalidations uint64) symbolMemoKey {
	sum := sha256.Sum256(content)
	return symbolMemoKey{
		uri:           canonicalMemoURI(uri),
		contentHash:   hex.EncodeToString(sum[:]),
		epoch:         epoch,
		invalidations: invalidations,
	}
}

// canonicalMemoURI normalizes parseable URIs the same way the nested bridge
// keys its documents, so differently-spelled equivalent URIs share entries.
func canonicalMemoURI(raw string) string {
	parsed, err := workspaceuri.Parse(raw)
	if err != nil {
		return raw
	}
	return parsed.Canonical()
}

type memoEntry[T any] struct {
	value     T
	confirmed bool
	// firstSeen is when the current value was first observed; the quiet
	// window runs from there, and a changed value restarts it.
	firstSeen time.Time
	// usedSeq is a monotonic access sequence (deterministic LRU; wall-clock
	// stamps can tie on coarse clocks).
	usedSeq uint64
	// budgetBytes is the §F14 backend-buffer charge for value (its JSON
	// wire size), held from store until the entry is replaced, evicted, or
	// dropped. Zero when no budget is configured or sizing failed.
	budgetBytes int64
}

// cclsMemo is embedded by value in Backend; its zero value is ready to use.
type cclsMemo struct {
	mu sync.Mutex

	// disabled is latched once from the environment in New.
	disabled bool

	// invalidations is the bridge-side epoch of "the whole world changed":
	// external source changes and worker recovery. Workspace snapshot
	// mutations use the finer-grained per-entry drops of
	// observeWorkspaceSnapshot instead.
	invalidations uint64

	// useSeq is the monotonic counter behind the LRU eviction order.
	useSeq uint64

	quietPeriod time.Duration

	completions map[completionMemoKey]*memoEntry[languages.CompletionList]
	symbols     map[symbolMemoKey]*memoEntry[[]languages.DocumentSymbol]

	// workspace tracks the content hashes of the C/C++ documents in the
	// last observed workspace snapshot, keyed by canonical URI.
	workspace map[string]string
}

func (m *cclsMemo) lock() {
	m.mu.Lock()
	if m.quietPeriod <= 0 {
		m.quietPeriod = cclsMemoQuietPeriodDefault
	}
	m.useSeq++
}

// completionServe reports whether an entry is admitted: confirmed by an
// equal re-computation and stable for the quiet period. Access refreshes
// LRU bookkeeping. The disabled stand-down is enforced single-site at the
// Backend key builders (completionMemoKey/symbolMemoKey); the table methods
// themselves are disable-agnostic.
func (m *cclsMemo) completionServe(key completionMemoKey, now time.Time) (languages.CompletionList, bool) {
	m.lock()
	defer m.mu.Unlock()
	entry, ok := m.completions[key]
	if !ok {
		return languages.CompletionList{}, false
	}
	entry.usedSeq = m.useSeq
	if !entry.confirmed || now.Sub(entry.firstSeen) < m.quietPeriod {
		return languages.CompletionList{}, false
	}
	return entry.value, true
}

func (m *cclsMemo) completionStore(key completionMemoKey, value languages.CompletionList, now time.Time) {
	m.lock()
	defer m.mu.Unlock()
	// §F14 (DEF-BUDWIRE): a memo value is resident backend memory and
	// counts against the "ccls-memo" quota. The charge is the value's JSON
	// wire size — the same measure the admission sort already pays. A
	// refused charge skips the store: the next identical request re-forwards
	// to clangd instead of staging unbounded memory, and the refusal is
	// counted (the memo hit is a cache; the request itself still succeeds).
	charged := memoValueBytes(value)
	if charged > 0 && !languages.ReserveBackendBuffer(cclsMemoBudgetOwner, charged) {
		languages.NoteBackendBufferRefusal(cclsMemoBudgetOwner)
		return
	}
	if m.completions == nil {
		m.completions = make(map[completionMemoKey]*memoEntry[languages.CompletionList])
	}
	entry, ok := m.completions[key]
	if ok && completionListsEquivalent(entry.value, value) {
		// A second forward returned the same candidate multiset — possibly
		// in a different tie-broken order, which is not a value change: the
		// document was quiet through a full round trip, so the value is
		// admitted once it ages past the quiet window (which started at the
		// first observation of this value). The stored value refreshes to
		// the latest raw upstream ordering, so what is served is a
		// byte-exact answer clangd actually produced. The charge swaps to
		// the refreshed value's size (identical multisets encode to the
		// same total, but the swap keeps the accounting honest regardless).
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, entry.budgetBytes)
		entry.value = value
		entry.budgetBytes = charged
		entry.confirmed = true
		entry.usedSeq = m.useSeq
		return
	}
	if ok {
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, entry.budgetBytes)
		entry.budgetBytes = 0
	}
	if !ok && len(m.completions) >= cclsMemoCapacity {
		m.evictCompletionLocked()
	}
	m.completions[key] = &memoEntry[languages.CompletionList]{
		value: value, confirmed: false, firstSeen: now, usedSeq: m.useSeq,
		budgetBytes: charged,
	}
}

// memoValueBytes measures a memo value's resident size by its JSON wire
// encoding — the same total the sort-key admission path already pays per
// item. A marshal failure (unreachable for these JSON-safe types) returns 0,
// storing the entry uncharged rather than refusing a good answer.
func memoValueBytes(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return int64(len(encoded))
}

// completionListsEquivalent reports whether two decoded completion lists
// carry the same answer: the same incompleteness bit and the same candidate
// multiset. clangd breaks ties between same-scored candidates (overload
// pairs, keyword pairs) nondeterministically, so byte-identical requests can
// return the same candidates in a different order; order alone is therefore
// not treated as a value change. Every other difference — a field change
// inside a candidate, an added or removed candidate, a flipped
// incompleteness bit — is a real change: the comparison runs
// reflect.DeepEqual over the actual sorted items, so it can never report
// equivalence for differing multisets regardless of what the sort key does.
// The sort feeds only this admission comparison; the served value keeps the
// upstream ordering untouched.
func completionListsEquivalent(a, b languages.CompletionList) bool {
	if a.IsIncomplete != b.IsIncomplete || len(a.Items) != len(b.Items) {
		return false
	}
	return reflect.DeepEqual(completionItemsSorted(a.Items), completionItemsSorted(b.Items))
}

// completionItemsSorted returns the items arranged by a total order whose
// equivalence coincides with full structural equality of
// languages.CompletionItem, so a candidate multiset has exactly one sorted
// arrangement and equal multisets always compare equal. A zero-length slice
// normalizes to nil: both spellings mean "no candidates" on the wire decode
// path (lspwire.DecodeCompletionList never produces a non-nil empty slice).
func completionItemsSorted(items []languages.CompletionItem) []languages.CompletionItem {
	if len(items) == 0 {
		return nil
	}
	keys := make([]string, len(items))
	for i, item := range items {
		keys[i] = completionItemSortKey(item)
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool { return keys[order[x]] < keys[order[y]] })
	sorted := make([]languages.CompletionItem, len(items))
	for out, idx := range order {
		sorted[out] = items[idx]
	}
	return sorted
}

// completionItemSortKey is the total-order key for completionItemsSorted:
// the JSON encoding of the item, which covers every retained field
// (sortText, label, kind, detail, insertText, filterText, insertTextFormat,
// documentation, textEdit, additionalTextEdits, evidence) and is injective
// for languages.CompletionItem — the type is JSON-safe by construction (no
// maps or interface fields; pointers encode by pointee with nil as null) —
// so structurally different items always get different keys and ties only
// happen between interchangeable identical items.
func completionItemSortKey(item languages.CompletionItem) string {
	encoded, err := json.Marshal(item)
	if err != nil {
		// Unreachable for languages.CompletionItem. Should the type ever
		// lose JSON-safety, deflated keys can only make the sorted
		// arrangement nondeterministic (conservative: admission fails to
		// confirm); the DeepEqual over the sorted items still never equates
		// different multisets.
		return ""
	}
	return string(encoded)
}

func (m *cclsMemo) evictCompletionLocked() {
	var oldest completionMemoKey
	var oldestUsed uint64
	first := true
	for key, entry := range m.completions {
		if first || entry.usedSeq < oldestUsed {
			oldest, oldestUsed, first = key, entry.usedSeq, false
		}
	}
	if !first {
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, m.completions[oldest].budgetBytes)
		delete(m.completions, oldest)
	}
}

func (m *cclsMemo) symbolServe(key symbolMemoKey, now time.Time) ([]languages.DocumentSymbol, bool) {
	m.lock()
	defer m.mu.Unlock()
	entry, ok := m.symbols[key]
	if !ok {
		return nil, false
	}
	entry.usedSeq = m.useSeq
	if !entry.confirmed || now.Sub(entry.firstSeen) < m.quietPeriod {
		return nil, false
	}
	return entry.value, true
}

func (m *cclsMemo) symbolStore(key symbolMemoKey, value []languages.DocumentSymbol, now time.Time) {
	m.lock()
	defer m.mu.Unlock()
	// §F14 (DEF-BUDWIRE): same charge-and-refuse discipline as
	// completionStore, against the "ccls-memo" quota.
	charged := memoValueBytes(value)
	if charged > 0 && !languages.ReserveBackendBuffer(cclsMemoBudgetOwner, charged) {
		languages.NoteBackendBufferRefusal(cclsMemoBudgetOwner)
		return
	}
	if m.symbols == nil {
		m.symbols = make(map[symbolMemoKey]*memoEntry[[]languages.DocumentSymbol])
	}
	entry, ok := m.symbols[key]
	if ok && reflect.DeepEqual(entry.value, value) {
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, entry.budgetBytes)
		entry.value = value
		entry.budgetBytes = charged
		entry.confirmed = true
		entry.usedSeq = m.useSeq
		return
	}
	if ok {
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, entry.budgetBytes)
		entry.budgetBytes = 0
	}
	if !ok && len(m.symbols) >= cclsMemoCapacity {
		m.evictSymbolLocked()
	}
	m.symbols[key] = &memoEntry[[]languages.DocumentSymbol]{
		value: value, confirmed: false, firstSeen: now, usedSeq: m.useSeq,
		budgetBytes: charged,
	}
}

func (m *cclsMemo) evictSymbolLocked() {
	var oldest symbolMemoKey
	var oldestUsed uint64
	first := true
	for key, entry := range m.symbols {
		if first || entry.usedSeq < oldestUsed {
			oldest, oldestUsed, first = key, entry.usedSeq, false
		}
	}
	if !first {
		languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, m.symbols[oldest].budgetBytes)
		delete(m.symbols, oldest)
	}
}

// invalidate drops nothing eagerly: the invalidations counter is part of
// every key, so bumped keys simply cannot reach old entries and LRU
// reclaims them.
func (m *cclsMemo) invalidate() {
	m.mu.Lock()
	m.invalidations++
	m.mu.Unlock()
}

// currentInvalidations snapshots the counter for key construction.
func (m *cclsMemo) currentInvalidations() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invalidations
}

// observeWorkspaceSnapshot diffs the C/C++ documents of a new workspace
// snapshot against the last observed one. A content change to some OTHER
// document (or any document appearing/disappearing) can change this
// document's clangd answer through includes and overlays, so it must
// invalidate every entry; a document's own content change is already carried
// by its key's content hash and leaves its own entries valid. The S18
// documentSymbol sampler flips a marker in a single self-contained file, so
// its own-document edits do not disturb the A/B entries.
//
// The bool reports whether an invalidation happened.
func (m *cclsMemo) observeWorkspaceSnapshot(documents map[string][]byte) bool {
	m.lock()
	defer m.mu.Unlock()
	if m.disabled {
		return false
	}
	next := make(map[string]string, len(documents))
	for uri, content := range documents {
		sum := sha256.Sum256(content)
		next[canonicalMemoURI(uri)] = hex.EncodeToString(sum[:])
	}
	changed := make(map[string]struct{})
	for uri, hash := range next {
		if previous, ok := m.workspace[uri]; !ok || previous != hash {
			changed[uri] = struct{}{}
		}
	}
	for uri := range m.workspace {
		if _, ok := next[uri]; !ok {
			// A document disappearing from the workspace changes the
			// compilation world for every remaining document.
			changed[uri] = struct{}{}
		}
	}
	if m.workspace == nil {
		m.workspace = next
		return false
	}
	m.workspace = next
	if len(changed) == 0 {
		return false
	}
	// Entries for a document whose own bytes changed stay; everything else
	// was computed in a world where at least one other document differed.
	// The global invalidation counter is not bumped: it must stay stable
	// across own-document edits so a document's content-keyed entries
	// survive its own marker flips.
	m.dropEntriesChangedUnder(changed)
	return true
}

// dropEntriesChangedUnder expects the caller to hold m.mu and the changed
// set to be non-empty.
func (m *cclsMemo) dropEntriesChangedUnder(changed map[string]struct{}) {
	entrySeesForeignChange := func(uri string) bool {
		for changedURI := range changed {
			if changedURI != uri {
				return true
			}
		}
		return false
	}
	for key := range m.completions {
		if entrySeesForeignChange(key.uri) {
			languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, m.completions[key].budgetBytes)
			delete(m.completions, key)
		}
	}
	for key := range m.symbols {
		if entrySeesForeignChange(key.uri) {
			languages.ReleaseBackendBuffer(cclsMemoBudgetOwner, m.symbols[key].budgetBytes)
			delete(m.symbols, key)
		}
	}
}

// memoDisabledFromEnv latches whether the nested per-request RPC trace is
// armed; see cclsNestedRPCTimingEnv.
func memoDisabledFromEnv() bool {
	return os.Getenv(cclsNestedRPCTimingEnv) != ""
}

// completionListClone returns a shallow copy so a caller cannot mutate the
// cached slice. Item payloads are shared: every caller treats completion
// results as read-only projection input.
func completionListClone(list languages.CompletionList) languages.CompletionList {
	cloned := languages.CompletionList{IsIncomplete: list.IsIncomplete}
	if list.Items != nil {
		cloned.Items = make([]languages.CompletionItem, len(list.Items))
		copy(cloned.Items, list.Items)
	}
	return cloned
}

// documentSymbolsClone returns a shallow copy of the top level; children
// slices are shared under the same read-only convention.
func documentSymbolsClone(symbols []languages.DocumentSymbol) []languages.DocumentSymbol {
	if symbols == nil {
		return nil
	}
	cloned := make([]languages.DocumentSymbol, len(symbols))
	copy(cloned, symbols)
	return cloned
}
