package server

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
)

const (
	goOverlayFactsMaxRecords = 200_000
	goOverlayFactsMaxBytes   = int64(32 << 20)
	goOverlayCacheMaxBytes   = int64(64 << 20)
	goOverlayCacheMaxItems   = 2
	goOverlayWorkspaceLimit  = 100
)

var errGoOverlayFactsBudget = errors.New("Go semantic overlay facts exceed the request budget")

type goOverlayFactsKey struct {
	workspace        identity.WorkspaceID
	language         string
	diskDigest       identity.ContentHash
	overlayDigest    identity.ContentHash
	planDigest       identity.ContentHash
	snapshotInstance uint64
	revision         uint64
	builder          uintptr
	provider         uintptr
}

type goOverlayFactsEntry struct {
	key   goOverlayFactsKey
	facts *goSnapshotSemanticFacts
	bytes int64
}

type goOverlayFactsFlight struct {
	done chan struct{}
}

// goSnapshotSemanticFactsCache is owned by one server. It retains at most two
// immutable request fact sets and 64 MiB of estimated index memory. It never
// writes a semantic generation.
type goSnapshotSemanticFactsCache struct {
	mu      sync.Mutex
	entries map[goOverlayFactsKey]*list.Element
	lru     list.List
	flights map[goOverlayFactsKey]*goOverlayFactsFlight
	used    int64
}

func newGoSnapshotSemanticFactsCache() *goSnapshotSemanticFactsCache {
	return &goSnapshotSemanticFactsCache{
		entries: make(map[goOverlayFactsKey]*list.Element),
		flights: make(map[goOverlayFactsKey]*goOverlayFactsFlight),
	}
}

type goSnapshotSemanticFacts struct {
	language         string
	identity         model.Identity
	snapshotInstance uint64
	revision         uint64
	scopes           []model.Scope
	coverage         []model.Coverage
	provenance       map[string]model.Provenance
	symbols          []model.Symbol
	occurrences      []model.Occurrence
	byURI            map[string][]int
	bySymbol         map[goOverlaySymbolKey][]int
	symbolByID       map[goOverlaySymbolKey]model.Symbol
	retainedBytes    int64
}

type goOverlaySymbolKey struct {
	scopeID string
	id      identity.SymbolID
}

type goOverlayWorkspaceSymbol struct {
	Symbol     model.Symbol
	Definition model.Occurrence
}

