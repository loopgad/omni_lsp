package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/virtual"
)

// TestVirtualMappingsReleasedOnHostCloseRenameAndTeardown pins bug round 20
// A#11 (圈 4): the virtual registry documented UnregisterHost as "the cleanup
// hook for host-document close (didClose / workspace teardown)" but nothing
// ever called it — UnregisterHost/UnregisterVirtual had zero production call
// sites, so a host document's mappings survived didClose and didRenameFiles
// forever. Harmless while no in-process producer exists (the registry is an
// embedder API), but the moment a §D12 producer lands, every close/rename
// would accumulate stale mappings and ResolveHost/semanticSource would read
// a dead host. All three release points are pinned here: host didClose,
// host rename (old URI), and a CloseSession backstop sweep.
func TestVirtualMappingsReleasedOnHostCloseRenameAndTeardown(t *testing.T) {
	s := New(DefaultConfig())
	reg := s.VirtualRegistry()
	seg := []virtual.Segment{{HostStart: 0, HostEnd: 4, VirtStart: 0, VirtEnd: 4, Quality: virtual.QualityExact}}

	// Leg 1: host didClose must drop that host's virtual mappings.
	host1 := "file:///w/close.go"
	if err := reg.RegisterVirtual("virtual:///w/close.go#0", host1, seg); err != nil {
		t.Fatal(err)
	}
	closeMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "c", IsStr: true}, "textDocument/didClose",
		json.RawMessage(`{"textDocument":{"uri":"`+host1+`"}}`))
	if _, err := s.handleDidClose(context.Background(), closeMsg); err != nil {
		t.Fatalf("didClose: %v", err)
	}
	if got := reg.LookupVirtual(host1); len(got) != 0 {
		t.Fatalf("virtual mapping outlived its host's didClose: %v (cleanup hook never wired)", got)
	}

	// Leg 2: workspace/didRenameFiles must drop the old URI's mappings.
	host2 := "file:///w/rename-old.go"
	if err := reg.RegisterVirtual("virtual:///w/rename-old.go#0", host2, seg); err != nil {
		t.Fatal(err)
	}
	renameMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "r", IsStr: true}, "workspace/didRenameFiles",
		json.RawMessage(`{"files":[{"oldUri":"`+host2+`","newUri":"file:///w/rename-new.go"}]}`))
	if _, err := s.handleDidRenameFiles(context.Background(), renameMsg); err != nil {
		t.Fatalf("didRenameFiles: %v", err)
	}
	if got := reg.LookupVirtual(host2); len(got) != 0 {
		t.Fatalf("virtual mapping outlived its host's rename: %v", got)
	}

	// Leg 3: CloseSession is the backstop — nothing survives teardown.
	host3 := "file:///w/teardown.go"
	if err := reg.RegisterVirtual("virtual:///w/teardown.go#0", host3, seg); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSession(); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := reg.LookupVirtual(host3); len(got) != 0 {
		t.Fatalf("virtual mapping survived CloseSession: %v", got)
	}
}
