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

const maxSemanticImportBytes = 64 << 20

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
	gen, err := s.openSemanticExportGeneration(ctx, scopeID, "SCIP")
	if err != nil {
		return nil, nil, err
	}
	defer gen.close()
	var data []byte
	var losses []interop.SemanticSCIPExportLoss
	if allowLossy {
		data, losses, err = interop.ExportSCIPSemanticScopeWithLosses(ctx, gen.reader, scopeID)
	} else {
		data, err = interop.ExportSCIPSemanticScope(ctx, gen.reader, scopeID)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := gen.verifyUnchanged(ctx, s, "SCIP"); err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

// semanticExportGeneration is one open, verified, fresh semantic generation
// reader shared by the SCIP and LSIF export paths. close releases the reader,
// the captured disk view and the snapshot lease in the same order the
// original per-path defers ran.
type semanticExportGeneration struct {
	idx      *indexService
	reader   *semantic.Reader
	metadata semantic.Metadata
	revision uint64
	close    func()
}

// openSemanticExportGeneration centralizes the export freshness guards both
// artifact formats rely on (label only shapes error text: "SCIP" or "LSIF"):
// an available index, no open overlay documents, a verified semantic
// generation bound to this workspace with a matching disk digest, and a
// re-captured view whose planning and tool identities still reproduce it.
func (s *Server) openSemanticExportGeneration(ctx context.Context, scopeID, label string) (*semanticExportGeneration, error) {
	if ctx == nil || scopeID == "" {
		return nil, errors.New("semantic " + label + " export requires a context and scope ID")
	}
	idx, _, reason := s.indexState()
	if idx == nil {
		return nil, fmt.Errorf("semantic %s export: index unavailable: %s", label, reason)
	}
	if len(s.vfs.OpenFiles()) != 0 {
		return nil, errors.New("semantic " + label + " export requires closed documents so disk and overlay state cannot be mixed")
	}
	revision := s.currentRevision()
	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return nil, fmt.Errorf("semantic %s export: open generation: %w", label, err)
	}
	view := lease.Snapshot()
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("semantic %s export: generation is not a verified semantic index: %w", label, err)
	}
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != idx.workspaceID || metadata.Identity.SnapshotRev != 0 ||
		metadata.Identity.DiskDigest == "" || metadata.DiskDigest != metadata.Identity.DiskDigest {
		_ = reader.Close()
		_ = lease.Close()
		return nil, errors.New("semantic " + label + " export: generation identity is incomplete or belongs to another workspace")
	}
	diskView, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		_ = reader.Close()
		_ = lease.Close()
		return nil, fmt.Errorf("semantic %s export: capture workspace: %w", label, err)
	}
	if diskView.Identity().DiskDigest != metadata.DiskDigest ||
		!semanticPlanningStillMatches(ctx, s.semanticIndexBindings(), diskView, metadata) ||
		!semanticGenerationToolsStillMatch(ctx, metadata) {
		_ = diskView.Close()
		_ = reader.Close()
		_ = lease.Close()
		return nil, errors.New("semantic " + label + " export: generation is stale or its build/tool identity changed")
	}
	return &semanticExportGeneration{
		idx:      idx,
		reader:   reader,
		metadata: metadata,
		revision: revision,
		close: func() {
			_ = diskView.Close()
			_ = reader.Close()
			_ = lease.Close()
		},
	}, nil
}

// verifyUnchanged re-runs the freshness guards after artifact bytes were
// produced: a workspace, disk, index or tool identity change voids the export
// instead of leaking stale facts.
func (g *semanticExportGeneration) verifyUnchanged(ctx context.Context, s *Server, label string) error {
	if err := ctx.Err(); err != nil || s.currentRevision() != g.revision || len(s.vfs.OpenFiles()) != 0 {
		return errors.New("semantic " + label + " export: workspace changed during export")
	}
	digest, err := semanticDiskDigest(ctx, g.idx.root, g.idx.dir)
	if err != nil || digest != g.metadata.DiskDigest {
		return errors.New("semantic " + label + " export: disk contents changed during export")
	}
	current, _, _ := s.indexState()
	if current != g.idx || !semanticGenerationToolsStillMatch(ctx, g.metadata) {
		return errors.New("semantic " + label + " export: index or tool identity changed during export")
	}
	return nil
}