// GetOrExport returns exact snapshot facts from this cache or performs one
// bounded full-scope export for the requested language. Concurrent requests
// for the same snapshot share a single export. The view must still be open
// when loading a miss.
func (c *goSnapshotSemanticFactsCache) GetOrExport(
	ctx context.Context,
	s *Server,
	view *goSnapshotSemanticView,
	builder languages.SemanticIndexRequestBuilder,
	provider languages.SemanticIndexProvider,
	language string,
) (*goSnapshotSemanticFacts, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || s == nil || view == nil || builder == nil || provider == nil || language == "" {
		return nil, errGoSemanticOverlayUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !view.StillCurrent(s, ctx) {
		return nil, errGoSemanticOverlayStale
	}
	request, err := builder.BuildIndexRequest(ctx, view, view.base.rootURI)
	if err != nil {
		return nil, err
	}
	if err := validateGoSnapshotOverlayPlan(ctx, view, request, language); err != nil {
		return nil, err
	}
	planDigest, ok := goOverlayPlanDigest(request)
	if !ok {
		return nil, errGoSemanticOverlayUnavailable
	}
	key, ok := goOverlayFactsCacheKey(view, builder, provider, planDigest, language)
	if !ok {
		return nil, errGoSemanticOverlayUnavailable
	}
	load := func() (*goSnapshotSemanticFacts, error) {
		sink := &boundedGoOverlayFactsSink{}
		request, report, err := exportGoSnapshotOverlayWithRequest(ctx, s, view, request, provider, sink, language)
		if err != nil {
			return nil, err
		}
		facts, err := buildGoSnapshotSemanticFactsContext(ctx, view, request, report, sink, language)
		if err != nil {
			return nil, err
		}
		if err := validateGoSnapshotFactSources(ctx, view, facts); err != nil {
			return nil, err
		}
		if !view.StillCurrent(s, ctx) || !goSnapshotOverlayPlanToolsStillMatch(ctx, request) {
			return nil, errGoSemanticOverlayStale
		}
		return facts, nil
	}
	return c.getOrLoad(ctx, key, load, func() bool {
		return view.StillCurrent(s, ctx) && goSnapshotOverlayPlanToolsStillMatch(ctx, request)
	})
}

func goOverlayFactsCacheKey(view *goSnapshotSemanticView, builder, provider any, planDigest identity.ContentHash, language string) (goOverlayFactsKey, bool) {
	if view == nil || view.base == nil || view.snapshot == nil || view.base.snapshot == "" ||
		view.identity.Workspace == "" || view.identity.Workspace != view.base.identity.Workspace ||
		language == "" ||
		view.identity.SnapshotRev == 0 || view.base.identity.SnapshotRev != view.identity.SnapshotRev ||
		view.overlay.Revision != view.identity.SnapshotRev || view.overlay.SnapshotInstance == 0 ||
		view.overlay.SnapshotInstance != view.snapshot.InstanceID() || view.overlay.Digest == "" ||
		view.base.identity.DiskDigest == "" || planDigest == "" ||
		view.identity.DiskDigest != semanticOverlayWorkspaceDigest(view.base.identity.DiskDigest, view.overlay.Digest) {
		return goOverlayFactsKey{}, false
	}
	builderID, builderOK := overlayProviderPointer(builder)
	providerID, providerOK := overlayProviderPointer(provider)
	if !builderOK || !providerOK {
		return goOverlayFactsKey{}, false
	}
	return goOverlayFactsKey{
		workspace: view.identity.Workspace, language: language, diskDigest: view.base.identity.DiskDigest,
		overlayDigest: view.overlay.Digest, planDigest: planDigest, snapshotInstance: view.snapshot.InstanceID(),
		revision: view.identity.SnapshotRev, builder: builderID, provider: providerID,
	}, true
}

func goOverlayPlanDigest(request model.Request) (identity.ContentHash, bool) {
	if request.View == nil || len(request.Scopes) == 0 || len(request.Provenance) != len(request.Scopes) {
		return "", false
	}
	scopes := append([]model.Scope(nil), request.Scopes...)
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].ID < scopes[j].ID })
	provenance := make(map[string]model.Provenance, len(request.Provenance))
	for scopeID, item := range request.Provenance {
		item.Tools = append([]model.ToolIdentity(nil), item.Tools...)
		sort.Slice(item.Tools, func(i, j int) bool {
			if item.Tools[i].Name != item.Tools[j].Name {
				return item.Tools[i].Name < item.Tools[j].Name
			}
			if item.Tools[i].Path != item.Tools[j].Path {
				return item.Tools[i].Path < item.Tools[j].Path
			}
			if item.Tools[i].Version != item.Tools[j].Version {
				return item.Tools[i].Version < item.Tools[j].Version
			}
			return item.Tools[i].SHA256 < item.Tools[j].SHA256
		})
		provenance[scopeID] = item
	}
	payload, err := json.Marshal(struct {
		WorkspaceRootURI string
		Identity         model.Identity
		Scopes           []model.Scope
		Provenance       map[string]model.Provenance
	}{request.WorkspaceRootURI, request.View.Identity(), scopes, provenance})
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(payload)
	return identity.ContentHash("sha256:" + hex.EncodeToString(digest[:])), true
}

func goSnapshotOverlayPlanToolsStillMatch(ctx context.Context, request model.Request) bool {
	if ctx == nil {
		return false
	}
	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return false
		}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || len(provenance.Tools) == 0 {
			return false
		}
		for _, tool := range provenance.Tools {
			if !toolIdentityStillMatches(ctx, tool) {
				return false
			}
		}
	}
	return ctx.Err() == nil
}

func overlayProviderPointer(value any) (uintptr, bool) {
	if value == nil {
		return 0, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return 0, false
	}
	return rv.Pointer(), true
}

