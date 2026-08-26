// Package graph 实现五层依赖图与选择性失效（goal.md §M0-M4）：
// File→Module→Build Target→Workspace→External 五层节点，
// imports/includes/generates/expands/implements/inherits 六种边，
// 沿反向依赖的 BFS 失效传播（§M1）、公共语义指纹跳过（§M2）、
// MaxFanout 安全阀（§M3），以及显式生成依赖建模（§M4）。
//
// Owned mutable state: nodes/edges/rev 三张 map 与 next ID 计数器、
// genOf/genOut 生成器登记表——全部仅在包内访问，无后台 goroutine。
//
// Invariants:
//  1. Propagate 只返回可经反向边证明的受影响者，绝不盲目扩大范围（§M1）；
//     无法证明时由 ErrFanoutTooWide 交还调用方全清（§M3：正确性优先）。
//  2. 公共语义指纹未变的节点不继续向下传播，下游可证明安全（§M2）。
//  3. 每个生成物必带显式 EdgeGenerates 边与 Generator 登记，
//     工具版本升级即成为一等失效原因（§M4）。
package graph

import "sort"

// Layer 是图的五层之一（goal.md §M0）。
type Layer uint8

const (
	LayerFile        Layer = iota // 源文件
	LayerModule                   // Module/Package/Crate
	LayerBuildTarget              // 构建目标
	LayerWorkspace                // 工作区
	LayerExternal                 // 外部依赖
)

// EdgeKind 是六种依赖边（goal.md §M0）。
type EdgeKind uint8

const (
	EdgeImports    EdgeKind = iota // From 引用 To
	EdgeIncludes                   // 文本包含（如 C #include）
	EdgeGenerates                  // To 由 From 生成（§M4）
	EdgeExpands                    // 宏/模板展开
	EdgeImplements                 // 接口实现
	EdgeInherits                   // 类继承
)

// NodeID 是节点的稳定标识，由 AddNode 单调分配。
type NodeID int

// Node 是图中一个实体；Path 为文件路径或层内限定名。
type Node struct {
	ID    NodeID
	Layer Layer
	Path  string
}

// Edge 表示 From 依赖 To（方向：From → To = From 受 To 影响）。
type Edge struct {
	From NodeID
	To   NodeID
	Kind EdgeKind
}

// Graph 是五层依赖图。非并发安全：
// ponytail: 单 goroutine 使用；引擎侧出现并发编辑时在外层加 RWMutex 即可。
type Graph struct {
	nodes map[NodeID]Node
	edges map[NodeID][]Edge // 出边索引：From → 边列表
	rev   map[NodeID][]Edge // 反向索引：To → 指向它的边列表
	next  NodeID

	genOf  map[NodeID]Generator // 生成物 → 登记的生成器（generated.go 用）
	genOut map[string][]NodeID  // Tool 名 → 该工具的全部生成物
}

// New 返回空图。
func New() *Graph {
	return &Graph{
		nodes:  make(map[NodeID]Node),
		edges:  make(map[NodeID][]Edge),
		rev:    make(map[NodeID][]Edge),
		genOf:  make(map[NodeID]Generator),
		genOut: make(map[string][]NodeID),
	}
}

// AddNode 新增节点并返回其 ID。
func (g *Graph) AddNode(layer Layer, path string) NodeID {
	id := g.next
	g.next++
	g.nodes[id] = Node{ID: id, Layer: layer, Path: path}
	return id
}

// AddEdge 记录一条 From→To 的依赖边。
func (g *Graph) AddEdge(from, to NodeID, kind EdgeKind) {
	e := Edge{From: from, To: to, Kind: kind}
	g.edges[from] = append(g.edges[from], e)
	g.rev[to] = append(g.rev[to], e)
}

// Node 返回节点本体。
func (g *Graph) Node(id NodeID) (Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// Edges 返回 from 的全部出边副本。
func (g *Graph) Edges(from NodeID) []Edge {
	out := make([]Edge, len(g.edges[from]))
	copy(out, g.edges[from])
	return out
}

// ReverseDependents 返回直接依赖 id 的节点（谁引用了我）。
func (g *Graph) ReverseDependents(id NodeID) []NodeID {
	in := g.rev[id]
	out := make([]NodeID, len(in))
	for i, e := range in {
		out[i] = e.From
	}
	return out
}

// DetectCycles 用 DFS 三色标记找出全部环（§M0 建图自检）。
func (g *Graph) DetectCycles() [][]NodeID {
	const (
		white uint8 = iota
		gray
		black
	)
	color := make(map[NodeID]uint8, len(g.nodes))
	stack := make([]NodeID, 0, len(g.nodes))
	var cycles [][]NodeID

	var dfs func(NodeID)
	dfs = func(n NodeID) {
		color[n] = gray
		stack = append(stack, n)
		for _, e := range g.edges[n] {
			switch color[e.To] {
			case white:
				dfs(e.To)
			case gray: // 回边：栈中 e.To 起即为一个环
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == e.To {
						cyc := append([]NodeID(nil), stack[i:]...)
						cycles = append(cycles, cyc)
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}

	ids := make([]NodeID, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] }) // 确定性遍历序
	for _, id := range ids {
		if color[id] == white {
			dfs(id)
		}
	}
	return cycles
}
