package identity

import (
	"testing"
)

func TestEvidenceKindString(t *testing.T) {
	cases := []struct {
		kind     EvidenceKind
		expected string
	}{
		{EvidenceLexical, "lexical"},
		{EvidenceSyntax, "syntax"},
		{EvidenceIndex, "index"},
		{EvidenceSemantic, "semantic"},
		{EvidenceCompiler, "compiler"},
		{EvidenceKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.expected {
			t.Errorf("EvidenceKind(%d).String() = %q, want %q", c.kind, got, c.expected)
		}
	}
}

func TestResultStatusString(t *testing.T) {
	cases := []struct {
		status   ResultStatus
		expected string
	}{
		{ResultExact, "exact"},
		{ResultPartial, "partial"},
		{ResultUnknown, "unknown"},
		{ResultUnavailable, "unavailable"},
		{ResultStatus(99), "invalid"},
	}
	for _, c := range cases {
		if got := c.status.String(); got != c.expected {
			t.Errorf("ResultStatus(%d).String() = %q, want %q", c.status, got, c.expected)
		}
	}
}

func TestSnapshotIDString(t *testing.T) {
	id := SnapshotID{Workspace: "ws1", Revision: 42}
	got := id.String()
	want := "snap{ws1:42}"
	if got != want {
		t.Errorf("SnapshotID.String() = %q, want %q", got, want)
	}
}

func TestSemanticResultConstructors(t *testing.T) {
	ev := []Evidence{{Kind: EvidenceCompiler, Snapshot: SnapshotID{Workspace: "ws", Revision: 1}}}

	exact := NewExactResult("value", ev)
	if exact.Status != ResultExact {
		t.Errorf("NewExactResult status = %v, want ResultExact", exact.Status)
	}
	if exact.Completeness != Complete {
		t.Errorf("NewExactResult completeness = %v, want Complete", exact.Completeness)
	}

	partial := NewPartialResult("value", ev)
	if partial.Status != ResultPartial {
		t.Errorf("NewPartialResult status = %v, want ResultPartial", partial.Status)
	}

	unknown := NewUnknownResult[string](ev)
	if unknown.Status != ResultUnknown {
		t.Errorf("NewUnknownResult status = %v, want ResultUnknown", unknown.Status)
	}

	unavail := NewUnavailableResult[string](ev)
	if unavail.Status != ResultUnavailable {
		t.Errorf("NewUnavailableResult status = %v, want ResultUnavailable", unavail.Status)
	}
	// With evidence, ResultUnavailable is a valid explicit unknown, NOT stale.
	if unavail.IsStale() {
		t.Error("NewUnavailableResult with evidence should not be considered stale (valid explicit unknown)")
	}

	// Without evidence, ResultUnavailable IS stale.
	emptyUnavail := NewUnavailableResult[string](nil)
	if !emptyUnavail.IsStale() {
		t.Error("NewUnavailableResult without evidence should be considered stale")
	}
}

func TestSemanticResultNotStaleWhenHasEvidence(t *testing.T) {
	// ResultUnavailable with evidence is NOT stale (it's a valid explicit unknown).
	ev := []Evidence{{Kind: EvidenceCompiler, Snapshot: SnapshotID{Workspace: "ws", Revision: 1}}}
	r := NewUnavailableResult[string](ev)
	if r.IsStale() {
		t.Error("ResultUnavailable with evidence should not be considered stale")
	}
}