func (c *goSnapshotSemanticFactsCache) getOrLoad(
	ctx context.Context,
	key goOverlayFactsKey,
	load func() (*goSnapshotSemanticFacts, error),
	stillCurrent func() bool,
) (*goSnapshotSemanticFacts, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if element, ok := c.entries[key]; ok {
			c.lru.MoveToFront(element)
			facts := element.Value.(*goOverlayFactsEntry).facts
			c.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !stillCurrent() {
				return nil, errGoSemanticOverlayStale
			}
			return facts, nil
		}
		if flight, ok := c.flights[key]; ok {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
				continue
			}
		}
		flight := &goOverlayFactsFlight{done: make(chan struct{})}
		c.flights[key] = flight
		c.mu.Unlock()

		facts, err := load()
		c.mu.Lock()
		delete(c.flights, key)
		if err == nil && facts != nil && ctx.Err() == nil && stillCurrent() && facts.retainedBytes <= goOverlayCacheMaxBytes {
			c.insertLocked(key, facts)
		}
		close(flight.done)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && !stillCurrent() {
			return nil, errGoSemanticOverlayStale
		}
		return facts, err
	}
}

func (c *goSnapshotSemanticFactsCache) insertLocked(key goOverlayFactsKey, facts *goSnapshotSemanticFacts) {
	if old, ok := c.entries[key]; ok {
		entry := old.Value.(*goOverlayFactsEntry)
		c.used -= entry.bytes
		c.lru.Remove(old)
		delete(c.entries, key)
	}
	for c.lru.Len() >= goOverlayCacheMaxItems || c.used > goOverlayCacheMaxBytes-facts.retainedBytes {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*goOverlayFactsEntry)
		delete(c.entries, entry.key)
		c.used -= entry.bytes
		c.lru.Remove(oldest)
	}
	entry := &goOverlayFactsEntry{key: key, facts: facts, bytes: facts.retainedBytes}
	c.entries[key] = c.lru.PushFront(entry)
	c.used += entry.bytes
}

type boundedGoOverlayFactsSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
	bytes       int64
	records     int
}

func (s *boundedGoOverlayFactsSink) add(size int64) error {
	if size < 0 || s.records >= goOverlayFactsMaxRecords || size > goOverlayFactsMaxBytes-s.bytes {
		return errGoOverlayFactsBudget
	}
	s.bytes += size
	s.records++
	return nil
}

func (s *boundedGoOverlayFactsSink) WriteSymbols(ctx context.Context, values []model.Symbol) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.add(160 + int64(len(value.ID)+len(value.ScopeID)+len(value.Name)+len(value.Kind)+len(value.Signature))); err != nil {
			return err
		}
		s.symbols = append(s.symbols, value)
	}
	return nil
}

func (s *boundedGoOverlayFactsSink) WriteOccurrences(ctx context.Context, values []model.Occurrence) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.add(192 + int64(len(value.SymbolID)+len(value.ScopeID)+len(value.URI)+len(value.Role)+len(value.SourceHash)+len(value.BuildContext))); err != nil {
			return err
		}
		s.occurrences = append(s.occurrences, value)
	}
	return nil
}

func (s *boundedGoOverlayFactsSink) WriteEdges(ctx context.Context, values []model.Edge) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.add(224 + int64(len(value.From)+len(value.To)+len(value.ScopeID)+len(value.SourceURI)+len(value.Kind)+len(value.SourceHash)+len(value.BuildContext))); err != nil {
			return err
		}
		s.edges = append(s.edges, value)
	}
	return nil
}

func buildGoSnapshotSemanticFacts(
	view *goSnapshotSemanticView,
	request model.Request,
	report model.Report,
	sink *boundedGoOverlayFactsSink,
	language string,
) (*goSnapshotSemanticFacts, error) {
	return buildGoSnapshotSemanticFactsContext(context.Background(), view, request, report, sink, language)
}

