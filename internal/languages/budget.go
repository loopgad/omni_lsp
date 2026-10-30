package languages

// §F14 backend-buffer budget wiring (DEF-BUDWIRE, F14 half).
//
// goal.md §F14: "Backend memory MUST count against workspace/global policy."
// The manager this file latches is internal/runtime/budget.Manager; its F13
// counterpart lives in internal/runtime/server (index ladder) and this file
// is deliberately independent of it, exactly as the two were kept separate
// when the budget package was written.
//
// Budget source (the wiring decision registered for DEF-BUDWIRE): the
// process environment, not config files. The F13 half set the precedent
// (OMNILSP_INDEX_MEMORY_BUDGET_BYTES, default off), and a config path would
// have to cross the server construction chain this lane must not touch.
//
//   - OMNILSP_BACKEND_BUDGET_BYTES            global hard limit (bytes)
//   - OMNILSP_BACKEND_QUOTA_<OWNER>_BYTES     per-owner hard quota (bytes)
//
// With every variable unset the manager stays nil and every helper below is
// a passthrough — zero behavior change (X10 red line). With any variable set
// the manager exists and refusals are fail-closed AND counted, never silent:
// each wiring site pairs its refusal with a visible effect (dropped
// publication, failed command, truncated capture, skipped memo store) plus a
// per-owner refusal counter readable through BackendBufferRefusals.
//
// Owner names are the backend identities already used on the wire:
// "clangd", "pyright", "tsserver", "rust-analyzer" (nested.Conn cfg.Name),
// "golang-native" (identity.BackendID), and "ccls-memo" for the ccls memo
// tables, which have no connection of their own. Unknown owners count only
// against the global limit.

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/omnilsp/omni/internal/runtime/budget"
)

// backendBudgetOwners enumerates every owner this lane wires. A new backend
// must be added here to be reachable by OMNILSP_BACKEND_QUOTA_<OWNER>_BYTES.
var backendBudgetOwners = []string{
	"clangd",
	"pyright",
	"tsserver",
	"rust-analyzer",
	"golang-native",
	"ccls-memo",
}

// backendBudgetState is latched once per process on first use. Production
// code never resets it; the reset hook exists so a test binary can pin the
// environment per test case the way t.Setenv requires.
type backendBudgetState struct {
	mu       sync.Mutex
	manager  *budget.Manager
	resolved bool
	refusals sync.Map // owner string -> *atomic.Uint64
}

var backendBudget backendBudgetState

// BackendBudget returns the process-wide F14 manager, or nil when no budget
// variable is configured. The latch is resolved once; later environment
// changes are ignored in production.
func BackendBudget() *budget.Manager {
	backendBudget.mu.Lock()
	defer backendBudget.mu.Unlock()
	if !backendBudget.resolved {
		backendBudget.manager = newBackendBudgetFromEnv()
		backendBudget.resolved = true
	}
	return backendBudget.manager
}

// newBackendBudgetFromEnv builds the manager from the environment. The
// manager exists when the global limit or ANY quota variable is set; with
// nothing set it returns nil (default off, X10). An unset global limit is
// represented as MaxInt64 so the Manager's absolute-hard invariant stays
// meaningful while per-owner quotas do the binding.
//
// Quota variables map back to owners by exact derivation from the known
// owner list; a variable for an unknown owner (an arbitrary nested.Conn
// name, a future plugin lane) is de-normalized (lower-case, '_'→'-') so its
// owner still gets its own quota instead of silently collapsing into the
// global-only accounting.
func newBackendBudgetFromEnv() *budget.Manager {
	hard, hardSet := budget.ParsePositiveBytes(budget.EnvBackendBudgetBytes)
	quotaVars := budget.EnvironQuotaLimits()
	if !hardSet && len(quotaVars) == 0 {
		return nil
	}
	if !hardSet {
		hard = int64(^uint64(0) >> 1) // MaxInt64: no global ceiling configured
	}
	m := budget.New(0, hard)
	known := make(map[string]string, len(backendBudgetOwners))
	for _, owner := range backendBudgetOwners {
		known[budget.QuotaEnvName(owner)] = owner
	}
	for envName, limit := range quotaVars {
		owner, ok := known[envName]
		if !ok {
			owner = deNormalizeQuotaOwner(envName)
		}
		m.SetQuota(owner, limit)
	}
	return m
}

