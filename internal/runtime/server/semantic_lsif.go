package server

import (
	"context"

	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/index/model"
)

// SemanticLSIFImportResult identifies the generation created by a local LSIF
// import. It mirrors SemanticSCIPImportResult; coverage stays attached to the
// generation and honestly reports the fact families the omnilsp LSIF subset
// cannot prove (LSIF encodes no model edge kinds at all).
type SemanticLSIFImportResult struct {
	Generation uint64           `json:"generation"`
	ScopeID    string           `json:"scopeId"`
	Coverage   []model.Coverage `json:"coverage"`
}

// ExportSemanticLSIF exports one scope from the current fresh semantic
// generation as LSIF JSON Lines. It shares the SCIP export path's freshness
// guards (openSemanticExportGeneration) and refuses the semantic edge kinds
// the omnilsp LSIF subset cannot represent; use
// ExportSemanticLSIFWithLosses to omit them explicitly.
func (s *Server) ExportSemanticLSIF(ctx context.Context, scopeID string) ([]byte, error) {
	data, _, err := s.exportSemanticLSIF(ctx, scopeID, false)
	return data, err
}

// ExportSemanticLSIFWithLosses explicitly enables omission of the semantic
// edge kinds the omnilsp LSIF subset cannot encode. Losses are returned as
// structured counts and embedded in the LSIF metaData.toolInfo.args by the
// interop exporter. The default export remains strict.
func (s *Server) ExportSemanticLSIFWithLosses(ctx context.Context, scopeID string) ([]byte, []interop.SemanticLSIFExportLoss, error) {
	return s.exportSemanticLSIF(ctx, scopeID, true)
}

func (s *Server) exportSemanticLSIF(ctx context.Context, scopeID string, allowLossy bool) ([]byte, []interop.SemanticLSIFExportLoss, error) {
	gen, err := s.openSemanticExportGeneration(ctx, scopeID, "LSIF")
	if err != nil {
		return nil, nil, err
	}
	defer gen.close()
	var data []byte
	var losses []interop.SemanticLSIFExportLoss
	if allowLossy {
		data, losses, err = interop.ExportLSIFSemanticScopeWithLosses(ctx, gen.reader, scopeID)
	} else {
		data, err = interop.ExportLSIFSemanticScope(ctx, gen.reader, scopeID)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := gen.verifyUnchanged(ctx, s, "LSIF"); err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

// ImportSemanticLSIF imports a bounded LSIF JSON Lines stream into an empty
// index store, mirroring ImportSemanticSCIP: the same empty-store, trust,
// scope-ownership and freshness guards apply, and the imported generation
// never claims completeness the format cannot prove.
func (s *Server) ImportSemanticLSIF(ctx context.Context, language, scopeID string, data []byte) (SemanticLSIFImportResult, error) {
	generation, coverage, err := s.importSemanticScope(ctx, language, scopeID, data, "LSIF", interop.ImportLSIFToModel)
	if err != nil {
		return SemanticLSIFImportResult{}, err
	}
	return SemanticLSIFImportResult{Generation: generation, ScopeID: scopeID, Coverage: coverage}, nil
}
