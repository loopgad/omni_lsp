package server

import (
	"context"
	"sort"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

func (s *Server) beginBackendWorkspaceSnapshot(ctx context.Context, be languages.Backend, captured *snapshot.Snapshot) (context.Context, func() error, error) {
	if err := s.flushExternalSourceChanges(ctx, be); err != nil {
		return ctx, nil, err
	}
	synchronizer, ok := be.(languages.WorkspaceSnapshotSynchronizer)
	if !ok {
		return ctx, nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if captured == nil && s.snapMgr != nil {
		captured = s.snapMgr.Current()
	}
	if captured == nil {
		return ctx, nil, nil
	}

	uris := captured.Documents()
	sort.Strings(uris)
	documents := make([]languages.WorkspaceDocument, 0, len(uris))
	for _, documentURI := range uris {
		doc := captured.Document(documentURI)
		if doc == nil {
			continue
		}
		documents = append(documents, languages.WorkspaceDocument{
			URI: documentURI, LanguageID: doc.LanguageID, Version: doc.Version, Content: doc.Content,
		})
	}
	return synchronizer.BeginWorkspaceSnapshot(ctx, languages.WorkspaceSnapshot{
		Revision: captured.ID().Revision, Documents: documents,
	})
}

func backendWorkspaceGeneration(be languages.Backend) uint64 {
	if scoped, ok := be.(languages.WorkspaceSnapshotSynchronizer); ok {
		return scoped.WorkspaceSnapshotGeneration()
	}
	return 0
}