// deNormalizeQuotaOwner maps a quota variable's middle section back to an
// owner name: OMNILSP_BACKEND_QUOTA_FAKE_LSP_BYTES → "fake-lsp". The mapping
// is the inverse of budget.QuotaEnvName for the repo's owner set and a
// deterministic best effort for everything else.
func deNormalizeQuotaOwner(envName string) string {
	middle := strings.TrimSuffix(strings.TrimPrefix(envName, budget.EnvBackendQuotaPrefix), budget.EnvBackendQuotaSuffix)
	return strings.ToLower(strings.ReplaceAll(middle, "_", "-"))
}

// ReserveBackendBuffer reserves n staged bytes for owner against the F14
// budget. A nil manager (no budget configured) admits everything. A false
// return is a real refusal: the caller must not allocate and must surface
// the refusal (the wiring sites drop, fail, or truncate visibly) — calling
// NoteBackendBufferRefusal is how the refusal becomes countable.
func ReserveBackendBuffer(owner string, n int64) bool {
	m := BackendBudget()
	if m == nil {
		return true
	}
	return m.Reserve(owner, n) == nil
}

// ReleaseBackendBuffer returns n previously reserved bytes for owner. With a
// nil manager it is a no-op.
func ReleaseBackendBuffer(owner string, n int64) {
	m := BackendBudget()
	if m == nil {
		return
	}
	m.Release(owner, n)
}

// NoteBackendBufferRefusal counts one refused allocation for owner. Every
// wiring site calls this when ReserveBackendBuffer declines, so an operator
// can see fail-closed events instead of guessing why output shrank.
func NoteBackendBufferRefusal(owner string) {
	value, _ := backendBudget.refusals.LoadOrStore(owner, &atomic.Uint64{})
	value.(*atomic.Uint64).Add(1)
}

// BackendBufferRefusals reports how many allocations were refused for owner
// so far in this process.
func BackendBufferRefusals(owner string) uint64 {
	value, ok := backendBudget.refusals.Load(owner)
	if !ok {
		return 0
	}
	return value.(*atomic.Uint64).Load()
}

// BackendBufferCap returns the tightened inbound frame cap for owner's stdio
// pipe buffers: the per-owner quota when it is below the codec default, else
// 0 meaning "leave the codec default in place". With no budget configured it
// always returns 0 (X10).
func BackendBufferCap(owner string) int {
	m := BackendBudget()
	if m == nil {
		return 0
	}
	limit, ok := m.Quota(owner)
	if !ok || limit <= 0 || limit >= int64(maxCodecDefaultFrameCap) {
		return 0
	}
	return int(limit)
}

// maxCodecDefaultFrameCap mirrors jsonrpc.DefaultMaxMessageSize. It is
// restated here rather than imported to keep this file's dependency surface
// on the protocol package empty; a test pins the two together.
const maxCodecDefaultFrameCap = 64 * 1024 * 1024

// ResetBackendBudgetForTest clears the latch so a later BackendBudget call
// re-reads the environment. Production code must not call it.
func ResetBackendBudgetForTest() {
	backendBudget.mu.Lock()
	backendBudget.manager = nil
	backendBudget.resolved = false
	backendBudget.mu.Unlock()
}

// BudgetedBuffer is a bytes.Buffer whose growth is charged to the F14 budget
// as it happens: every Write reserves len(p) bytes first, and a refused
// reservation fails the write (counted by the caller's error path) instead of
// silently swallowing the payload. With no budget configured it behaves
// exactly like bytes.Buffer.
//
// The reservation is held from the write until Reset, matching the real
// lifetime of the captured bytes: the buffer content is resident between the
// two commands that share it in the golang module-graph capture.
type BudgetedBuffer struct {
	buf   bytes.Buffer
	owner string
	held  int64
}

