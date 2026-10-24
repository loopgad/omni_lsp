package ccls

// Hot-path memo tests (see memo.go): admission (confirmed + quiet), value
// stability, invalidation (source changes, other-document snapshot changes,
// epoch/generation guards), capacity, and the nested-trace stand-down. All
// behavioral tests drive the same fake clangd protocol backend as
// bridge_test.go, so "was the request forwarded?" is observable as the
// presence or absence of a wire frame.

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
)

// memoQuiet opens the memo with an explicit quiet window; the zero value
// would be normalized to the production default.
func memoQuiet(b *Backend, quiet time.Duration) {
	b.memo.mu.Lock()
	b.memo.quietPeriod = quiet
	b.memo.mu.Unlock()
}

// memoSettle waits out coarse Windows clock granularity so an entry aged
// past a nanosecond-scale quiet window reads as old.
func memoSettle() { time.Sleep(5 * time.Millisecond) }

// expectNoCclsFrame asserts the bridge issued no child traffic for a
// memo-served request.
func expectNoCclsFrame(t *testing.T, frames <-chan string) {
	t.Helper()
	select {
	case frame := <-frames:
		t.Fatalf("memo-served request still forwarded a child frame: %.120q", frame)
	case <-time.After(150 * time.Millisecond):
	}
}

// drainCclsFrames consumes pending notification frames produced outside a
// request/reply exchange (snapshot reconciliation traffic).
func drainCclsFrames(t *testing.T, frames <-chan string) {
	t.Helper()
	for {
		select {
		case <-frames:
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

func memoCompletionItems(label string) []map[string]any {
	return []map[string]any{{
		"label": label, "kind": 3, "detail": "int",
		"sortText": "01", "filterText": label,
	}}
}

// memoTieCompletionReply builds a five-candidate completion reply in the
// diagnosed jitter shape: two same-scored pairs (a std::stoi overload pair
// and a delete/delete[] keyword pair) whose relative order is the only thing
// that differs between two upstream answers to the same request.
func memoTieCompletionReply(swapped bool) []map[string]any {
	stoiString := map[string]any{
		"label": "std::stoi(const string &Str)", "kind": 3,
		"detail": "int std::stoi(const string &Str)", "sortText": "15", "filterText": "stoi",
	}
	stoiWstring := map[string]any{
		"label": "std::stoi(const wstring &Str)", "kind": 3,
		"detail": "int std::stoi(const wstring &Str)", "sortText": "15", "filterText": "stoi",
	}
	deleteExpr := map[string]any{"label": "delete expression", "kind": 14, "sortText": "20"}
	deleteArray := map[string]any{"label": "delete[] expression", "kind": 14, "sortText": "20"}
	ret := map[string]any{"label": "return", "kind": 6, "sortText": "01"}
	if swapped {
		return []map[string]any{ret, stoiWstring, stoiString, deleteArray, deleteExpr}
	}
	return []map[string]any{ret, stoiString, stoiWstring, deleteExpr, deleteArray}
}

// memoDetailCompletionReply answers with one candidate whose label and
// sortText never change while the detail does — the difference shape a
// field-blind sort key would swallow as equivalence.
func memoDetailCompletionReply(detail string) []map[string]any {
	return []map[string]any{{
		"label": "target", "kind": 3, "detail": detail,
		"sortText": "01", "filterText": "target",
	}}
}

func memoSymbolReply(name string) []map[string]any {
	return []map[string]any{{
		"name": name, "kind": 13,
		"range": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 4},
			"end":   map[string]uint32{"line": 0, "character": 10},
		},
		"selectionRange": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 4},
			"end":   map[string]uint32{"line": 0, "character": 10},
		},
	}}
}

