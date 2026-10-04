package main

import (
	"context"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/runtime/server"
)

type semanticResponseBinder interface {
	RegisterSemanticIdentity(jsonrpc.RequestID, replay.SemanticIdentity) error
	BindSemanticResponse(jsonrpc.RequestID, bool, uint64, string) error
}

// indexedSemanticResponseBinder records provenance only when one exact index
// generation supplied the response. Live and mixed-source answers remain
// unverified until their complete tool/source provenance can be captured.
func indexedSemanticResponseBinder(srv *server.Server, binder semanticResponseBinder) func(jsonrpc.RequestID, []identity.Evidence) {
	return func(id jsonrpc.RequestID, evidence []identity.Evidence) {
		generation, ok := indexedSemanticResponseGeneration(evidence)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		current, err := srv.CurrentSemanticIndexIdentity(ctx)
		cancel()
		if err != nil || current.Generation != generation {
			return
		}
		if err := binder.RegisterSemanticIdentity(id, *replaySemanticIdentity(current)); err != nil {
			warn("semantic replay identity unavailable: %v", err)
			return
		}
		if err := binder.BindSemanticResponse(id, true, current.Generation, current.IndexContentDigest); err != nil {
			warn("semantic replay response binding unavailable: %v", err)
		}
	}
}

func indexedSemanticResponseGeneration(evidence []identity.Evidence) (uint64, bool) {
	if len(evidence) == 0 {
		return 0, false
	}
	var generation uint64
	for _, item := range evidence {
		if item.Kind != identity.EvidenceIndex || item.IndexGen == 0 {
			return 0, false
		}
		if generation != 0 && generation != uint64(item.IndexGen) {
			return 0, false
		}
		generation = uint64(item.IndexGen)
	}
	return generation, generation != 0
}
