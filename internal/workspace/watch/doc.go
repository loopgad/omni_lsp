// Package watch implements the §D14 workspace file watcher as a
// zero-dependency polling scanner (dependency whitelist rules out fsnotify).
//
// Invariants:
//
//	INV-WATCH-001: the fingerprint map is bounded by the walked tree, a
//	maximum depth of 64, a per-file ceiling of 256 MiB and a whole-tree ceiling
//	of 8 GiB — runaway trees cannot stall the poll loop. The two byte ceilings
//	were implemented without being written down here; the depth figure was
//	written down wrong.
//
//	INV-WATCH-002: the first Scan establishes the baseline and reports no
//	events; quiet trees produce zero events thereafter.
//
//	INV-WATCH-003: unreadable and over-limit entries never abort the walk, but
//	any one of them invalidates the pass: Scan reports no events and keeps the
//	previous baseline rather than diffing a tree it could not read fully. A
//	partial result would be worse than none, because it reads as a deletion.
package watch