// forwardedCompletion drives one CompletionList call that is expected to
// reach the child; reply drives the fake exchange from the test goroutine
// while the request blocks.
func forwardedCompletion(t *testing.T, b *Backend, stdin *cclsProtocolWriter, child interface{ Write([]byte) (int, error) }, uri string, content []byte, rev uint64, result any) languages.CompletionList {
	t.Helper()
	got := make(chan struct {
		list languages.CompletionList
		err  error
	}, 1)
	go func() {
		list, err := b.CompletionList(context.Background(), languages.CompletionRequest{
			URI: uri, Content: content, SnapshotRev: rev, Line: 1, Column: 2,
		})
		got <- struct {
			list languages.CompletionList
			err  error
		}{list: list, err: err}
	}()
	replyToCclsRequest(t, stdin.frames, child, "textDocument/completion", result)
	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("CompletionList: %v", response.err)
		}
		return response.list
	case <-time.After(2 * time.Second):
		t.Fatal("forwarded completion request did not complete after its reply")
		return languages.CompletionList{}
	}
}

// servedCompletion drives one CompletionList call that must be answered
// from the memo without child traffic.
func servedCompletion(t *testing.T, b *Backend, uri string, content []byte, rev uint64) languages.CompletionList {
	t.Helper()
	got := make(chan struct {
		list languages.CompletionList
		err  error
	}, 1)
	go func() {
		list, err := b.CompletionList(context.Background(), languages.CompletionRequest{
			URI: uri, Content: content, SnapshotRev: rev, Line: 1, Column: 2,
		})
		got <- struct {
			list languages.CompletionList
			err  error
		}{list: list, err: err}
	}()
	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("CompletionList: %v", response.err)
		}
		return response.list
	case <-time.After(2 * time.Second):
		t.Fatal("completion request did not complete (expected a memo hit)")
		return languages.CompletionList{}
	}
}

// forwardedSymbols / servedSymbols mirror the completion pair for
// documentSymbol.
func forwardedSymbols(t *testing.T, b *Backend, stdin *cclsProtocolWriter, child interface{ Write([]byte) (int, error) }, uri string, content []byte, rev uint64, result any) []languages.DocumentSymbol {
	t.Helper()
	got := make(chan struct {
		syms []languages.DocumentSymbol
		err  error
	}, 1)
	go func() {
		syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: uri, Content: content, SnapshotRev: rev,
		})
		got <- struct {
			syms []languages.DocumentSymbol
			err  error
		}{syms: syms, err: err}
	}()
	replyToCclsRequest(t, stdin.frames, child, "textDocument/documentSymbol", result)
	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("DocumentSymbols: %v", response.err)
		}
		return response.syms
	case <-time.After(2 * time.Second):
		t.Fatal("forwarded documentSymbol request did not complete after its reply")
		return nil
	}
}

func servedSymbols(t *testing.T, b *Backend, uri string, content []byte, rev uint64) []languages.DocumentSymbol {
	t.Helper()
	got := make(chan struct {
		syms []languages.DocumentSymbol
		err  error
	}, 1)
	go func() {
		syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: uri, Content: content, SnapshotRev: rev,
		})
		got <- struct {
			syms []languages.DocumentSymbol
			err  error
		}{syms: syms, err: err}
	}()
	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("DocumentSymbols: %v", response.err)
		}
		return response.syms
	case <-time.After(2 * time.Second):
		t.Fatal("documentSymbol request did not complete (expected a memo hit)")
		return nil
	}
}

// TestMemoServesConfirmedQuietCompletionFromCache pins the win itself: the
// second identical observation admits the entry, and the third identical
// request is answered without any child frame.
func TestMemoServesConfirmedQuietCompletionFromCache(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\nint other;\n")

	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	memoSettle()

	list := servedCompletion(t, b, uri, content, 2)
	if len(list.Items) != 1 || list.Items[0].Label != "target" {
		t.Fatalf("memo-served completion = %+v, want the cached target item", list)
	}
	expectNoCclsFrame(t, stdin.frames)
}

