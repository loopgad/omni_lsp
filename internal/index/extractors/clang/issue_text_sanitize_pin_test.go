package clang

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Pin for round20/bugB bug 3: issue text reaches model.Coverage.Reason and
// the validated sink requires every stored string to be valid UTF-8
// (internal/index/semantic/semantic.go allStringsValid → ErrMalformedPayload
// fails the whole sink.Finalize). Byte truncation (`reason[:maxIssueBytes-3]
// + "..."`) splits multi-byte characters, and raw helper stderr bytes can
// carry invalid sequences straight into coverage reasons via err.Error().
// Every entry point into Coverage.Reason must emit valid UTF-8.

func TestAddContextIssueTruncatesOnRuneBoundary(t *testing.T) {
	analysis := analyzeState{}
	analysis.addContextIssue(strings.Repeat("汉", 200)) // 600 bytes of 3-byte runes

	got := analysis.issues[0]
	if !utf8.ValidString(got) {
		t.Fatalf("addContextIssue byte truncation split a multi-byte character: %q (valid=%v)", got, utf8.ValidString(got))
	}
	if len(got) > maxIssueBytes {
		t.Fatalf("addContextIssue truncation exceeded the byte budget: %d > %d", len(got), maxIssueBytes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("addContextIssue truncation lost its ellipsis suffix: %q", got)
	}
}

func TestAddContextIssueReplacesInvalidBytesFromRawStderr(t *testing.T) {
	analysis := analyzeState{}
	// Short enough to skip truncation entirely: the raw helper stderr bytes
	// must still be normalized before they can reach Coverage.Reason.
	raw := strings.Repeat("clang: error: 源文件内部错误\n", 10) + "\xff\xfe oops"
	analysis.addContextIssue(raw)

	if len(analysis.issues) != 1 {
		t.Fatalf("addContextIssue stored %d issues, want 1", len(analysis.issues))
	}
	if !utf8.ValidString(analysis.issues[0]) {
		t.Fatalf("addContextIssue passed raw invalid UTF-8 stderr bytes through: %q", analysis.issues[0])
	}
}

func TestAppendDiscoveryProblemTruncatesOnRuneBoundary(t *testing.T) {
	var problems []string
	appendDiscoveryProblem(&problems, strings.Repeat("汉", 200)) // 600 bytes

	if len(problems) != 1 {
		t.Fatalf("appendDiscoveryProblem stored %d problems, want 1", len(problems))
	}
	if !utf8.ValidString(problems[0]) {
		t.Fatalf("appendDiscoveryProblem byte truncation split a multi-byte character: %q", problems[0])
	}
	if len(problems[0]) > maxIssueBytes {
		t.Fatalf("appendDiscoveryProblem truncation exceeded the byte budget: %d > %d", len(problems[0]), maxIssueBytes)
	}
}

func TestCoverageReasonSanitizedAtBoundary(t *testing.T) {
	// Error paths that bypass addContextIssue (helper build failures etc.)
	// feed err.Error() — with embedded raw stderr — straight into
	// coverageBuilder.set, so the boundary itself must sanitize.
	coverage := newCoverage("scope-under-test")
	coverage.unavailableAll("compile libclang helper: boom (\xff\xfe raw stderr)")

	results := coverage.results()
	if len(results) == 0 {
		t.Fatal("coverage.results returned no entries")
	}
	for _, item := range results {
		if !utf8.ValidString(item.Reason) {
			t.Fatalf("Coverage.Reason carries invalid UTF-8 for fact %q: %q", item.Fact, item.Reason)
		}
	}
}

func TestSanitizeIssueTextKeepsShortAsciiIntact(t *testing.T) {
	const reason = "clang: error: linker command failed with exit code 1"
	if got := sanitizeIssueText(reason); got != reason {
		t.Fatalf("sanitizeIssueText rewrote a plain ASCII reason: %q", got)
	}
}
