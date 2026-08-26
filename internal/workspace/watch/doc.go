// Package watch implements the §D14 workspace file watcher as a
// zero-dependency polling scanner (dependency whitelist rules out fsnotify).
//
// Invariants:
//
//	INV-WATCH-001: the fingerprint map is bounded by the walked tree and a
//	maximum depth of 16 — runaway trees cannot stall the poll loop.
//
//	INV-WATCH-002: the first Scan establishes the baseline and reports no
//	events; quiet trees produce zero events thereafter.
//
//	INV-WATCH-003: unreadable entries are skipped, never fatal; a partial
//	scan is preferable to a dead watcher.
package watch
