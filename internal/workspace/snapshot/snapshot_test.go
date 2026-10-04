package snapshot

// Invariants:
//   INV-SNAPSHOT-001: published Snapshot is immutable
//   INV-SNAPSHOT-002: no two-Snapshot reads per request
//   INV-SNAPSHOT-003: old Snapshot not mutated by new publication

import (
	"testing"
)

func TestNewSnapshot(t *testing.T) {
	docs := map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", LanguageID: "go", Version: 1, Content: []byte("package main")},
	}
	snap := New("workspace1", 1, docs)

	id := snap.ID()
	if id.WorkspaceID != "workspace1" {
		t.Errorf("expected workspace1, got %s", id.WorkspaceID)
	}
	if id.Revision != 1 {
		t.Errorf("expected revision 1, got %d", id.Revision)
	}
}

func TestSnapshotDocuments(t *testing.T) {
	docs := map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", Content: []byte("package main")},
		"file:///b.go": {URI: "file:///b.go", Content: []byte("package util")},
	}
	snap := New("ws", 1, docs)

	uris := snap.Documents()
	if len(uris) != 2 {
		t.Errorf("expected 2 documents, got %d", len(uris))
	}
}

func TestSnapshotDocument(t *testing.T) {
	docs := map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", Content: []byte("hello")},
	}
	snap := New("ws", 1, docs)

	d := snap.Document("file:///a.go")
	if d == nil {
		t.Fatal("expected document")
	}
	if string(d.Content) != "hello" {
		t.Errorf("expected hello, got %s", string(d.Content))
	}

	// Non-existent document.
	d = snap.Document("file:///nonexistent.go")
	if d != nil {
		t.Error("non-existent document should return nil")
	}
}

func TestSnapshotCanonicalIdentityPreservesDisplayURI(t *testing.T) {
	displayURI := "file:///C:/test.go"
	aliasURI := "file:///c:/test.go"
	snap := New("ws", 1, map[string]DocumentSnapshot{
		displayURI: {URI: displayURI, Content: []byte("source")},
	})

	doc := snap.Document(aliasURI)
	if doc == nil {
		t.Fatal("canonical URI alias did not resolve document")
	}
	if doc.URI != displayURI {
		t.Fatalf("document display URI = %q, want %q", doc.URI, displayURI)
	}
	if got := snap.Documents(); len(got) != 1 || got[0] != displayURI {
		t.Fatalf("Documents lost display spelling: %v", got)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	original := []byte("original")
	docs := map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", Content: original},
	}
	snap := New("ws", 1, docs)

	// Contract (see New): the MAP is defensively copied; content bytes are
	// shared by design — immutability is enforced upstream at the VFS write
	// boundary (vfs.Open/Update clone once). Mutating the docs map here must
	// not affect the published snapshot.
	docs["file:///intruder.go"] = DocumentSnapshot{URI: "file:///intruder.go"}
	delete(docs, "file:///a.go")

	if snap.Document("file:///intruder.go") != nil {
		t.Error("map mutation after New leaked into snapshot")
	}
	d := snap.Document("file:///a.go")
	if d == nil {
		t.Fatal("expected document")
	}

	// Read boundary: mutating the returned copy must not touch the snapshot.
	d.Content[0] = 'X'
	if again := snap.Document("file:///a.go"); string(again.Content) != "original" {
		t.Errorf("Document() leak: %q", again.Content)
	}
}

func TestManagerPublishAndCurrent(t *testing.T) {
	m := NewManager()

	if m.Current() != nil {
		t.Error("initial current should be nil")
	}

	snap1 := New("ws", 1, map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", Content: []byte("v1")},
	})
	m.Publish(snap1)

	cur := m.Current()
	if cur == nil {
		t.Fatal("current should not be nil after publish")
	}
	if cur.ID().Revision != 1 {
		t.Errorf("expected revision 1, got %d", cur.ID().Revision)
	}

	snap2 := New("ws", 2, map[string]DocumentSnapshot{
		"file:///a.go": {URI: "file:///a.go", Content: []byte("v2")},
	})
	m.Publish(snap2)

	cur = m.Current()
	if cur.ID().Revision != 2 {
		t.Errorf("expected revision 2, got %d", cur.ID().Revision)
	}
}

func TestManagerConcurrency(t *testing.T) {
	m := NewManager()

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(n uint64) {
			snap := New("ws", n, map[string]DocumentSnapshot{
				"file:///a.go": {URI: "file:///a.go", Content: []byte("content")},
			})
			m.Publish(snap)
			done <- true
		}(uint64(i))
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	cur := m.Current()
	if cur == nil {
		t.Error("current should not be nil")
	}
}
