package scheduler

// goal.md §F8: requests SHOULD be cost-classified and "Large work MUST have
// stricter concurrency caps". This file owns the classification policy and the
// per-class cap derivation; scheduler.go applies the caps at the pick point.

// resolveClass maps CostUnset to DefaultCostClass. Both the enqueue path
// (trySend) and the pick loop go through it, so an unclassified request can
// never be parked in the dead CostUnset lane, slip into the wide interactive
// lanes, or dodge the strict Large/VeryLarge caps (fail-closed §F8 default).
func resolveClass(c CostClass) CostClass {
	if c == CostUnset {
		return DefaultCostClass
	}
	return c
}

// ClassifyCost maps LSP method families to §F8 cost classes. It lives in the
// scheduler package so the server's dispatch hook stays a single line; the
// scheduler learns method NAME strings only and keeps INV-ARCH-002 (no
// protocol package import) intact. Unknown methods fall to DefaultCostClass —
// never to a wide or cap-exempt lane.
//
// Methods the server handles inline (lifecycle, document state transitions —
// see server.go inlineMethods) never reach the scheduler, so they have no
// entry here.
func ClassifyCost(method string) CostClass {
	switch method {
	case "textDocument/hover", "textDocument/completion", "textDocument/signatureHelp",
		"omnilsp/status", "omnilsp/explain", "omnilsp/backendStatus",
		"omnilsp/resultMeta", "omnilsp/queryTrace", "omnilsp/indexStats":
		return CostTiny // local lookup / cache hit
	case "textDocument/definition", "textDocument/declaration",
		"textDocument/typeDefinition", "textDocument/implementation",
		"textDocument/documentSymbol", "textDocument/inlayHint",
		"textDocument/codeAction", "textDocument/formatting",
		"textDocument/diagnostic", "textDocument/prepareRename":
		return CostSmall // single-file parse / query
	case "textDocument/rename", "textDocument/semanticTokens/full",
		"textDocument/semanticTokens/range":
		return CostMedium // package/module-level semantic work
	case "textDocument/references", "workspace/symbol":
		return CostLarge // workspace references / index query
	case "omnilsp/reindex":
		return CostVeryLarge // full indexing / rebuild (synchronous rebuild in handleReindex)
	default:
		return DefaultCostClass
	}
}

// derivedClassCap returns the §F8 default concurrency cap for a class at the
// given worker-pool size. Tiny/Small (interactive lookups) span the whole
// pool; Medium — the DefaultCostClass safety net — is held to ¾ so an
// unclassified flood can never occupy every worker; Large and VeryLarge, the
// classes §F8's MUST binds, are held to ½ and ¼.
func derivedClassCap(c CostClass, maxConcurrent int) int {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	switch c {
	case CostMedium:
		return max(1, maxConcurrent*3/4)
	case CostLarge:
		return max(1, maxConcurrent/2)
	case CostVeryLarge:
		return max(1, maxConcurrent/4)
	default: // CostTiny, CostSmall: interactive lookups get the full pool
		return maxConcurrent
	}
}

// classCapFor resolves the effective cap for a class: an explicit positive
// Config.ClassCaps entry wins, clamped to [1, MaxConcurrent] so a cap can
// neither strand a class at zero (its requests would queue forever) nor
// exceed the worker pool. Zero/negative/absent entries derive from
// MaxConcurrent.
func classCapFor(cfg Config, c CostClass) int {
	if cfg.ClassCaps != nil {
		if v := cfg.ClassCaps[c]; v > 0 {
			if v > cfg.MaxConcurrent {
				v = cfg.MaxConcurrent
			}
			if v < 1 {
				v = 1
			}
			return v
		}
	}
	return derivedClassCap(c, cfg.MaxConcurrent)
}
