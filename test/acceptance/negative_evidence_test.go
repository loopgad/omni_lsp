package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type negativeEvidenceClient struct {
	status, completeness string
	missing, malformed   bool
	err                  error
}

func (c negativeEvidenceClient) RequestContext(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method != "omnilsp/resultMeta" {
		return nil, errors.New("unexpected request")
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.malformed {
		return json.RawMessage(`{`), nil
	}
	if c.missing {
		return json.RawMessage(`[]`), nil
	}
	request := params.(map[string]string)
	return json.Marshal([]map[string]string{
		{"method": request["method"], "status": "exact", "completeness": "complete"},
		{"method": request["method"], "status": c.status, "completeness": c.completeness},
	})
}

func TestNegativeEvidenceRejectsUnknownPartialAndUnavailableResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client negativeEvidenceClient
		passed bool
	}{
		{"verified", negativeEvidenceClient{status: "exact", completeness: "complete"}, true},
		{"unknown", negativeEvidenceClient{status: "unknown", completeness: "unknown"}, false},
		{"partial", negativeEvidenceClient{status: "partial", completeness: "known_subset"}, false},
		{"incomplete_exact", negativeEvidenceClient{status: "exact", completeness: "unknown"}, false},
		{"unavailable", negativeEvidenceClient{status: "unavailable", completeness: "unknown"}, false},
		{"missing", negativeEvidenceClient{missing: true}, false},
		{"malformed", negativeEvidenceClient{malformed: true}, false},
		{"process_error", negativeEvidenceClient{err: errors.New("process exited")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := semanticVerifyNegativeEvidence(tc.client, "file:///workspace/query.ts")
			if (err == nil) != tc.passed {
				t.Fatalf("passed=%v err=%v", tc.passed, err)
			}
		})
	}
}