// NewBudgetedBuffer returns a budgeted capture buffer for owner. owner may
// be empty only in tests; production callers pass the backend identity.
func NewBudgetedBuffer(owner string) *BudgetedBuffer {
	return &BudgetedBuffer{owner: owner}
}

// Write appends p, reserving its length against the budget first. A refusal
// returns an error and writes nothing: the caller's command fails visibly
// rather than producing a truncated result presented as complete.
func (b *BudgetedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !ReserveBackendBuffer(b.owner, int64(len(p))) {
		NoteBackendBufferRefusal(b.owner)
		return 0, &BudgetRefusalError{Owner: b.owner, Bytes: len(p)}
	}
	b.held += int64(len(p))
	return b.buf.Write(p)
}

// Len returns the captured byte count.
func (b *BudgetedBuffer) Len() int { return b.buf.Len() }

// Read delegates to the underlying buffer so a BudgetedBuffer is an
// io.Reader drop-in (the golang module-graph capture feeds it to
// json.NewDecoder and bufio.NewScanner). Reads consume the capture exactly
// as bytes.Buffer does; accounting is untouched.
func (b *BudgetedBuffer) Read(p []byte) (int, error) { return b.buf.Read(p) }

// Bytes returns the captured bytes.
func (b *BudgetedBuffer) Bytes() []byte { return b.buf.Bytes() }

// String returns the captured bytes as a string.
func (b *BudgetedBuffer) String() string { return b.buf.String() }

// Reset discards the capture and releases its reservation.
func (b *BudgetedBuffer) Reset() {
	ReleaseBackendBuffer(b.owner, b.held)
	b.held = 0
	b.buf.Reset()
}

// BudgetRefusalError reports a refused F14 buffer reservation. It is a plain
// error: the sites that surface it wrap it into their own failure messages
// (partial facts, truncated-capture markers), which keeps the budget wiring
// decoupled from the backend error taxonomy.
type BudgetRefusalError struct {
	Owner string
	Bytes int
}

func (e *BudgetRefusalError) Error() string {
	return "backend buffer budget refused " + strconv.Itoa(e.Bytes) +
		" bytes for owner " + strconv.Quote(e.Owner)
}

// budgetedReadChunk bounds the transient staging allocation of
// ReadBudgetedAll per read step.
const budgetedReadChunk = 32 * 1024

// ReadBudgetedAll reads r to completion while charging the staged bytes to
// the F14 budget. A refused reservation stops the read and appends a visible
// truncation marker, so a consumer that prints the capture sees that it is
// incomplete; the refusal is counted. The reservation covers only the read
// window — the return value is transient staging the callers embed into
// error strings or discard, not resident state (the resident accounting
// sites are the diagnostics store and the memo tables). With no budget
// configured this is io.ReadAll.
func ReadBudgetedAll(owner string, r io.Reader) ([]byte, error) {
	m := BackendBudget()
	if m == nil {
		return io.ReadAll(r)
	}
	var captured int64
	var out bytes.Buffer
	buf := make([]byte, budgetedReadChunk)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if !ReserveBackendBuffer(owner, int64(n)) {
				NoteBackendBufferRefusal(owner)
				// The chunks captured so far leave with the return value;
				// their reservation must leave with them.
				ReleaseBackendBuffer(owner, captured)
				out.WriteString("\n[omnilsp: capture truncated at the F14 backend buffer budget]")
				return out.Bytes(), nil
			}
			captured += int64(n)
			out.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			ReleaseBackendBuffer(owner, captured)
			return out.Bytes(), rerr
		}
	}
	ReleaseBackendBuffer(owner, captured)
	return out.Bytes(), nil
}
