package model

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
)

type testView struct{ id Identity }

func (v testView) Identity() Identity                                 { return v.id }
func (testView) Walk(context.Context, string, func(File) error) error { return nil }
func (testView) Read(context.Context, string) (io.ReadCloser, error)  { return nil, nil }

func validRequest() (Request, Report) {
	identityValue := Identity{Workspace: "ws", DiskDigest: "sha256:abc", SnapshotRev: 7}
	scope := Scope{ID: "go:module:root", Language: "go", RootURI: "file:///repo"}
	scope.BuildContext = ComputeBuildContextID(scope, "go/packages", "go1.26.1", "go1.26.1/windows-amd64", nil)
	request := Request{
		View:   testView{id: identityValue},
		Scopes: []Scope{scope},
		Provenance: map[string]Provenance{
			scope.ID: {
				SchemaVersion: SchemaVersion, Identity: identityValue, Scope: scope,
				Extractor: "go/packages", ExtractorVer: "go1.26.1",
				Backend:   identity.BackendID{Language: "go", Name: "gopls-compatible-export"},
				Toolchain: "go1.26.1/windows-amd64",
			},
		},
	}
	report := Report{Identity: identityValue}
	for _, fact := range RequiredFactKinds {
		report.Coverage = append(report.Coverage, Coverage{ScopeID: scope.ID, Fact: fact, State: Complete})
	}
	return request, report
}

func TestValidateReportRequiresEveryScopeAndFact(t *testing.T) {
	request, report := validRequest()
	if err := ValidateReport(request, report); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}

	t.Run("omitted fact", func(t *testing.T) {
		bad := report
		bad.Coverage = append([]Coverage(nil), report.Coverage[:len(report.Coverage)-1]...)
		if err := ValidateReport(request, bad); !errors.Is(err, ErrMissingCoverage) {
			t.Fatalf("ValidateReport error = %v, want ErrMissingCoverage", err)
		}
	})
	t.Run("unknown without reason", func(t *testing.T) {
		bad := report
		bad.Coverage = append([]Coverage(nil), report.Coverage...)
		bad.Coverage[0].State = Unknown
		if err := ValidateReport(request, bad); !errors.Is(err, ErrInvalidCoverage) {
			t.Fatalf("ValidateReport error = %v, want ErrInvalidCoverage", err)
		}
	})
	t.Run("duplicate fact", func(t *testing.T) {
		bad := report
		bad.Coverage = append(append([]Coverage(nil), report.Coverage...), report.Coverage[0])
		if err := ValidateReport(request, bad); !errors.Is(err, ErrDuplicateCoverage) {
			t.Fatalf("ValidateReport error = %v, want ErrDuplicateCoverage", err)
		}
	})
	t.Run("wrong snapshot", func(t *testing.T) {
		bad := report
		bad.Identity.SnapshotRev++
		if err := ValidateReport(request, bad); !errors.Is(err, ErrSnapshotMismatch) {
			t.Fatalf("ValidateReport error = %v, want ErrSnapshotMismatch", err)
		}
	})
	t.Run("missing provenance", func(t *testing.T) {
		bad := request
		bad.Provenance = nil
		if err := ValidateReport(bad, report); !errors.Is(err, ErrInvalidProvenance) {
			t.Fatalf("ValidateReport error = %v, want ErrInvalidProvenance", err)
		}
	})
	t.Run("unpinned extractor tool", func(t *testing.T) {
		badReq := request
		badReq.Provenance = map[string]Provenance{}
		for scopeID, provenance := range request.Provenance {
			provenance.Tools = []ToolIdentity{{Name: "clang-indexer", Path: "C:/tools/indexer.exe", Version: "1.0", SHA256: "not-a-hash"}}
			scope := request.Scopes[0]
			scope.BuildContext = ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
			provenance.Scope = scope
			badReq.Scopes = []Scope{scope}
			badReq.Provenance[scopeID] = provenance
		}
		if err := ValidateReport(badReq, report); !errors.Is(err, ErrInvalidProvenance) {
			t.Fatalf("ValidateReport error = %v, want ErrInvalidProvenance", err)
		}
	})
}

func TestValidateReportRequiresToolsOnlyForAttestedFacts(t *testing.T) {
	request, report := validRequest()
	tool := ToolIdentity{Name: "extractor", Path: "C:/tools/extractor.exe", Version: "1", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	provenance := request.Provenance[request.Scopes[0].ID]
	provenance.Tools = []ToolIdentity{tool}
	request.Scopes[0].BuildContext = ComputeBuildContextID(request.Scopes[0], provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = request.Scopes[0]
	request.Provenance[request.Scopes[0].ID] = provenance
	for i := range report.Coverage {
		report.Coverage[i].State = Unknown
		report.Coverage[i].Reason = "configuration incomplete before tool launch"
	}
	if err := ValidateReport(request, report); err != nil {
		t.Fatalf("unknown scope without launched tools rejected: %v", err)
	}
	report.Coverage[0].State = IncompleteKnownSubset
	if err := ValidateReport(request, report); !errors.Is(err, ErrInvalidProvenance) {
		t.Fatalf("attested facts without tool record: got %v, want provenance error", err)
	}
}

func TestComputeBuildContextIDBindsConfigAndCanonicalizesMaps(t *testing.T) {
	base := Scope{
		ID: "ts:project:root", Language: "typescript", RootURI: "file:///repo",
		Build: BuildInputs{Environment: map[string]string{"TS_NODE": "1", "NODE_ENV": "test"}, Options: map[string]string{"strict": "true"}, Arguments: []string{"--project", "tsconfig.json"}},
	}
	tools := []ToolIdentity{{Name: "tsc", Path: "C:/tools/tsc.js", Version: "6.0.3", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	first := ComputeBuildContextID(base, "typescript-api", "1", "node-24", tools)
	base.Build.Environment = map[string]string{"NODE_ENV": "test", "TS_NODE": "1"}
	second := ComputeBuildContextID(base, "typescript-api", "1", "node-24", tools)
	if first != second {
		t.Fatalf("map iteration changed build identity: %q != %q", first, second)
	}
	base.Build.Options["strict"] = "false"
	if got := ComputeBuildContextID(base, "typescript-api", "1", "node-24", tools); got == first {
		t.Fatal("compiler option change did not change build identity")
	}
}
