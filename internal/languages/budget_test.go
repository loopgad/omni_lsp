package languages

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// The BackendBufferCap sentinel logic restates the codec default locally;
// this pin keeps the two from drifting apart.
func TestMaxCodecDefaultFrameCapMirror(t *testing.T) {
	if maxCodecDefaultFrameCap != jsonrpc.DefaultMaxMessageSize {
		t.Fatalf("maxCodecDefaultFrameCap = %d, want jsonrpc.DefaultMaxMessageSize %d", maxCodecDefaultFrameCap, jsonrpc.DefaultMaxMessageSize)
	}
}

// resetBudgetForTest pins a per-test budget environment: t.Setenv before the
// first use of the latch, plus a reset so previously latched state from
// another test cannot leak in.
func resetBudgetForTest(t *testing.T, pairs map[string]string) {
	t.Helper()
	for name, value := range pairs {
		t.Setenv(name, value)
	}
	ResetBackendBudgetForTest()
	t.Cleanup(ResetBackendBudgetForTest)
}

func resetBudgetUnset(t *testing.T) {
	t.Helper()
	t.Setenv(backendBudgetEnvProbeUnused, "")
	ResetBackendBudgetForTest()
	t.Cleanup(ResetBackendBudgetForTest)
}

// backendBudgetEnvProbeUnused keeps resetBudgetUnset honest about not
// configuring anything: no budget variable is set for this test process, so
// setting an empty probe value is a no-op on the environment surface.
const backendBudgetEnvProbeUnused = "OMNILSP_BACKEND_BUDGET_BYTES_PROBE_UNUSED"

func TestBackendBudgetUnconfiguredIsPassthrough(t *testing.T) {
	resetBudgetUnset(t)

	if m := BackendBudget(); m != nil {
		t.Fatalf("BackendBudget with no budget variables set = %v, want nil (X10: default off)", m)
	}
	if !ReserveBackendBuffer("clangd", 1<<30) {
		t.Fatal("unconfigured budget must admit every reservation")
	}
	if got := BackendBufferRefusals("clangd"); got != 0 {
		t.Fatalf("refusals recorded with no budget configured: %d", got)
	}
	if cap := BackendBufferCap("clangd"); cap != 0 {
		t.Fatalf("BackendBufferCap with no budget = %d, want 0 (codec default stays)", cap)
	}
}

func TestBackendBudgetGlobalHardLimitRejects(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "64",
	})

	m := BackendBudget()
	if m == nil {
		t.Fatal("global budget variable set but manager stayed nil")
	}
	if !ReserveBackendBuffer("clangd", 64) {
		t.Fatal("first reservation within the hard limit must be admitted")
	}
	if ReserveBackendBuffer("pyright", 1) {
		// Invariant 2: one owner's refusal never blocks another — but the
		// GLOBAL limit is shared, so this must refuse too.
		t.Fatal("global hard limit must bind across owners")
	}
	if got := BackendBufferRefusals("pyright"); got != 0 {
		t.Fatalf("refusal counter must only move via NoteBackendBufferRefusal, got %d", got)
	}
	NoteBackendBufferRefusal("pyright")
	if got := BackendBufferRefusals("pyright"); got != 1 {
		t.Fatalf("refusal counter after one note = %d, want 1", got)
	}
	ReleaseBackendBuffer("clangd", 64)
	if !ReserveBackendBuffer("pyright", 64) {
		t.Fatal("after release the same bytes must be reservable by another owner")
	}
}

func TestBackendBudgetPerOwnerQuotaIsolates(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_CLANGD_BYTES": "100",
	})

	m := BackendBudget()
	if m == nil {
		t.Fatal("per-owner quota set but manager stayed nil")
	}
	if ReserveBackendBuffer("clangd", 101) {
		t.Fatal("per-owner quota must refuse the over-limit reservation")
	}
	if !ReserveBackendBuffer("tsserver", 1<<20) {
		t.Fatal("a clangd quota refusal must not block tsserver (§F14 isolation)")
	}
	if !ReserveBackendBuffer("clangd", 100) {
		t.Fatal("exactly-at-quota reservation must be admitted")
	}
	if ReserveBackendBuffer("clangd", 1) {
		t.Fatal("one byte past the quota must be refused")
	}
}

func TestBackendBudgetInvalidValuesStayOff(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES":       "not-a-number",
		"OMNILSP_BACKEND_QUOTA_CLANGD_BYTES": "-3",
	})

	if m := BackendBudget(); m != nil {
		t.Fatalf("every configured variable was invalid; manager must stay nil, got %v", m)
	}
}