// TestMemoCompletionUnconfirmedOrYoungEntryStillForwards pins the quiet
// admission: an entry that has not been confirmed by an equal
// re-computation, or whose value is younger than the quiet window, must keep
// forwarding — this is what keeps clangd's context-incomplete prewarm lists
// out of the cache.
func TestMemoCompletionUnconfirmedOrYoungEntryStillForwards(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		b, stdin, child := newCclsProtocolBackend(t)
		memoQuiet(b, time.Nanosecond)
		const uri = "file:///x/main.cpp"
		content := []byte("int target(void);\n")

		forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
		// Only one observation so far: the repeat must re-forward instead of
		// trusting a possibly prewarm-era answer.
		forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	})

	t.Run("young", func(t *testing.T) {
		b, stdin, child := newCclsProtocolBackend(t)
		memoQuiet(b, time.Hour)
		const uri = "file:///x/main.cpp"
		content := []byte("int target(void);\n")

		for i := 0; i < 2; i++ {
			forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
		}
		memoSettle()
		// Confirmed but still younger than the quiet window: forwards.
		forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	})
}

// TestMemoCompletionValueChangedResetsConfirmation pins that a changed
// upstream answer restarts admission and that the latest value is the one
// served.
func TestMemoCompletionValueChangedResetsConfirmation(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")

	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("early"))
	// The second observation differs (index/preamble settled): admission
	// must restart around the new value, never around "early".
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("settled"))
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("settled"))
	memoSettle()

	list := servedCompletion(t, b, uri, content, 2)
	if len(list.Items) != 1 || list.Items[0].Label != "settled" {
		t.Fatalf("memo-served completion = %+v, want the latest settled item", list)
	}
	expectNoCclsFrame(t, stdin.frames)
}

// TestMemoCompletionConfirmsAcrossTieReorder pins the fix itself: clangd
// breaks ties between same-scored candidates nondeterministically, so two
// forwards of a byte-identical request can answer with both same-scored
// pairs in the opposite relative order. That is the same candidate multiset,
// not a value change, so the second observation must confirm the entry and
// the memo must serve afterwards — with the served value byte-identical to
// the latest raw upstream answer.
func TestMemoCompletionConfirmsAcrossTieReorder(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")

	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoTieCompletionReply(false))
	latest := forwardedCompletion(t, b, stdin, child, uri, content, 2, memoTieCompletionReply(true))
	memoSettle()

	served := servedCompletion(t, b, uri, content, 2)
	if len(served.Items) != 5 {
		t.Fatalf("memo-served completion holds %d items, want the 5 upstream candidates", len(served.Items))
	}
	if !reflect.DeepEqual(served, latest) {
		t.Fatalf("memo-served completion must be the latest raw upstream ordering:\nserved: %+v\nlatest: %+v", served.Items, latest.Items)
	}
	expectNoCclsFrame(t, stdin.frames)
}

