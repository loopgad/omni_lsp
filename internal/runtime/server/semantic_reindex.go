package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
)

type semanticProviderJob struct {
	language string
	binding  semanticIndexBinding
	request  model.Request
}

func (s *Server) reindexSemantic(ctx context.Context, idx *indexService, revision uint64, stats *IndexRebuildStats) (json.RawMessage, error) {
	view, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = view.Close() }()

	bindings := s.semanticIndexBindings()
	jobs := make([]semanticProviderJob, 0, len(bindings))
	allScopes := make([]model.Scope, 0, len(bindings))
	allProvenance := make(map[string]model.Provenance)
	seenScopes := make(map[string]struct{})
	for _, binding := range bindings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request, err := binding.binding.planner.BuildIndexRequest(ctx, view, view.rootURI)
		if err != nil {
			return nil, fmt.Errorf("plan %s semantic index: %w", binding.language, err)
		}
		if request.View == nil || request.View.Identity() != view.Identity() {
			return nil, fmt.Errorf("plan %s semantic index: request is not bound to captured workspace view", binding.language)
		}
		request.View = view
		if request.WorkspaceRootURI != "" && request.WorkspaceRootURI != view.rootURI {
			return nil, fmt.Errorf("plan %s semantic index: workspace root differs from captured view", binding.language)
		}
		request.WorkspaceRootURI = view.rootURI
		for _, scope := range request.Scopes {
			if _, exists := seenScopes[scope.ID]; exists {
				return nil, fmt.Errorf("plan semantic index: duplicate scope ID %q", scope.ID)
			}
			provenance, ok := request.Provenance[scope.ID]
			if !ok || provenance.Scope.ID != scope.ID {
				return nil, fmt.Errorf("plan %s semantic index: missing provenance for scope %q", binding.language, scope.ID)
			}
			seenScopes[scope.ID] = struct{}{}
			allScopes = append(allScopes, scope)
			allProvenance[scope.ID] = provenance
		}
		jobs = append(jobs, semanticProviderJob{language: binding.language, binding: binding.binding, request: request})
	}
	if len(allScopes) == 0 {
		return nil, errors.New("semantic index: registered providers discovered no project scopes")
	}
	sort.Slice(allScopes, func(i, j int) bool {
		if allScopes[i].Language != allScopes[j].Language {
			return allScopes[i].Language < allScopes[j].Language
		}
		if allScopes[i].RootURI != allScopes[j].RootURI {
			return allScopes[i].RootURI < allScopes[j].RootURI
		}
		return allScopes[i].ID < allScopes[j].ID
	})
	request := model.Request{WorkspaceRootURI: view.rootURI, View: view, Scopes: allScopes, Provenance: allProvenance}
	build, err := idx.store.BeginBuild(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semantic.NewSink(ctx, build, request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sink.Close() }()
	aggregate := model.Report{Identity: view.Identity(), UsedTools: make(map[string][]model.ToolIdentity)}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report, exportErr := job.binding.provider.ExportIndex(ctx, job.request, sink)
		if exportErr != nil {
			return nil, fmt.Errorf("export %s semantic index: %w", job.language, exportErr)
		}
		if err := model.ValidateReport(job.request, report); err != nil {
			return nil, fmt.Errorf("validate %s semantic report: %w", job.language, err)
		}
		aggregate.Coverage = append(aggregate.Coverage, report.Coverage...)
		for scopeID, tools := range report.UsedTools {
			aggregate.UsedTools[scopeID] = append([]model.ToolIdentity(nil), tools...)
		}
		aggregate.Errors = append(aggregate.Errors, report.Errors...)
	}
	if err := model.ValidateReport(request, aggregate); err != nil {
		return nil, fmt.Errorf("validate merged semantic report: %w", err)
	}
	if err := sink.Finalize(ctx, aggregate); err != nil {
		return nil, fmt.Errorf("finalize semantic generation: %w", err)
	}
	currentDigest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil {
		return nil, err
	}
	if currentDigest != view.Identity().DiskDigest {
		return nil, fmt.Errorf("workspace files changed during semantic reindex")
	}
	if currentRevision := s.currentRevision(); currentRevision != revision {
		return nil, fmt.Errorf("workspace changed during semantic reindex (snapshot %d, current %d)", revision, currentRevision)
	}
	if err := view.Close(); err != nil {
		return nil, fmt.Errorf("close immutable semantic view: %w", err)
	}
	if err := build.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	stats.Files = len(view.files)
	for _, file := range view.files {
		stats.Bytes += file.Size
	}
	return json.Marshal(map[string]any{
		"generation": build.GenerationID(), "files": stats.Files, "bytes": stats.Bytes,
		"skipped": stats.Skipped, "revision": revision, "semanticScopes": len(allScopes),
	})
}
