package graph

import "github.com/omnilsp/omni/internal/semantic/query"

// Generator 标识一个代码生成工具及其版本（goal.md §M4）。
type Generator struct {
	Tool    string
	Version string
}

// RegisterGenerated 登记生成依赖：source --EdgeGenerates--> generated，
// 并记录生成器身份供版本比对。幂等：重复登记不产生重复边或重复表项。
func RegisterGenerated(g *Graph, generated, source NodeID, gen Generator) {
	g.genOf[generated] = gen
	known := false
	for _, o := range g.genOut[gen.Tool] {
		if o == generated {
			known = true
			break
		}
	}
	if !known {
		g.genOut[gen.Tool] = append(g.genOut[gen.Tool], generated)
	}

	for _, e := range g.edges[source] {
		if e.To == generated && e.Kind == EdgeGenerates {
			return
		}
	}
	g.AddEdge(source, generated, EdgeGenerates)
}

// InvalidateGenerator 在 generator 版本变化时（登记版本 ≠ gen.Version）
// 失效其全部生成物及其下游，并把登记更新为 gen.Version（§M4）。
// 同 Tool 同 Version 重复调用为无操作。
func InvalidateGenerator(g *Graph, gen Generator) ([]query.Dep, error) {
	var stale []NodeID
	for _, out := range g.genOut[gen.Tool] {
		if old, ok := g.genOf[out]; ok && old.Version != gen.Version {
			stale = append(stale, out)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	for _, out := range stale {
		g.genOf[out] = gen
	}
	return Propagate(g, stale, Options{})
}