// TestMemoCompletionRealDifferenceResetsQuietWindow pins the anti-freeze
// side of the normalized comparison: a real candidate change — same label
// and sortText with a different detail, exactly the difference a field-blind
// sort key would swallow — must restart admission, so the quiet window
// restarts at the change. The probe below lands past the quiet window
// measured from the first observation but inside the window measured from
// the change: it must still forward. An over-eager comparator that equated
// the change away would confirm at the second request and serve here
// instead.
func TestMemoCompletionRealDifferenceResetsQuietWindow(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, 300*time.Millisecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")

	start := time.Now()
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoDetailCompletionReply("early index state"))
	time.Sleep(150 * time.Millisecond)
	// Real change: admission must restart around the settled value, with the
	// quiet window counting from this change, not from the first reply.
	settled := forwardedCompletion(t, b, stdin, child, uri, content, 2, memoDetailCompletionReply("settled index"))
	changed := time.Now()
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoDetailCompletionReply("settled index"))

	// 210ms after the change (inside the 300ms restarted window) and ~360ms
	// after the first observation (outside the window an over-eager
	// comparator would still be counting from): must forward, not serve.
	if wait := time.Until(changed.Add(210 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if time.Since(start) < 300*time.Millisecond {
		// The probe must be past the quiet window measured from the very
		// first observation for the over-eager variant to even be able to
		// serve; make that certain rather than incidental.
		time.Sleep(300*time.Millisecond - time.Since(start))
	}
	forwardedCompletion(t, b, stdin, child, uri, content, 2, memoDetailCompletionReply("settled index"))

	time.Sleep(310 * time.Millisecond)
	if list := servedCompletion(t, b, uri, content, 2); !reflect.DeepEqual(list, settled) {
		t.Fatalf("memo-served completion = %+v, want the latest settled answer %+v", list, settled)
	}
	expectNoCclsFrame(t, stdin.frames)
}

// TestCompletionListsEquivalentTieOrderOnly pins the equivalence side of the
// normalized admission comparison (memo.go): same candidate multiset in any
// order — identity, both same-scored pairs swapped, full reversal — compares
// equivalent, symmetrically, and neither input slice is reordered by the
// comparison (the served value keeps the upstream ordering).
func TestCompletionListsEquivalentTieOrderOnly(t *testing.T) {
	base := languages.CompletionList{Items: []languages.CompletionItem{
		{Label: "return", Kind: 6, SortText: "01", FilterText: "return"},
		{Label: "std::stoi(const string &Str)", Kind: 3, SortText: "15", Detail: "int std::stoi(const string &Str)", FilterText: "stoi", Documentation: "Converts to int."},
		{Label: "std::stoi(const wstring &Str)", Kind: 3, SortText: "15", Detail: "int std::stoi(const wstring &Str)", FilterText: "stoi", Documentation: "Converts to int (wide)."},
		{Label: "delete expression", Kind: 14, SortText: "20", InsertText: "delete", FilterText: "delete"},
		{Label: "delete[] expression", Kind: 14, SortText: "20", InsertText: "delete[]", FilterText: "delete"},
	}}
	before := append([]languages.CompletionItem{}, base.Items...)
	swappedPairs := languages.CompletionList{Items: []languages.CompletionItem{
		base.Items[0], base.Items[2], base.Items[1], base.Items[4], base.Items[3],
	}}
	reversed := languages.CompletionList{Items: []languages.CompletionItem{
		base.Items[4], base.Items[3], base.Items[2], base.Items[1], base.Items[0],
	}}

	if !completionListsEquivalent(base, base) {
		t.Fatal("a list must compare equivalent to itself")
	}
	if !completionListsEquivalent(base, swappedPairs) || !completionListsEquivalent(swappedPairs, base) {
		t.Fatal("same-scored pair swaps are the same candidate multiset and must compare equivalent, symmetrically")
	}
	if !completionListsEquivalent(base, reversed) {
		t.Fatal("a full reversal is the same candidate multiset and must compare equivalent")
	}
	if !completionListsEquivalent(languages.CompletionList{}, languages.CompletionList{Items: []languages.CompletionItem{}}) {
		t.Fatal("nil and empty item slices both mean zero candidates and must compare equivalent")
	}
	if !reflect.DeepEqual(base.Items, before) {
		t.Fatal("the equivalence comparison must not reorder its inputs; the served value keeps the upstream ordering")
	}
}

// TestCompletionListsEquivalentDetectsCandidateDifferences pins the
// difference side of the normalized admission comparison: every real change
// — the incompleteness bit, candidate count, or any retained field of a
// candidate (including same-label changes a field-blind sort key would
// swallow) — must compare non-equivalent, symmetrically.
func TestCompletionListsEquivalentDetectsCandidateDifferences(t *testing.T) {
	base := languages.CompletionList{Items: []languages.CompletionItem{
		{
			Label: "target", Kind: 3, Detail: "int target(void)",
			Documentation: "the target function", InsertText: "target()",
			SortText: "01", FilterText: "target", InsertTextFormat: 1,
			TextEdit: &languages.CompletionTextEdit{
				Range:   &languages.Range{StartLine: 1, StartCharacter: 2, EndLine: 1, EndCharacter: 2},
				NewText: "target()",
			},
			AdditionalTextEdits: []languages.TextEdit{
				{StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10, NewText: "x"},
			},
		},
		{Label: "other", Kind: 13, SortText: "02", FilterText: "other"},
	}}

	// withItem0 rebuilds the list with item 0 edited through a deep copy, so
	// pointed-to payloads (textEdit, additionalTextEdits) are never mutated
	// in place through the shared pointers of base.
	withItem0 := func(edit func(item *languages.CompletionItem)) func([]languages.CompletionItem) languages.CompletionList {
		return func(items []languages.CompletionItem) languages.CompletionList {
			changed := append([]languages.CompletionItem{}, items...)
			edit(&changed[0])
			return languages.CompletionList{Items: changed}
		}
	}
	cases := []struct {
		name   string
		mutate func([]languages.CompletionItem) languages.CompletionList
	}{
		{"isIncomplete flipped", func(items []languages.CompletionItem) languages.CompletionList {
			return languages.CompletionList{IsIncomplete: true, Items: items}
		}},
		{"candidate removed", func(items []languages.CompletionItem) languages.CompletionList {
			return languages.CompletionList{Items: items[:1]}
		}},
		{"candidate added", func(items []languages.CompletionItem) languages.CompletionList {
			grown := append(append([]languages.CompletionItem{}, items...), languages.CompletionItem{Label: "newcomer", SortText: "03"})
			return languages.CompletionList{Items: grown}
		}},
		{"label changed", withItem0(func(item *languages.CompletionItem) { item.Label = "targetX" })},
		{"kind changed", withItem0(func(item *languages.CompletionItem) { item.Kind = 4 })},
		{"detail changed with same label and sortText", withItem0(func(item *languages.CompletionItem) { item.Detail = "int target(int)" })},
		{"documentation changed", withItem0(func(item *languages.CompletionItem) { item.Documentation = "changed" })},
		{"insertText changed", withItem0(func(item *languages.CompletionItem) { item.InsertText = "target($0)" })},
		{"sortText changed", withItem0(func(item *languages.CompletionItem) { item.SortText = "00" })},
		{"filterText changed", withItem0(func(item *languages.CompletionItem) { item.FilterText = "tgt" })},
		{"insertTextFormat changed", withItem0(func(item *languages.CompletionItem) { item.InsertTextFormat = 2 })},
		{"textEdit dropped", withItem0(func(item *languages.CompletionItem) { item.TextEdit = nil })},
		{"textEdit range changed", withItem0(func(item *languages.CompletionItem) {
			item.TextEdit = &languages.CompletionTextEdit{
				Range:   &languages.Range{StartLine: 1, StartCharacter: 2, EndLine: 1, EndCharacter: 5},
				NewText: "target()",
			}
		})},
		{"additionalTextEdits dropped", withItem0(func(item *languages.CompletionItem) { item.AdditionalTextEdits = nil })},
		{"additionalTextEdit text changed", withItem0(func(item *languages.CompletionItem) {
			item.AdditionalTextEdits = []languages.TextEdit{{StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10, NewText: "y"}}
		})},
	}
	for _, tc := range cases {
		changed := tc.mutate(base.Items)
		if completionListsEquivalent(base, changed) {
			t.Fatalf("%s: a real candidate difference must not compare equivalent", tc.name)
		}
		if completionListsEquivalent(changed, base) {
			t.Fatalf("%s: non-equivalence must be symmetric", tc.name)
		}
	}
}

// TestMemoSymbolServesAcrossOwnMarkerFlip reproduces the S18
// syntax_update_after_edit shape: every sample bumps the snapshot revision
// and flips a marker, alternating between two byte states. Answers are keyed
// by content, so returning to earlier bytes must serve from the memo even
// though the revision only ever moves forward.
func TestMemoSymbolServesAcrossOwnMarkerFlip(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	contentA := []byte("int target(void);\n// marker=A\n")
	contentB := []byte("int target(void);\n// marker=B\n")
	snapshotWith := func(rev uint64, content []byte) {
		t.Helper()
		_, finish, err := b.BeginWorkspaceSnapshot(context.Background(), languages.WorkspaceSnapshot{
			Revision: rev,
			Documents: []languages.WorkspaceDocument{
				{URI: uri, LanguageID: "cpp", Content: content},
			},
		})
		if err != nil {
			t.Fatalf("BeginWorkspaceSnapshot: %v", err)
		}
		if err := finish(); err != nil {
			t.Fatalf("finish snapshot: %v", err)
		}
	}

	// Marker A: store, confirm (with the revision advancing underneath),
	// then serve.
	snapshotWith(1, contentA)
	forwardedSymbols(t, b, stdin, child, uri, contentA, 1, memoSymbolReply("target"))
	snapshotWith(2, contentA)
	forwardedSymbols(t, b, stdin, child, uri, contentA, 2, memoSymbolReply("target"))
	memoSettle()
	snapshotWith(3, contentA)
	syms := servedSymbols(t, b, uri, contentA, 3)
	if len(syms) != 1 || syms[0].Name != "target" {
		t.Fatalf("marker-A symbols were not served from the memo: %+v", syms)
	}
	expectNoCclsFrame(t, stdin.frames)

	// Flip to marker B: store, confirm, serve.
	snapshotWith(4, contentB)
	forwardedSymbols(t, b, stdin, child, uri, contentB, 4, memoSymbolReply("target"))
	snapshotWith(5, contentB)
	forwardedSymbols(t, b, stdin, child, uri, contentB, 5, memoSymbolReply("target"))
	memoSettle()
	snapshotWith(6, contentB)
	if syms := servedSymbols(t, b, uri, contentB, 6); len(syms) != 1 || syms[0].Name != "target" {
		t.Fatalf("marker-B symbols were not served from the memo: %+v", syms)
	}
	expectNoCclsFrame(t, stdin.frames)

	// Flip back to A at a yet-higher revision: the reconciliation may push
	// its didChange notification, but the retained A entry must answer the
	// query without forwarding it.
	snapshotWith(7, contentA)
	drainCclsFrames(t, stdin.frames)
	syms = servedSymbols(t, b, uri, contentA, 7)
	if len(syms) != 1 || syms[0].Name != "target" {
		t.Fatalf("memo-served document symbols = %+v, want the cached target symbol", syms)
	}
	expectNoCclsFrame(t, stdin.frames)
}

// TestMemoSymbolInvalidatedByOtherDocumentChange pins the cross-document
// guard: editing a different C/C++ document in the workspace changes this
// document's compilation world (includes, overlays), so its memoized answers
// must be dropped even though its own bytes did not change.
func TestMemoSymbolInvalidatedByOtherDocumentChange(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const main = "file:///x/main.cpp"
	const util = "file:///x/util.h"
	mainContent := []byte("#include \"util.h\"\nint target(void);\n")

	beginSnapshot := func(rev uint64, utilContent []byte) {
		t.Helper()
		_, finish, err := b.BeginWorkspaceSnapshot(context.Background(), languages.WorkspaceSnapshot{
			Revision: rev,
			Documents: []languages.WorkspaceDocument{
				{URI: main, LanguageID: "cpp", Content: mainContent},
				{URI: util, LanguageID: "cpp", Content: utilContent},
			},
		})
		if err != nil {
			t.Fatalf("BeginWorkspaceSnapshot: %v", err)
		}
		if err := finish(); err != nil {
			t.Fatalf("finish snapshot: %v", err)
		}
	}

	beginSnapshot(1, []byte("int helper(void);\n"))
	drainCclsFrames(t, stdin.frames)

	for i := 0; i < 2; i++ {
		forwardedSymbols(t, b, stdin, child, main, mainContent, 1, memoSymbolReply("target"))
	}
	memoSettle()
	served := servedSymbols(t, b, main, mainContent, 1)
	if len(served) != 1 || served[0].Name != "target" {
		t.Fatalf("expected memo-served symbols, got %+v", served)
	}
	expectNoCclsFrame(t, stdin.frames)

	// util.h changes on the next snapshot; main.cpp bytes stay identical.
	beginSnapshot(2, []byte("int helper(void); int extra(int);\n"))
	drainCclsFrames(t, stdin.frames)

	// The stale entry is gone: the next request must re-forward.
	forwardedSymbols(t, b, stdin, child, main, mainContent, 2, memoSymbolReply("target"))
}

// TestMemoInvalidatedBySourceChangeNotification pins the external source
// change hook: a watched-files event must drop memoized symbol answers.
// (The completion key additionally carries the nested workspace generation,
// which this same notification also advances.)
func TestMemoInvalidatedBySourceChangeNotification(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")

	for i := 0; i < 2; i++ {
		forwardedSymbols(t, b, stdin, child, uri, content, 2, memoSymbolReply("target"))
	}
	memoSettle()
	served := servedSymbols(t, b, uri, content, 2)
	if len(served) != 1 || served[0].Name != "target" {
		t.Fatalf("expected memo-served symbols, got %+v", served)
	}
	expectNoCclsFrame(t, stdin.frames)

	if err := b.NotifySourceChanges(context.Background(), []languages.SourceChange{
		{URI: "file:///x/other.h", Kind: languages.SourceChangeChanged},
	}); err != nil {
		t.Fatalf("NotifySourceChanges: %v", err)
	}
	drainCclsFrames(t, stdin.frames)

	forwardedSymbols(t, b, stdin, child, uri, content, 2, memoSymbolReply("target"))
}

// TestMemoKeysCarryEpochGenerationAndInvalidations pins the key guards as
// pure key-equality facts, independently of driving a live worker. The
// supervisor-epoch member is what invalidates every entry when clangd is
// replaced (recovery, crash restart): a behavioral recovery test is not
// feasible on the fake protocol harness because retiring the fake epoch
// closes its one io.Pipe pair and the supervisor restart loop cannot
// re-attach to it; the epoch key fact plus nested's epoch-on-restart
// invariant (a fresh epoch only exists after Attach+MarkReady on a new
// process) carry that path instead.
func TestMemoKeysCarryEpochGenerationAndInvalidations(t *testing.T) {
	base := completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 1, 2, 1, 7, 5, 2)
	if base != completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 1, 2, 1, 7, 5, 2) {
		t.Fatal("identical completion requests must produce identical keys")
	}
	if base == completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 1, 2, 1, 7, 6, 2) {
		t.Fatal("completion key must change with the nested workspace generation")
	}
	if base == completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 1, 2, 1, 8, 5, 2) {
		t.Fatal("completion key must change with the supervisor epoch")
	}
	if base == completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 1, 2, 1, 7, 5, 3) {
		t.Fatal("completion key must change with the bridge invalidation counter")
	}
	if base == completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\nint b;\n"), 1, 2, 1, 7, 5, 2) {
		t.Fatal("completion key must change with the document content")
	}
	if base == completionMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 2, 2, 1, 7, 5, 2) {
		t.Fatal("completion key must change with the position")
	}

	symbolBase := symbolMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 7, 2)
	if symbolBase != symbolMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 7, 2) {
		t.Fatal("identical symbol requests must produce identical keys")
	}
	if symbolBase == symbolMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 8, 2) {
		t.Fatal("symbol key must change with the supervisor epoch")
	}
	if symbolBase == symbolMemoKeyFor("file:///x/a.cpp", []byte("int a;\n"), 7, 3) {
		t.Fatal("symbol key must change with the bridge invalidation counter")
	}
	if symbolBase == symbolMemoKeyFor("file:///x/a.cpp", []byte("int a;\nint b;\n"), 7, 2) {
		t.Fatal("symbol key must change with the document content")
	}
}