func buildGoSnapshotSemanticFactsContext(
	ctx context.Context,
	view *goSnapshotSemanticView,
	request model.Request,
	report model.Report,
	sink *boundedGoOverlayFactsSink,
	language string,
) (*goSnapshotSemanticFacts, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if view == nil || sink == nil || language == "" || request.View == nil || request.View.Identity() != view.Identity() || len(request.Scopes) == 0 {
		return nil, fmt.Errorf("%w: %s export returned no stable request facts", errGoSemanticOverlayUnavailable, language)
	}
	scopes := make(map[string]model.Scope, len(request.Scopes))
	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if scope.Language != language || scope.ID == "" || scope.BuildContext == "" {
			return nil, fmt.Errorf("%w: invalid %s scope identity", errGoSemanticOverlayUnavailable, language)
		}
		if _, duplicate := scopes[scope.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate %s scope identity", errGoSemanticOverlayIncomplete, language)
		}
		scopes[scope.ID] = scope
	}
	facts := &goSnapshotSemanticFacts{
		language: language,
		identity: view.Identity(), snapshotInstance: view.SnapshotInstanceID(), revision: view.overlay.Revision,
		scopes: append([]model.Scope(nil), request.Scopes...), coverage: append([]model.Coverage(nil), report.Coverage...),
		provenance: make(map[string]model.Provenance, len(request.Provenance)),
		symbols:    append([]model.Symbol(nil), sink.symbols...), occurrences: append([]model.Occurrence(nil), sink.occurrences...),
		byURI: make(map[string][]int), bySymbol: make(map[goOverlaySymbolKey][]int),
		symbolByID: make(map[goOverlaySymbolKey]model.Symbol, len(sink.symbols)),
	}
	for scopeID, provenance := range request.Provenance {
		facts.provenance[scopeID] = provenance
	}
	for _, symbol := range facts.symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if symbol.ID == "" || symbol.Name == "" {
			return nil, fmt.Errorf("%w: symbol omitted stable ID or name", errGoSemanticOverlayIncomplete)
		}
		if _, ok := scopes[symbol.ScopeID]; !ok {
			return nil, fmt.Errorf("%w: symbol references unknown %s scope", errGoSemanticOverlayIncomplete, language)
		}
		key := goOverlaySymbolKey{scopeID: symbol.ScopeID, id: symbol.ID}
		if prior, duplicate := facts.symbolByID[key]; duplicate && (prior.Name != symbol.Name || prior.Kind != symbol.Kind || prior.Signature != symbol.Signature) {
			return nil, fmt.Errorf("%w: stable symbol ID maps to conflicting symbols", errGoSemanticOverlayIncomplete)
		}
		facts.symbolByID[key] = symbol
	}
	for index, occurrence := range facts.occurrences {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		scope, ok := scopes[occurrence.ScopeID]
		key := goOverlaySymbolKey{scopeID: occurrence.ScopeID, id: occurrence.SymbolID}
		if !ok || facts.symbolByID[key].ID == "" || occurrence.URI == "" ||
			occurrence.SourceHash == "" || occurrence.BuildContext != scope.BuildContext ||
			(occurrence.Role != "definition" && occurrence.Role != "declaration" && occurrence.Role != "reference") {
			return nil, fmt.Errorf("%w: occurrence has unknown identity, source, or role", errGoSemanticOverlayIncomplete)
		}
		facts.byURI[occurrence.URI] = append(facts.byURI[occurrence.URI], index)
		facts.bySymbol[key] = append(facts.bySymbol[key], index)
	}
	for _, indexes := range facts.byURI {
		sort.Slice(indexes, func(i, j int) bool { return uriOccurrenceIndexLess(facts.occurrences, indexes[i], indexes[j]) })
	}
	for _, indexes := range facts.bySymbol {
		sort.Slice(indexes, func(i, j int) bool { return occurrenceIndexLess(facts.occurrences, indexes[i], indexes[j]) })
	}
	facts.retainedBytes = sink.bytes + int64(len(facts.symbols))*96 + int64(len(facts.occurrences))*128
	if facts.retainedBytes > goOverlayFactsMaxBytes {
		return nil, errGoOverlayFactsBudget
	}
	return facts, nil
}

