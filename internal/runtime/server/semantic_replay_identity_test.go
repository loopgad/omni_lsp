package server

import (
	"context"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestCurrentSemanticIndexIdentityRejectsUnpinnedFixture(t *testing.T) {
	s, _, _ := newSemanticQueryFixture(t, 1)
	_, err := s.CurrentSemanticIndexIdentity(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tool identity") {
		t.Fatalf("identity without pinned extractor tools: %v", err)
	}
}

func TestSemanticResponseObserverReceivesRequestIndexEvidence(t *testing.T) {
	s, mainURI, _, line, column := newPersistentLocationsServer(t, &persistentLocationsFixture{referenceCoverage: model.Complete})
	var observedID jsonrpc.RequestID
	var observed []identity.Evidence
	s.SetSemanticResponseObserver(func(id jsonrpc.RequestID, evidence []identity.Evidence) {
		observedID = id
		observed = append([]identity.Evidence(nil), evidence...)
	})
	callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 92)
	if observedID.Num != 92 || len(observed) != 1 || observed[0].Kind != identity.EvidenceIndex || observed[0].IndexGen == 0 {
		t.Fatalf("request-bound semantic evidence = id=%+v, evidence=%+v", observedID, observed)
	}
}
