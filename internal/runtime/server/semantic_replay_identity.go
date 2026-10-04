package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sort"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
)

// SemanticIndexIdentity is a verified, immutable generation identity for
// session recording. It is intentionally separate from the product protocol.
type SemanticIndexIdentity struct {
	Generation         uint64
	IndexContentDigest string
	BuildContexts      map[string]string
	Tools              []model.ToolIdentity
}

// CurrentSemanticIndexIdentity returns the exact currently available disk
// generation. A stale workspace, changed tool, or incomplete provenance has
// no replayable identity.
func (s *Server) CurrentSemanticIndexIdentity(ctx context.Context) (SemanticIndexIdentity, error) {
	idx, _, _ := s.indexState()
	if idx == nil {
		return SemanticIndexIdentity{}, errors.New("semantic replay: no index service")
	}
	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return SemanticIndexIdentity{}, err
	}
	defer lease.Close()
	view := lease.Snapshot()
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return SemanticIndexIdentity{}, err
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != idx.workspaceID || metadata.Identity.DiskDigest == "" ||
		metadata.Identity.SnapshotRev != 0 || metadata.DiskDigest != metadata.Identity.DiskDigest ||
		!semanticGenerationToolsStillMatch(ctx, metadata) {
		return SemanticIndexIdentity{}, errors.New("semantic replay: generation identity is stale")
	}
	diskView, err := captureSemanticView(ctx, idx.root, idx.workspaceID, s.currentRevision(), idx.dir)
	if err != nil {
		return SemanticIndexIdentity{}, err
	}
	defer diskView.Close()
	if diskView.Identity().DiskDigest != metadata.DiskDigest ||
		!semanticPlanningStillMatches(ctx, s.semanticIndexBindings(), diskView, metadata) {
		return SemanticIndexIdentity{}, errors.New("semantic replay: generation does not match current build inputs")
	}
	digest, err := semanticGenerationContentDigest(ctx, view)
	if err != nil {
		return SemanticIndexIdentity{}, err
	}
	contexts := make(map[string]string, len(metadata.Scopes))
	toolsByKey := make(map[string]model.ToolIdentity)
	for _, scope := range metadata.Scopes {
		if scope.ID == "" || scope.BuildContext == "" {
			return SemanticIndexIdentity{}, errors.New("semantic replay: scope has no build identity")
		}
		contexts[scope.ID] = string(scope.BuildContext)
		for _, tool := range metadata.UsedTools[scope.ID] {
			if tool.Name == "" || tool.Path == "" || tool.Version == "" || tool.SHA256 == "" {
				return SemanticIndexIdentity{}, errors.New("semantic replay: incomplete tool identity")
			}
			toolsByKey[tool.Name+"\x00"+tool.Path+"\x00"+tool.Version+"\x00"+tool.SHA256] = tool
		}
	}
	if len(contexts) == 0 || len(toolsByKey) == 0 {
		return SemanticIndexIdentity{}, errors.New("semantic replay: generation lacks scopes or tool identity")
	}
	keys := make([]string, 0, len(toolsByKey))
	for key := range toolsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tools := make([]model.ToolIdentity, 0, len(keys))
	for _, key := range keys {
		tools = append(tools, toolsByKey[key])
	}
	return SemanticIndexIdentity{Generation: view.ID, IndexContentDigest: digest, BuildContexts: contexts, Tools: tools}, nil
}

func semanticGenerationContentDigest(ctx context.Context, view persistent.GenerationView) (string, error) {
	h := sha256.New()
	for _, ref := range view.Segments {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		payload, err := view.ReadSegment(ref.ID)
		if err != nil {
			return "", err
		}
		writeDigestPart(h, []byte(ref.ID))
		writeDigestPart(h, payload)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeDigestPart(h hash.Hash, part []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(part)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(part)
}