func validateGoSnapshotFactSources(ctx context.Context, view *goSnapshotSemanticView, facts *goSnapshotSemanticFacts) error {
	if view == nil || facts == nil || facts.identity != view.Identity() ||
		facts.snapshotInstance != view.SnapshotInstanceID() || facts.revision != view.identity.SnapshotRev {
		return errGoSemanticOverlayStale
	}
	uris := make([]string, 0, len(facts.byURI))
	for fileURI := range facts.byURI {
		uris = append(uris, fileURI)
	}
	sort.Strings(uris)
	for _, fileURI := range uris {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, ok := view.file(fileURI)
		if !ok || file.URI != fileURI || file.LanguageID != facts.language || file.Size < 0 ||
			file.Size > semanticViewMaxFileBytes || file.SHA256 == "" {
			return fmt.Errorf("%w: occurrence source is absent from the %s snapshot view", errGoSemanticOverlayIncomplete, facts.language)
		}
		for _, index := range facts.byURI[fileURI] {
			if facts.occurrences[index].SourceHash != file.SHA256 {
				return fmt.Errorf("%w: occurrence source hash differs from the %s snapshot view", errGoSemanticOverlayIncomplete, facts.language)
			}
		}
		content, err := readGoSnapshotViewFile(ctx, view, file)
		if err != nil {
			return err
		}
		for _, index := range facts.byURI[fileURI] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, _, ok := persistedOccurrenceOffsets(content, facts.occurrences[index].Range); !ok {
				return fmt.Errorf("%w: occurrence range is invalid for its exact source", errGoSemanticOverlayIncomplete)
			}
		}
	}
	return nil
}

func readGoSnapshotViewFile(ctx context.Context, view *goSnapshotSemanticView, file model.File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if file.Size < 0 || file.Size > semanticViewMaxFileBytes {
		return nil, errGoOverlayFactsBudget
	}
	reader, err := view.Read(ctx, file.URI)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: reader}, file.Size+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil {
		return nil, errors.Join(readErr, closeErr, ctx.Err())
	}
	if int64(len(content)) != file.Size || semanticOverlayContentHash(content) != file.SHA256 {
		return nil, fmt.Errorf("%w: source %q changed in the request view", errGoSemanticOverlayStale, file.URI)
	}
	return content, nil
}

func (v *goSnapshotSemanticView) file(fileURI string) (model.File, bool) {
	if v == nil || v.base == nil {
		return model.File{}, false
	}
	if overlay, ok := v.overlayFiles[fileURI]; ok {
		return overlay.file, true
	}
	file, ok := v.base.byURI[fileURI]
	return file, ok
}

// symbolAt stops scanning after positions past the query. URI buckets must
// therefore be ordered by position, independently of symbol identity.
func uriOccurrenceIndexLess(occurrences []model.Occurrence, leftIndex, rightIndex int) bool {
	left, right := occurrences[leftIndex], occurrences[rightIndex]
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartChar != right.Range.StartChar {
		return left.Range.StartChar < right.Range.StartChar
	}
	if left.Range.EndLine != right.Range.EndLine {
		return left.Range.EndLine < right.Range.EndLine
	}
	if left.Range.EndChar != right.Range.EndChar {
		return left.Range.EndChar < right.Range.EndChar
	}
	if left.ScopeID != right.ScopeID {
		return left.ScopeID < right.ScopeID
	}
	if left.SymbolID != right.SymbolID {
		return left.SymbolID < right.SymbolID
	}
	return left.Role < right.Role
}

