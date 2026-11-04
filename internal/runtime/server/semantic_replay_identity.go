package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sort"
	"sync"

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

// semanticIndexIdentityMemo caches the last verified SemanticIndexIdentity per
// (index service, committed generation). Round-23 OPT-2: the semantic response
// observer called CurrentSemanticIndexIdentity on EVERY evidence-indexed
// response, and each call re-derived the whole identity — full-tree
// captureSemanticView (~0.65-1.6s on a 7MB workspace), every pinned tool
// re-hashed, every store segment re-read and re-hashed — although one
// generation's identity is invariant for that generation's lifetime. The memo
// is bound to the index-service pointer, so a workspace rebind (new idx) never
// serves a stale identity; a new generation has a new view.ID and misses.
// Errors are never cached: a transient lease failure must stay retryable and
// must not poison later responses.
type semanticIndexIdentityMemo struct {
	mu    sync.Mutex
	owner *indexService
	gen   uint64
	value SemanticIndexIdentity
}

func newSemanticIndexIdentityMemo() *semanticIndexIdentityMemo {
	return &semanticIndexIdentityMemo{}
}

func (s *Server) cachedSemanticIndexIdentity(idx *indexService, generation uint64) (SemanticIndexIdentity, bool) {
	memo := s.semanticIdentityMemo
	if memo == nil {
		return SemanticIndexIdentity{}, false
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.owner == idx && memo.gen == generation {
		return memo.value, true
	}
	return SemanticIndexIdentity{}, false
}

func (s *Server) storeSemanticIndexIdentity(idx *indexService, generation uint64, value SemanticIndexIdentity) {
	memo := s.semanticIdentityMemo
	if memo == nil {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.owner, memo.gen, memo.value = idx, generation, value
}

// CurrentSemanticIndexIdentity returns the exact currently available disk
// generation. A stale workspace, changed tool, or incomplete provenance has
// no replayable identity. Within one committed generation the identity is
// memoized (round-23 OPT-2): the generation pin still opens the snapshot
// lease every call, so a reindex serves a fresh identity and the per-response
// observer hits the memo in O(1).
func (s *Server) CurrentSemanticIndexIdentity(ctx context.Context) (SemanticIndexIdentity, error) {
	idx, _, _ := s.indexState()
	if idx == nil {
		return SemanticIndexIdentity{}, errors.New("semantic replay: no index service")
	}
	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return SemanticIndexIdentity{}, err
	}
	view := lease.Snapshot()
	if cached, ok := s.cachedSemanticIndexIdentity(idx, view.ID); ok {
		_ = lease.Close()
		return cached, nil
	}
	defer lease.Close()
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
	identity := SemanticIndexIdentity{Generation: view.ID, IndexContentDigest: digest, BuildContexts: contexts, Tools: tools}
	s.storeSemanticIndexIdentity(idx, view.ID, identity)
	return identity, nil
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