// TestMemoCanonicalURIKeyEquivalence pins that URI spellings the workspace
// URI package treats as the same document share memo entries.
func TestMemoCanonicalURIKeyEquivalence(t *testing.T) {
	a := symbolMemoKeyFor("file:///C:/w/a.cpp", []byte("int a;\n"), 1, 0)
	b := symbolMemoKeyFor("file:///c:/w/a.cpp", []byte("int a;\n"), 1, 0)
	if a != b {
		t.Fatalf("canonical-equivalent URIs produced different memo keys: %+v vs %+v", a, b)
	}
}

// TestMemoDisabledUnderNestedRPCTraceEnv pins the stand-down: while the S18
// nested RPC trace is armed the memo must not intercept, because that
// evidence mode requires one nested timing sample per candidate request and
// a memo hit issues no nested request at all.
func TestMemoDisabledUnderNestedRPCTraceEnv(t *testing.T) {
	t.Setenv(cclsNestedRPCTimingEnv, "")
	if memoDisabledFromEnv() {
		t.Fatal("memo must be enabled without the nested RPC trace env")
	}
	t.Setenv(cclsNestedRPCTimingEnv, filepath.Join("tmp", "nested-rpc.json"))
	if !memoDisabledFromEnv() {
		t.Fatal("memo must stand down while the nested RPC trace env is armed")
	}

	b, stdin, child := newCclsProtocolBackend(t)
	b.memo.disabled = true
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")
	for i := 0; i < 3; i++ {
		forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	}
}