func occurrenceIndexLess(occurrences []model.Occurrence, leftIndex, rightIndex int) bool {
	left, right := occurrences[leftIndex], occurrences[rightIndex]
	if left.SymbolID != right.SymbolID {
		return left.SymbolID < right.SymbolID
	}
	if left.URI != right.URI {
		return left.URI < right.URI
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartChar != right.Range.StartChar {
		return left.Range.StartChar < right.Range.StartChar
	}
	if left.Range.EndLine != right.Range.EndLine {
		return left.Range.EndLine < right.Range.EndLine
	}
	if left.Range.EndChar != right.Range.EndChar {
		return left.Range.EndChar < right.Range.EndChar
	}
	if left.Role != right.Role {
		return left.Role < right.Role
	}
	return left.ScopeID < right.ScopeID
}

func (f *goSnapshotSemanticFacts) DefinitionAt(fileURI string, line, character uint32) ([]model.Occurrence, bool) {
	key, ok := f.symbolKeyAt(fileURI, line, character)
	if !ok {
		return nil, false
	}
	var result []model.Occurrence
	for _, index := range f.bySymbol[key] {
		occurrence := f.occurrences[index]
		if occurrence.Role == "definition" {
			result = append(result, occurrence)
		}
	}
	return result, len(result) != 0
}

func (f *goSnapshotSemanticFacts) ReferencesAt(fileURI string, line, character uint32, includeDeclaration bool) ([]model.Occurrence, bool) {
	key, ok := f.symbolKeyAt(fileURI, line, character)
	if !ok {
		return nil, false
	}
	var result []model.Occurrence
	for _, index := range f.bySymbol[key] {
		occurrence := f.occurrences[index]
		if occurrence.Role == "reference" || includeDeclaration && (occurrence.Role == "definition" || occurrence.Role == "declaration") {
			result = append(result, occurrence)
		}
	}
	return result, len(result) != 0
}

func (f *goSnapshotSemanticFacts) WorkspaceSymbols(query string, limit int) []goOverlayWorkspaceSymbol {
	if f == nil {
		return nil
	}
	if limit <= 0 || limit > goOverlayWorkspaceLimit {
		limit = goOverlayWorkspaceLimit
	}
	needle := strings.ToLower(query)
	symbols := append([]model.Symbol(nil), f.symbols...)
	sort.Slice(symbols, func(i, j int) bool {
		left, right := strings.ToLower(symbols[i].Name), strings.ToLower(symbols[j].Name)
		if left != right {
			return left < right
		}
		if symbols[i].ScopeID != symbols[j].ScopeID {
			return symbols[i].ScopeID < symbols[j].ScopeID
		}
		return symbols[i].ID < symbols[j].ID
	})
	result := make([]goOverlayWorkspaceSymbol, 0, limit)
	for _, symbol := range symbols {
		if !strings.Contains(strings.ToLower(symbol.Name), needle) {
			continue
		}
		for _, index := range f.bySymbol[goOverlaySymbolKey{scopeID: symbol.ScopeID, id: symbol.ID}] {
			occurrence := f.occurrences[index]
			if occurrence.ScopeID == symbol.ScopeID && occurrence.Role == "definition" {
				result = append(result, goOverlayWorkspaceSymbol{Symbol: symbol, Definition: occurrence})
				break
			}
		}
		if len(result) == limit {
			break
		}
	}
	return result
}

func (f *goSnapshotSemanticFacts) symbolAt(fileURI string, line, character uint32) (identity.SymbolID, bool) {
	key, ok := f.symbolKeyAt(fileURI, line, character)
	return key.id, ok
}

func (f *goSnapshotSemanticFacts) symbolKeyAt(fileURI string, line, character uint32) (goOverlaySymbolKey, bool) {
	if f == nil {
		return goOverlaySymbolKey{}, false
	}
	indexes := f.byURI[fileURI]
	best := -1
	for _, index := range indexes {
		occurrence := f.occurrences[index]
		if occurrence.Range.StartLine > line || occurrence.Range.StartLine == line && occurrence.Range.StartChar > character {
			break
		}
		if !occurrenceContains(occurrence.Range, line, character) {
			continue
		}
		if best < 0 || occurrenceRangeNarrower(occurrence.Range, f.occurrences[best].Range) {
			best = index
			continue
		}
		if !occurrenceRangeNarrower(f.occurrences[best].Range, occurrence.Range) &&
			(goOverlaySymbolKey{scopeID: occurrence.ScopeID, id: occurrence.SymbolID} != goOverlaySymbolKey{scopeID: f.occurrences[best].ScopeID, id: f.occurrences[best].SymbolID}) {
			return goOverlaySymbolKey{}, false
		}
	}
	if best < 0 {
		return goOverlaySymbolKey{}, false
	}
	return goOverlaySymbolKey{scopeID: f.occurrences[best].ScopeID, id: f.occurrences[best].SymbolID}, true
}

func occurrenceContains(r model.Position, line, character uint32) bool {
	if line < r.StartLine || line > r.EndLine {
		return false
	}
	if line == r.StartLine && character < r.StartChar {
		return false
	}
	if line == r.EndLine && character >= r.EndChar {
		return false
	}
	return true
}

func occurrenceRangeNarrower(left, right model.Position) bool {
	leftLines := left.EndLine - left.StartLine
	rightLines := right.EndLine - right.StartLine
	if leftLines != rightLines {
		return leftLines < rightLines
	}
	if leftLines == 0 {
		return left.EndChar-left.StartChar < right.EndChar-right.StartChar
	}
	if left.StartLine != right.StartLine {
		return left.StartLine > right.StartLine
	}
	return left.StartChar > right.StartChar
}

var _ model.Sink = (*boundedGoOverlayFactsSink)(nil)
