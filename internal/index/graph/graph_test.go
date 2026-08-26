package graph

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/omnilsp/omni/internal/semantic/query"
)

func depSet(deps []query.Dep) map[string]bool {
	out := make(map[string]bool, len(deps))
	for _, d := range deps {
		out[d.Kind+":"+d.ID] = true
	}
	return out
}

func wantDeps(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d deps %v, want %d %v", len(got), got, len(want), want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing dep %q in %v", w, got)
		}
	}
}

// TestM0_LayersAndEdges 五层建图 + 六种边 + 反向查询（goal.md §M0）。
func TestM0_LayersAndEdges(t *testing.T) {
	g := New()
	f := g.AddNode(LayerFile, "main.go")
	m := g.AddNode(LayerModule, "example.com/m")
	bt := g.AddNode(LayerBuildTarget, "//cmd:bin")
	ws := g.AddNode(LayerWorkspace, "ws-root")
	ext := g.AddNode(LayerExternal, "golang.org/x/tools")

	g.AddEdge(f, m, EdgeImports)
	g.AddEdge(m, bt, EdgeIncludes)
	g.AddEdge(bt, ws, EdgeExpands)
	g.AddEdge(ws, ext, EdgeImports)
	g.AddEdge(ext, bt, EdgeImplements)
	g.AddEdge(m, ws, EdgeGenerates)
	g.AddEdge(ws, m, EdgeInherits)

	n, ok := g.Node(f)
	if !ok || n.Layer != LayerFile || n.Path != "main.go" {
		t.Fatalf("Node(%d) = %+v, %v", f, n, ok)
	}

	seen := map[EdgeKind]bool{}
	for _, id := range []NodeID{f, m, bt, ws, ext} {
		for _, e := range g.Edges(id) {
			seen[e.Kind] = true
		}
	}
	for k := EdgeImports; k <= EdgeInherits; k++ {
		if !seen[k] {
			t.Fatalf("edge kind %d missing from graph", k)
		}
	}

	deps := g.ReverseDependents(m) // f imports m，ws inherits m
	sort.Slice(deps, func(i, j int) bool { return deps[i] < deps[j] })
	if len(deps) != 2 || deps[0] != f || deps[1] != ws {
		t.Fatalf("ReverseDependents(m) = %v, want [%d %d]", deps, f, ws)
	}
}

