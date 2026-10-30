package server

import (
	"reflect"
	"sort"

	"github.com/omnilsp/omni/internal/languages"
)

type semanticIndexBinding struct {
	provider languages.SemanticIndexProvider
	planner  languages.SemanticIndexRequestBuilder
}

type namedSemanticIndexBinding struct {
	language string
	binding  semanticIndexBinding
}

func (s *Server) semanticIndexBindings() []namedSemanticIndexBinding {
	s.mu.RLock()
	bindings := make([]namedSemanticIndexBinding, 0, len(s.semanticIndexes))
	for language, binding := range s.semanticIndexes {
		bindings = append(bindings, namedSemanticIndexBinding{language: language, binding: binding})
	}
	s.mu.RUnlock()
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].language < bindings[j].language })
	unique := bindings[:0]
	for _, binding := range bindings {
		duplicate := false
		for i, prior := range unique {
			if sameSemanticCapability(binding.binding.provider, prior.binding.provider) &&
				sameSemanticCapability(binding.binding.planner, prior.binding.planner) {
				duplicate = true
				// One capability per family, but report it under the canonical
				// family key: alias and dialect keys (c, javascript,
				// typescriptreact, javascriptreact) sort before their canonical
				// key, and downstream exact-language consumers (SCIP/LSIF
				// import selection, reindex labels) must keep seeing the
				// canonical language the artifacts were planned under.
				if semanticIndexFamily(binding.language) == binding.language &&
					semanticIndexFamily(prior.language) != prior.language {
					unique[i] = binding
				}
				break
			}
		}
		if !duplicate {
			unique = append(unique, binding)
		}
	}
	return unique
}

func sameSemanticCapability(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() {
		return false
	}
	if av.Type().Comparable() {
		return av.Interface() == bv.Interface()
	}
	if av.Kind() == reflect.Pointer {
		return av.Pointer() == bv.Pointer()
	}
	return false
}
