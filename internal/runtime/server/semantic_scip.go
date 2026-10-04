package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/trust"
)

const maxSCIPImportBytes = 64 << 20

// SemanticSCIPImportResult identifies the generation created by a local SCIP
// import. Coverage remains attached to the generation and is always
// incomplete where SCIP cannot prove scope completeness.
type SemanticSCIPImportResult struct {
	Generation uint64           `json:"generation"`
	ScopeID    string           `json:"scopeId"`
	Coverage   []model.Coverage `json:"coverage"`
}

// ExportSemanticSCIP exports one scope from the current fresh semantic
// generation. It refuses stale or unbound tool/build contexts and never
// materializes facts from an open-document overlay.
func (s *Server) ExportSemanticSCIP(ctx context.Context, scopeID string) ([]byte, error) {
	data, _, err := s.exportSemanticSCIP(ctx, scopeID, false)
	return data, err
}

// ExportSemanticSCIPWithLosses explicitly enables omission of SCIP-
// unrepresentable edge kinds. Losses are returned as structured counts and
// embedded in the SCIP metadata by the interop exporter. The default export
// remains strict.
func (s *Server) ExportSemanticSCIPWithLosses(ctx context.Context, scopeID string) ([]byte, []interop.SemanticSCIPExportLoss, error) {
	return s.exportSemanticSCIP(ctx, scopeID, true)
}

func (s *Server) exportSemanticSCIP(ctx context.Context, scopeID string, allowLossy bool) ([]byte, []interop.SemanticSCIPExportLoss, error) {
	if ctx == nil || scopeID == "" {
		return nil, nil, errors.New("semantic SCIP export requires a context and scope ID")
	}
	idx, _, reason := s.indexState()
	if idx == nil {
		return nil, nil, fmt.Errorf("semantic SCIP export: index unavailable: %s", reason)
	}
	if len(s.vfs.OpenFiles()) != 0 {
		return nil, nil, errors.New("semantic SCIP export requires closed documents so disk and overlay state cannot be mixed")
	}
	revision := s.currentRevision()
	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("semantic SCIP export: open generation: %w", err)
	}
	defer lease.Close()
	view := lease.Snapshot()
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return nil, nil, fmt.Errorf("semantic SCIP export: generation is not a verified semantic index: %w", err)
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != idx.workspaceID || metadata.Identity.SnapshotRev != 0 ||
		metadata.Identity.DiskDigest == "" || metadata.DiskDigest != metadata.Identity.DiskDigest {
		return nil, nil, errors.New("semantic SCIP export: generation identity is incomplete or belongs to another workspace")
	}
	diskView, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("semantic SCIP export: capture workspace: %w", err)
	}
	defer diskView.Close()
	if diskView.Identity().DiskDigest != metadata.DiskDigest ||
		!semanticPlanningStillMatches(ctx, s.semanticIndexBindings(), diskView, metadata) ||
		!semanticGenerationToolsStillMatch(ctx, metadata) {
		return nil, nil, errors.New("semantic SCIP export: generation is stale or its build/tool identity changed")
	}
	var data []byte
	var losses []interop.SemanticSCIPExportLoss
	if allowLossy {
		data, losses, err = interop.ExportSCIPSemanticScopeWithLosses(ctx, reader, scopeID)
	} else {
		data, err = interop.ExportSCIPSemanticScope(ctx, reader, scopeID)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil || s.currentRevision() != revision || len(s.vfs.OpenFiles()) != 0 {
		return nil, nil, errors.New("semantic SCIP export: workspace changed during export")
	}
	digest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil || digest != metadata.DiskDigest {
		return nil, nil, errors.New("semantic SCIP export: disk contents changed during export")
	}
	current, _, _ := s.indexState()
	if current != idx || !semanticGenerationToolsStillMatch(ctx, metadata) {
		return nil, nil, errors.New("semantic SCIP export: index or tool identity changed during export")
	}
	return data, losses, nil
}

// ImportSemanticSCIP imports a bounded SCIP stream into an empty index store.
// Requiring an empty store prevents a single-scope import from silently
// replacing other languages or project scopes in a committed generation.
func (s *Server) ImportSemanticSCIP(ctx context.Context, language, scopeID string, data []byte) (SemanticSCIPImportResult, error) {
	if ctx == nil || language == "" || scopeID == "" {
		return SemanticSCIPImportResult{}, errors.New("semantic SCIP import requires a context, language, and scope ID")
	}
	if len(data) == 0 || len(data) > maxSCIPImportBytes {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import input must be between 1 and %d bytes", maxSCIPImportBytes)
	}
	idx, policy, reason := s.indexState()
	if idx == nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: index unavailable: %s", reason)
	}
	if policy == nil || policy.State() == trust.StateUntrusted {
		return SemanticSCIPImportResult{}, errors.New("semantic SCIP import rejected: workspace is untrusted")
	}
	if !idx.buildMu.TryLock() {
		return SemanticSCIPImportResult{}, errors.New("semantic SCIP import rejected: another reindex is running")
	}
	defer idx.buildMu.Unlock()
	reindexLease, err := idx.store.AcquireReindexLease(ctx)
	if err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: acquire reindex lease: %w", err)
	}
	defer reindexLease.Close()
	if existing, err := idx.store.OpenSnapshot(ctx); err == nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import refuses to replace existing generation %d; choose an empty index directory", existing.ID)
	} else if !errors.Is(err, persistent.ErrNoGeneration) {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: inspect index: %w", err)
	}

	revision := s.currentRevision()
	view, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: capture workspace: %w", err)
	}
	defer view.Close()

	var selected *model.Request
	for _, binding := range s.semanticIndexBindings() {
		if binding.language != language {
			continue
		}
		request, planErr := binding.binding.planner.BuildIndexRequest(ctx, view, view.rootURI)
		if planErr != nil {
			return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: plan %s scope: %w", language, planErr)
		}
		if request.View == nil || request.View.Identity() != view.Identity() {
			return SemanticSCIPImportResult{}, errors.New("semantic SCIP import: scope planner did not bind to the captured workspace")
		}
		for _, scope := range request.Scopes {
			if scope.ID != scopeID {
				continue
			}
			provenance, ok := request.Provenance[scope.ID]
			if !ok || selected != nil {
				return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: scope %q is missing or ambiguous", scopeID)
			}
			selected = &model.Request{
				View: view, Scopes: []model.Scope{scope},
				Provenance: map[string]model.Provenance{scope.ID: provenance},
			}
		}
	}
	if selected == nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: no registered %s provider owns scope %q", language, scopeID)
	}
	build, err := idx.store.BeginBuild(ctx)
	if err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: begin generation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semantic.NewSink(ctx, build, *selected)
	if err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: create semantic sink: %w", err)
	}
	defer sink.Close()
	report, err := interop.ImportSCIPToModel(ctx, data, *selected, sink)
	if err != nil {
		return SemanticSCIPImportResult{}, err
	}
	if err := sink.Finalize(ctx, report); err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: finalize generation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return SemanticSCIPImportResult{}, err
	}
	currentDigest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil || currentDigest != view.Identity().DiskDigest || s.currentRevision() != revision {
		return SemanticSCIPImportResult{}, errors.New("semantic SCIP import: workspace changed during import")
	}
	if err := view.Close(); err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: release captured view: %w", err)
	}
	if err := build.Commit(ctx); err != nil {
		return SemanticSCIPImportResult{}, fmt.Errorf("semantic SCIP import: commit generation: %w", err)
	}
	committed = true
	return SemanticSCIPImportResult{Generation: build.GenerationID(), ScopeID: scopeID, Coverage: report.Coverage}, nil
}