// TestM1_SelectivePropagation 改 b 只影响 b/c，不影响无关的 d（§M1）。
func TestM1_SelectivePropagation(t *testing.T) {
	g := New()
	a := g.AddNode(LayerFile, "a.go")
	b := g.AddNode(LayerFile, "b.go") // b imports a
	c := g.AddNode(LayerFile, "c.go") // c imports b
	d := g.AddNode(LayerFile, "d.go")
	g.AddEdge(b, a, EdgeImports)
	g.AddEdge(c, b, EdgeImports)
	g.AddEdge(d, a, EdgeImports) // d 只依赖 a，与 b 无关

	deps, err := Propagate(g, []NodeID{b}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantDeps(t, depSet(deps), "file:b.go", "file:c.go")
}

// TestM2_FingerprintSkip 指纹未变的上游不触发下游失效，用访问计数器证明（§M2）。
func TestM2_FingerprintSkip(t *testing.T) {
	g := New()
	u := g.AddNode(LayerFile, "u.go")
	dn := g.AddNode(LayerFile, "d.go") // d imports u
	g.AddEdge(dn, u, EdgeImports)

	base := map[NodeID]uint64{u: Fingerprint(g, u), dn: Fingerprint(g, dn)}
	visits := 0
	opts := Options{Visit: func(NodeID) { visits++ }}

	// u 内容变了但公共语义指纹未变 → 下游完全不被触碰
	same := map[NodeID]uint64{u: base[u], dn: base[dn]}
	deps, err := PropagateWithFingerprints(g, []NodeID{u}, base, same, opts)
	if err != nil {
		t.Fatal(err)
	}
	wantDeps(t, depSet(deps), "file:u.go")
	if visits != 1 {
		t.Fatalf("visits = %d, want 1（下游未被触碰）", visits)
	}

	// 指纹变化 → 下游进入失效集
	changed := map[NodeID]uint64{u: base[u] ^ 1, dn: base[dn]}
	deps, err = PropagateWithFingerprints(g, []NodeID{u}, base, changed, opts)
	if err != nil {
		t.Fatal(err)
	}
	wantDeps(t, depSet(deps), "file:u.go", "file:d.go")
	if visits != 3 {
		t.Fatalf("visits = %d, want 3", visits)
	}
}

// TestM3_FanoutSafetyValve 星型 100 叶子超限报错，上限内正常（§M3）。
func TestM3_FanoutSafetyValve(t *testing.T) {
	g := New()
	center := g.AddNode(LayerFile, "center.go")
	for i := 0; i < 100; i++ {
		leaf := g.AddNode(LayerFile, fmt.Sprintf("leaf%02d.go", i))
		g.AddEdge(leaf, center, EdgeImports)
	}

	if _, err := Propagate(g, []NodeID{center}, Options{MaxFanout: 10}); !errors.Is(err, ErrFanoutTooWide) {
		t.Fatalf("err = %v, want ErrFanoutTooWide", err)
	}

	deps, err := Propagate(g, []NodeID{center}, Options{MaxFanout: 101})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 101 {
		t.Fatalf("len(deps) = %d, want 101", len(deps))
	}
}

// TestM4_GeneratorInvalidation proto 改 / generator 升级均波及 generated 及其下游（§M4）。
func TestM4_GeneratorInvalidation(t *testing.T) {
	g := New()
	proto := g.AddNode(LayerFile, "schema.proto")
	pb := g.AddNode(LayerFile, "schema.pb.go")
	user := g.AddNode(LayerFile, "user.go") // user imports pb
	g.AddEdge(user, pb, EdgeImports)
	RegisterGenerated(g, pb, proto, Generator{Tool: "protoc-gen-go", Version: "v1.31"})

	// 源 schema.proto 改动 → 生成物与其下游全部失效
	deps, err := Propagate(g, []NodeID{proto}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantDeps(t, depSet(deps), "file:schema.proto", "file:schema.pb.go", "file:user.go")

	// generator 版本升级 → 输出与下游失效
	deps, err = InvalidateGenerator(g, Generator{Tool: "protoc-gen-go", Version: "v1.32"})
	if err != nil {
		t.Fatal(err)
	}
	wantDeps(t, depSet(deps), "file:schema.pb.go", "file:user.go")

	// 同版本再次检查 → 无可失效项
	deps, err = InvalidateGenerator(g, Generator{Tool: "protoc-gen-go", Version: "v1.32"})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("同版本失效了 %d 个节点，want 0", len(deps))
	}
}

// TestCycleDetection A→B→A 报环，无环图为空。
func TestCycleDetection(t *testing.T) {
	g := New()
	a := g.AddNode(LayerFile, "a.go")
	b := g.AddNode(LayerFile, "b.go")
	g.AddEdge(a, b, EdgeImports)
	g.AddEdge(b, a, EdgeImports)
	c := g.AddNode(LayerFile, "c.go")
	g.AddEdge(c, a, EdgeImports) // 环外参与者

	cycles := g.DetectCycles()
	if len(cycles) != 1 {
		t.Fatalf("cycles = %v, want 恰好 1 个环", cycles)
	}
	cyc := append([]NodeID(nil), cycles[0]...)
	sort.Slice(cyc, func(i, j int) bool { return cyc[i] < cyc[j] })
	if len(cyc) != 2 || cyc[0] != a || cyc[1] != b {
		t.Fatalf("cycle = %v, want {%d %d}", cyc, a, b)
	}

	if cs := New().DetectCycles(); len(cs) != 0 {
		t.Fatalf("空图报环 %v", cs)
	}
}
