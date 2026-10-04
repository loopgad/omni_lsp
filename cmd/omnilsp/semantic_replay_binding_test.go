package main

import (
	"testing"

	"github.com/omnilsp/omni/internal/identity"
)

func TestIndexedSemanticResponseGenerationRequiresOnlyOnePinnedGeneration(t *testing.T) {
	tests := []struct {
		name     string
		evidence []identity.Evidence
		want     uint64
		ok       bool
	}{
		{
			name:     "single generation",
			evidence: []identity.Evidence{{Kind: identity.EvidenceIndex, IndexGen: 12}},
			want:     12,
			ok:       true,
		},
		{
			name: "same generation across sources",
			evidence: []identity.Evidence{
				{Kind: identity.EvidenceIndex, IndexGen: 12},
				{Kind: identity.EvidenceIndex, IndexGen: 12},
			},
			want: 12,
			ok:   true,
		},
		{
			name: "mixed live and index evidence",
			evidence: []identity.Evidence{
				{Kind: identity.EvidenceIndex, IndexGen: 12},
				{Kind: identity.EvidenceCompiler, IndexGen: 12},
			},
		},
		{
			name:     "missing generation",
			evidence: []identity.Evidence{{Kind: identity.EvidenceIndex}},
		},
		{
			name: "conflicting generations",
			evidence: []identity.Evidence{
				{Kind: identity.EvidenceIndex, IndexGen: 12},
				{Kind: identity.EvidenceIndex, IndexGen: 13},
			},
		},
		{name: "empty evidence"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			generation, ok := indexedSemanticResponseGeneration(tc.evidence)
			if generation != tc.want || ok != tc.ok {
				t.Fatalf("indexed generation = (%d, %t), want (%d, %t)", generation, ok, tc.want, tc.ok)
			}
		})
	}
}