// ImportSemanticSCIP imports a bounded SCIP stream into an empty index store.
// Requiring an empty store prevents a single-scope import from silently
// replacing other languages or project scopes in a committed generation.
func (s *Server) ImportSemanticSCIP(ctx context.Context, language, scopeID string, data []byte) (SemanticSCIPImportResult, error) {
	generation, coverage, err := s.importSemanticScope(ctx, language, scopeID, data, "SCIP", interop.ImportSCIPToModel)
	if err != nil {
		return SemanticSCIPImportResult{}, err
	}
	return SemanticSCIPImportResult{Generation: generation, ScopeID: scopeID, Coverage: coverage}, nil
}

// importSemanticScope carries the validation and commit machinery shared by
// every semantic artifact import (label shapes error text: "SCIP" or "LSIF").
// convert parses one bounded artifact stream into the format-neutral model and
// is the only format-specific step.
func (s *Server) importSemanticScope(
	ctx context.Context,
	language, scopeID string,
	data []byte,
	label string,
	convert func(ctx context.Context, data []byte, req model.Request, sink model.Sink) (model.Report, error),
) (uint64, []model.Coverage, error) {
	if ctx == nil || language == "" || scopeID == "" {
		return 0, nil, errors.New("semantic " + label + " import requires a context, language, and scope ID")
	}
	if len(data) == 0 || len(data) > maxSemanticImportBytes {
		return 0, nil, fmt.Errorf("semantic %s import input must be between 1 and %d bytes", label, maxSemanticImportBytes)
	}
	idx, policy, reason := s.indexState()
	if idx == nil {
		return 0, nil, fmt.Errorf("semantic %s import: index unavailable: %s", label, reason)
	}
	if policy == nil || policy.State() == trust.StateUntrusted {
		return 0, nil, errors.New("semantic " + label + " import rejected: workspace is untrusted")
	}
	if !idx.buildMu.TryLock() {
		return 0, nil, errors.New("semantic " + label + " import rejected: another reindex is running")
	}
	defer idx.buildMu.Unlock()
	reindexLease, err := idx.store.AcquireReindexLease(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: acquire reindex lease: %w", label, err)
	}
	defer reindexLease.Close()
	if existing, err := idx.store.OpenSnapshot(ctx); err == nil {
		return 0, nil, fmt.Errorf("semantic %s import refuses to replace existing generation %d; choose an empty index directory", label, existing.ID)
	} else if !errors.Is(err, persistent.ErrNoGeneration) {
		return 0, nil, fmt.Errorf("semantic %s import: inspect index: %w", label, err)
	}

	revision := s.currentRevision()
	view, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: capture workspace: %w", label, err)
	}
	defer view.Close()

	var selected *model.Request
	for _, binding := range s.semanticIndexBindings() {
		if binding.language != language {
			continue
		}
		request, planErr := binding.binding.planner.BuildIndexRequest(ctx, view, view.rootURI)
		if planErr != nil {
			return 0, nil, fmt.Errorf("semantic %s import: plan %s scope: %w", label, language, planErr)
		}
		if request.View == nil || request.View.Identity() != view.Identity() {
			return 0, nil, errors.New("semantic " + label + " import: scope planner did not bind to the captured workspace")
		}
		for _, scope := range request.Scopes {
			if scope.ID != scopeID {
				continue
			}
			provenance, ok := request.Provenance[scope.ID]
			if !ok || selected != nil {
				return 0, nil, fmt.Errorf("semantic %s import: scope %q is missing or ambiguous", label, scopeID)
			}
			selected = &model.Request{
				View: view, Scopes: []model.Scope{scope},
				Provenance: map[string]model.Provenance{scope.ID: provenance},
			}
		}
	}
	if selected == nil {
		return 0, nil, fmt.Errorf("semantic %s import: no registered %s provider owns scope %q", label, language, scopeID)
	}
	build, err := idx.store.BeginBuild(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: begin generation: %w", label, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semantic.NewSink(ctx, build, *selected)
	if err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: create semantic sink: %w", label, err)
	}
	defer sink.Close()
	report, err := convert(ctx, data, *selected, sink)
	if err != nil {
		return 0, nil, err
	}
	if err := sink.Finalize(ctx, report); err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: finalize generation: %w", label, err)
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	currentDigest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil || currentDigest != view.Identity().DiskDigest || s.currentRevision() != revision {
		return 0, nil, errors.New("semantic " + label + " import: workspace changed during import")
	}
	if err := view.Close(); err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: release captured view: %w", label, err)
	}
	if err := build.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("semantic %s import: commit generation: %w", label, err)
	}
	committed = true
	return build.GenerationID(), report.Coverage, nil
}