func TestBackendBufferCapTightensToOwnerQuota(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_CLANGD_BYTES": "4096",
		"OMNILSP_BACKEND_QUOTA_PYRIGHT_BYTES": "" + // unset
			"",
	})
	if got := BackendBufferCap("clangd"); got != 4096 {
		t.Fatalf("BackendBufferCap(clangd) = %d, want 4096", got)
	}
	if got := BackendBufferCap("tsserver"); got != 0 {
		t.Fatalf("owner without quota must keep the codec default (0 sentinel), got %d", got)
	}
	// A quota at or above the codec default must not tighten anything.
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_CLANGD_BYTES": "68719476736", // 64 GiB > 64 MiB default
	})
	if got := BackendBufferCap("clangd"); got != 0 {
		t.Fatalf("quota above the codec default must not tighten the cap, got %d", got)
	}
}

func TestBudgetedBufferRefusalFailsWriteAtomically(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "16",
	})

	buf := NewBudgetedBuffer("clangd")
	before := BackendBufferRefusals("clangd")
	if _, err := buf.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatalf("in-budget write failed: %v", err)
	}
	n, err := buf.Write([]byte("X"))
	if err == nil {
		t.Fatal("write past the hard limit must fail (fail-closed), got nil error")
	}
	if n != 0 {
		t.Fatalf("refused write must be atomic (0 bytes written), wrote %d", n)
	}
	var refusal *BudgetRefusalError
	if _, ok := err.(*BudgetRefusalError); !ok {
		t.Fatalf("refused write error = %T, want *BudgetRefusalError", err)
	}
	_ = refusal
	if got, want := buf.String(), "0123456789abcdef"; got != want {
		t.Fatalf("refused write mutated the capture: %q, want %q", got, want)
	}
	if got := BackendBufferRefusals("clangd"); got != before+1 {
		t.Fatalf("refusals(clangd) delta = %d, want 1 (was %d)", got-before, before)
	}
	buf.Reset()
	if !ReserveBackendBuffer("clangd", 16) {
		t.Fatal("Reset must release the held reservation")
	}
}

func TestBudgetedBufferUnconfiguredMatchesBytesBuffer(t *testing.T) {
	resetBudgetUnset(t)

	var plain bytes.Buffer
	buf := NewBudgetedBuffer("clangd")
	payload := strings.Repeat("omnilsp", 10000)
	if _, err := buf.Write([]byte(payload)); err != nil {
		t.Fatalf("unconfigured write failed: %v", err)
	}
	plain.WriteString(payload)
	if buf.String() != plain.String() {
		t.Fatal("unconfigured BudgetedBuffer must behave exactly like bytes.Buffer")
	}
}

func TestReadBudgetedAllTruncatesWithVisibleMarker(t *testing.T) {
	// The quota covers two 32KiB read chunks and refuses the third, so the
	// capture keeps its in-budget prefix and gains the visible marker.
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "65536",
	})

	payload := bytes.Repeat([]byte("a"), 100*1024)
	refusalsBefore := BackendBufferRefusals("pyright")
	out, err := ReadBudgetedAll("pyright", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("truncation is a visible marker, not an error: %v", err)
	}
	const marker = "\n[omnilsp: capture truncated at the F14 backend buffer budget]"
	if !strings.HasSuffix(string(out), marker) {
		t.Fatalf("truncated capture must carry the visible marker, got suffix %q", string(out[max(0, len(out)-80):]))
	}
	if len(out) != 65536+len(marker) {
		t.Fatalf("truncated capture = %d bytes, want the 65536 in-budget prefix plus the marker", len(out))
	}
	if !bytes.HasPrefix(out, bytes.Repeat([]byte("a"), 1024)) {
		t.Fatal("the in-budget prefix must survive the truncation")
	}
	if got := BackendBufferRefusals("pyright"); got != refusalsBefore+1 {
		t.Fatalf("refusals(pyright) delta = %d, want 1", got-refusalsBefore)
	}
	// The returned prefix and the marker leave with the caller, so the
	// read-window reservation must leave with them: usage back to zero.
	if manager := BackendBudget(); manager == nil {
		t.Fatal("budget configured but manager stayed nil")
	} else if total, _ := manager.Usage(); total != 0 {
		t.Fatalf("global usage after truncated read = %d, want 0 (transient staging must release)", total)
	}
}

func TestReadBudgetedAllUnconfiguredMatchesReadAll(t *testing.T) {
	resetBudgetUnset(t)

	payload := bytes.Repeat([]byte("b"), 100*1024)
	out, err := ReadBudgetedAll("pyright", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("unconfigured read failed: %v", err)
	}
	want, _ := io.ReadAll(bytes.NewReader(payload))
	if !bytes.Equal(out, want) {
		t.Fatal("unconfigured ReadBudgetedAll must match io.ReadAll byte for byte")
	}
}