// TestMemoCapacityEvictsLeastRecentlyUsed pins the bound: the table never
// exceeds its capacity and the least recently used entry is the one evicted.
func TestMemoCapacityEvictsLeastRecentlyUsed(t *testing.T) {
	m := &cclsMemo{}
	m.mu.Lock()
	m.quietPeriod = time.Nanosecond
	m.mu.Unlock()

	store := func(uri, name string) symbolMemoKey {
		key := symbolMemoKeyFor(uri, []byte("int "+name+";\n"), 1, 0)
		value := []languages.DocumentSymbol{{Name: name}}
		m.symbolStore(key, value, time.Now())
		m.symbolStore(key, value, time.Now())
		return key
	}
	first := store("file:///x/f00.cpp", "first")
	for i := 1; i < cclsMemoCapacity; i++ {
		store("file:///x/f"+string(rune('0'+i/10))+string(rune('0'+i%10))+".cpp", "v")
	}
	if len(m.symbols) != cclsMemoCapacity {
		t.Fatalf("symbol memo holds %d entries, want exactly the capacity %d", len(m.symbols), cclsMemoCapacity)
	}
	memoSettle()
	if _, ok := m.symbolServe(first, time.Now()); !ok {
		t.Fatal("confirmed quiet entry must serve before eviction")
	}
	last := store("file:///x/fNew.cpp", "newcomer")
	if len(m.symbols) != cclsMemoCapacity {
		t.Fatalf("symbol memo holds %d entries after insert, want the capacity %d", len(m.symbols), cclsMemoCapacity)
	}
	// `first` was touched most recently, so some other (older) entry was
	// evicted; the newcomer and `first` must both answer.
	memoSettle()
	if _, ok := m.symbolServe(first, time.Now()); !ok {
		t.Fatal("most recently used entry must survive eviction")
	}
	if _, ok := m.symbolServe(last, time.Now()); !ok {
		t.Fatal("newly inserted entry must be present")
	}
}

// TestMemoClonesServedValues pins that callers cannot corrupt the cache
// through the returned slices.
func TestMemoClonesServedValues(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	memoQuiet(b, time.Nanosecond)
	const uri = "file:///x/main.cpp"
	content := []byte("int target(void);\n")

	for i := 0; i < 2; i++ {
		forwardedCompletion(t, b, stdin, child, uri, content, 2, memoCompletionItems("target"))
	}
	memoSettle()
	first := servedCompletion(t, b, uri, content, 2)
	first.Items[0].Label = "mutated"

	second := servedCompletion(t, b, uri, content, 2)
	if len(second.Items) != 1 || second.Items[0].Label != "target" {
		t.Fatalf("caller mutation leaked into the memo cache: %+v", second.Items)
	}
	expectNoCclsFrame(t, stdin.frames)
}
