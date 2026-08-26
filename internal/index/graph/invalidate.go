package graph

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"

	"github.com/omnilsp/omni/internal/semantic/query"
)

// ErrFanoutTooWide 是 §M3 安全阀：单次传播受影响节点数超过 Options.MaxFanout
// 时返回，由调用方决定扩大范围或全清——正确性优先于缓存保留。
var ErrFanoutTooWide = errors.New("graph: 受影响节点数超过 MaxFanout，调用方应全量失效")

// Options 控制 Propagate 行为。
type Options struct {
	MaxFanout int          // 单次传播的受影响节点上限；≤0 表示不限
	Visit     func(NodeID) // 可选观察钩子，每失效一个节点回调一次
}

// Propagate 从变更节点沿 ReverseDependents BFS 收集全部传递受影响者，
// 映射为 query.Dep 集：LayerFile → Dep{Kind:"file", ID:path}，
// 其他层 → Dep{Kind:"node", ID:nodeID}（§M1）。
func Propagate(g *Graph, changed []NodeID, opts Options) ([]query.Dep, error) {
	return propagate(g, changed, nil, nil, opts)
}

// PropagateWithFingerprints 是带 §M2 跳过优化的变体：
// prev 为变更前指纹、curr 为变更后指纹；某节点两者相等即公共语义未变，
// 不再向其下游传播。缺失指纹按"已变化"保守处理。
func PropagateWithFingerprints(g *Graph, changed []NodeID, prev, curr map[NodeID]uint64, opts Options) ([]query.Dep, error) {
	return propagate(g, changed, prev, curr, opts)
}

func propagate(g *Graph, changed []NodeID, prev, curr map[NodeID]uint64, opts Options) ([]query.Dep, error) {
	limit := opts.MaxFanout
	seen := make(map[NodeID]bool, len(changed))
	queue := make([]NodeID, 0, len(changed))
	var deps []query.Dep

	emit := func(id NodeID) bool { // false = 超 MaxFanout
		if limit > 0 && len(deps) >= limit {
			return false
		}
		deps = append(deps, toDep(g, id))
		if opts.Visit != nil {
			opts.Visit(id)
		}
		return true
	}

	for _, c := range changed {
		if seen[c] {
			continue
		}
		seen[c] = true
		queue = append(queue, c)
		if !emit(c) {
			return nil, ErrFanoutTooWide
		}
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]

		// §M2：该节点公共语义指纹未变 → 下游可证明安全，停止传播。
		pv, okP := prev[n]
		cv, okC := curr[n]
		if okP && okC && pv == cv {
			continue
		}

		// 常规边：影响沿反向边流向依赖方（改 To 波及 From）。
		spread := func(id NodeID) bool {
			if seen[id] {
				return true
			}
			seen[id] = true
			queue = append(queue, id)
			return emit(id)
		}
		for _, e := range g.rev[n] {
			if e.Kind == EdgeGenerates {
				// 生成物再生不影响其源文件，倒灌被排除（§M4）。
				continue
			}
			if !spread(e.From) {
				return nil, ErrFanoutTooWide
			}
		}
		// §M4：generates 边例外，影响沿正向流向生成物（改源波及产物）。
		for _, e := range g.edges[n] {
			if e.Kind == EdgeGenerates && !spread(e.To) {
				return nil, ErrFanoutTooWide
			}
		}
	}
	return deps, nil
}

// Fingerprint 计算 §M2 公共语义指纹：层、路径与全部出边的 FNV-1a 组合。
// 结构性推导，确定性输出；语言后端可替换为导出签名等更精细的实现。
func Fingerprint(g *Graph, id NodeID) uint64 {
	h := fnv.New64a()
	n := g.nodes[id]
	fmt.Fprintf(h, "L%d\x00%s\x00", n.Layer, n.Path)
	outs := make([]Edge, len(g.edges[id]))
	copy(outs, g.edges[id])
	sort.Slice(outs, func(i, j int) bool {
		if outs[i].Kind != outs[j].Kind {
			return outs[i].Kind < outs[j].Kind
		}
		return outs[i].To < outs[j].To
	})
	for _, e := range outs {
		fmt.Fprintf(h, "%d>%d;", e.Kind, e.To)
	}
	return h.Sum64()
}

// toDep 把图节点映射为引擎可消费的 query.Dep。
func toDep(g *Graph, id NodeID) query.Dep {
	n := g.nodes[id]
	if n.Layer == LayerFile {
		return query.Dep{Kind: "file", ID: n.Path}
	}
	return query.Dep{Kind: "node", ID: strconv.Itoa(int(id))}
}
