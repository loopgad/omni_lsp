package conformance

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPERF2_S18GateFailsClosedWithoutCandidate pins the PERF-2 hard opt-in
// contract for the S18/S19 real-process acceptance run: OMNILSP_ACCEPTANCE
// must never let the acceptance test earn PERF-2 credit from a silently
// skipped run (the DEF-S21CORPUS lesson). The =required mode therefore has to
// fail closed — with every candidate-binding variable stripped and no
// OMNILSP_BIN, TestS18S19_RealProcessPerformanceAcceptance must t.Fatalf with
// the missing-candidate message instead of skipping (exit 0) or falling back
// to a self-built candidate.
func TestPERF2_S18GateFailsClosedWithoutCandidate(t *testing.T) {
	if testing.Short() {
		t.Skip("guard shells out to the go toolchain; skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./test/perf", "-run", "^TestS18S19_RealProcessPerformanceAcceptance$", "-count=1", "-timeout", "15m")
	cmd.Dir = moduleRoot()
	// Strip every candidate- and acceptance-bound variable the outer process
	// may carry, then opt the inner run in: with the mode required and no
	// candidate binary, the S18/S19 test must fail (t.Fatalf), not skip and
	// not self-build.
	base := os.Environ()
	strip := map[string]bool{
		"OMNILSP_BIN": true, "OMNILSP_CANDIDATE_SHA256": true, "OMNILSP_RUN_ID": true,
		"OMNILSP_PERF_REPORT": true, "OMNILSP_ACCEPTANCE": true,
		"OMNILSP_S18_POLICY": true, "OMNILSP_S18_MODE": true,
		"OMNILSP_S18_STABLE_MODE": true, "OMNILSP_S18_FILTER": true,
	}
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && strip[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "OMNILSP_ACCEPTANCE=required")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("S18 gate did not fail closed without a candidate; output tail:\n%s", tailOf(string(out), 400))
	}
	if !strings.Contains(string(out), "opted in (required) but no frozen candidate") {
		t.Fatalf("S18 gate failure is not the missing-candidate t.Fatalf (tree may not compile); output tail:\n%s", tailOf(string(out), 400))
	}
}
