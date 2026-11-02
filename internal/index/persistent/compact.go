package persistent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Structure-bounded compaction (registry DEF-COMPACT, goal.md §L16).
//
// The store's payload contract, as the live semantic writer (§L5 boundary,
// internal/index/semantic) produces it: every segment payload is ONE JSON
// document — a batch envelope {"format","schemaVersion","kind","records"}
// with kind symbols|occurrences|edges — and the LAST segment of a generation
// is the terminal metadata envelope (kind "metadata"). The semantic reader
// enforces exactly that shape: it reads the last segment as metadata
// (reader.go OpenReader) and its strict decoder rejects trailing JSON, so
// merging many payloads into one physical segment renders a generation
// unreadable. That is what the pre-DEF-COMPACT Compact did (mergeAppend
// framing); this file replaces it.
//
// Compaction therefore re-partitions instead of concatenating: records of a
// kind are spliced byte-for-byte into bounded re-emitted envelopes, the
// metadata payload passes through verbatim as the terminal segment, and
// generations that do not match the batch layout are rewritten verbatim —
// never reinterpreted. Bounds (parity with the live writer/reader, which this
// package cannot import across the §L5 boundary):
const (
	// compactBatchBytes bounds each re-partitioned data payload, matching the
	// live semantic writer's batch bound (semantic.MaxBatchBytes, 8 MiB).
	compactBatchBytes int64 = 8 << 20
	// compactSegmentBytes is the hard backstop any output payload must stay
	// under, matching the live reader's segment bound (semantic.MaxSegmentBytes,
	// 64 MiB). Only a single record too large for one batch bound can push a
	// payload between the two; such a record is emitted as its own segment.
	compactSegmentBytes int64 = 64 << 20
)

type compactPayloadClass uint8

const (
	compactDataPayload compactPayloadClass = iota
	compactMetadataPayload
	compactOpaquePayload
)

// classifyCompactPayload inspects one stored payload against the batch
// contract. All stored bytes are untrusted (L7): anything that is not a
// single JSON object with a string "kind" and — for data segments — a
// non-empty "records" array is opaque and may only be rewritten verbatim.
// Data envelopes that carry a "metadata" field are opaque too: the live
// writer never emits one, and re-emitting it inside a merged envelope would
// preserve a segment shape the reader rejects as non-terminal metadata.
//
// It also returns the payload's envelope with the records field elided,
// deterministically encoded (map keys are sorted), so callers can require
// every merged payload of a kind to share one envelope template — compaction
// must never drop or mix envelope fields, including unknown ones a future
// schema version adds.
func classifyCompactPayload(payload []byte) (compactPayloadClass, string, [][]byte, []byte) {
	var envelope map[string]json.RawMessage
	// json.Unmarshal accepts exactly one top-level value, so trailing data —
	// the very defect decodeStrict guards against — classifies as opaque.
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return compactOpaquePayload, "", nil, nil
	}
	kindRaw, ok := envelope["kind"]
	if !ok {
		return compactOpaquePayload, "", nil, nil
	}
	var kind string
	if err := json.Unmarshal(kindRaw, &kind); err != nil || kind == "" {
		return compactOpaquePayload, "", nil, nil
	}
	if _, hasMetadata := envelope["metadata"]; hasMetadata && kind != "metadata" {
		return compactOpaquePayload, "", nil, nil
	}
	if kind == "metadata" {
		return compactMetadataPayload, kind, nil, nil
	}
	recordsRaw, ok := envelope["records"]
	if !ok {
		return compactOpaquePayload, "", nil, nil
	}
	var decoded []json.RawMessage
	if err := json.Unmarshal(recordsRaw, &decoded); err != nil || len(decoded) == 0 {
		return compactOpaquePayload, "", nil, nil
	}
	records := make([][]byte, len(decoded))
	for i, raw := range decoded {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			return compactOpaquePayload, "", nil, nil
		}
		records[i] = trimmed
	}
	template := make(map[string]json.RawMessage, len(envelope))
	for key, value := range envelope {
		if key == "records" {
			continue
		}
		template[key] = value
	}
	return compactDataPayload, kind, records, encodeCompactJSON(template)
}

// encodeCompactJSON emits one JSON value deterministically: map keys are
// sorted and HTML escaping is off so RawMessage record bytes pass through
// untouched (the live writer already emits \u003c-style escapes via
// json.Marshal; re-escaping with it would be a no-op, but leaving it off
// keeps foreign unescaped bytes verbatim too). json.Encoder appends a
// newline; it is trimmed.
func encodeCompactJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		// Unreachable for map[string]json.RawMessage assembled from JSON the
		// decoder already accepted; treat as opaque input upstream.
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// kindMerge accumulates one kind's records plus the shared envelope template.
//
// 增量定批（复证圈 2 审计发现 9）：记录凑满一个 batchLimit 即编码为输出批并
// 释放该批记录副本——旧实现把全部 records 聚合驻留到规划结束（全部 payload
// 驻留 + 按 kind 聚合全部 records + 再产出全部输出三份同驻，峰值 ≈3× 全代
// 体积）。批边界贪心规则与原 chunkCompactRecords 完全一致，输出逐字节相同。
type kindMerge struct {
	template []byte
	records  [][]byte // 当前未满批的记录副本（定批后释放）
	batches  [][]byte // 已定稿的输出批
	used     int64    // 当前批已用字节（含 envelope overhead）
}

// add 收录一条记录：单记录超出硬段上限返回 false（整代 verbatim 回退，原
// 语义）；当前批再放不下即先定稿（编码 + 释放该批副本）。
func (m *kindMerge) add(record []byte, batchLimit, segmentLimit int64) bool {
	overhead := compactEnvelopeOverhead(m.template)
	if overhead+2+int64(len(record)) > segmentLimit {
		return false
	}
	if len(m.records) > 0 && m.used+int64(len(record))+1 > batchLimit {
		m.batches = append(m.batches, encodeCompactBatch(m.template, m.records))
		m.records = nil // 释放该批记录副本（峰值治理）
		m.used = overhead
	}
	if len(m.records) > 0 {
		m.used++ // comma
	}
	m.used += int64(len(record))
	m.records = append(m.records, record)
	return true
}

// finish 定稿当前未满批（无残留记录时为 no-op）。
func (m *kindMerge) finish() {
	if len(m.records) > 0 {
		m.batches = append(m.batches, encodeCompactBatch(m.template, m.records))
		m.records = nil
	}
}

// planCompaction maps verified source payloads to output payloads.
//
// Live-layout generations (at least one conforming data segment, exactly one
// metadata segment, and it is terminal) are re-partitioned: per kind, in
// first-appearance kind order, records are spliced verbatim into envelopes
// bounded by batchLimit; the metadata payload is appended verbatim. Every
// other generation — opaque payloads, malformed envelopes, mixed envelope
// templates, non-terminal or duplicate metadata, data segments already
// larger than batchLimit — is returned unchanged so the rewrite preserves it
// byte-for-byte (readability is never risked).
//
// The plan never increases the segment count: re-partitioning only merges,
// because every source segment is gate-checked to fit the batch bound and
// the re-encoded envelope of a conforming payload never exceeds the source
// envelope it replaces (compaction only shrinks insignificant bytes).
func planCompaction(payloads [][]byte, batchLimit, segmentLimit int64) [][]byte {
	if batchLimit <= 0 || batchLimit > segmentLimit {
		batchLimit = segmentLimit
	}
	byKind := make(map[string]*kindMerge)
	var order []string
	var metaPayload []byte
	for i, payload := range payloads {
		last := i == len(payloads)-1
		class, kind, records, template := classifyCompactPayload(payload)
		switch class {
		case compactMetadataPayload:
			if !last || metaPayload != nil {
				return payloads
			}
			metaPayload = payload
		case compactDataPayload:
			if last {
				return payloads // data at the terminal position is not the live layout
			}
			if int64(len(payload)) > batchLimit {
				// A data segment already larger than the bound cannot be
				// re-partitioned without splitting below source granularity,
				// which would grow the segment count; rewrite verbatim.
				return payloads
			}
			merged := byKind[kind]
			if merged == nil {
				// 新批从 envelope overhead 起算（与原 chunkCompactRecords 的
				// 每批 used := overhead 同一口径）——漏算会让首个批次少记
				// envelope 字节、超限合并。
				merged = &kindMerge{template: template, used: compactEnvelopeOverhead(template)}
				byKind[kind] = merged
				order = append(order, kind)
			} else if string(merged.template) != string(template) {
				return payloads // differing envelopes must not be silently unified
			}
			for _, record := range records {
				if !merged.add(record, batchLimit, segmentLimit) {
					return payloads // one record cannot fit under the hard segment cap
				}
			}
		default:
			return payloads
		}
	}
	if metaPayload == nil || len(order) == 0 {
		return payloads
	}
	var plan [][]byte
	for _, kind := range order {
		merged := byKind[kind]
		merged.finish()
		plan = append(plan, merged.batches...)
	}
	return append(plan, metaPayload)
}

// compactEnvelopeOverhead is the encoded size of one of this kind's envelopes
// with the records bytes elided (the "[]" placeholder marks the two brackets
// the actual records array adds around the spliced bytes).
func compactEnvelopeOverhead(template []byte) int64 {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(template, &envelope); err != nil {
		return 0 // unreachable: classifyCompactPayload built this template
	}
	envelope["records"] = json.RawMessage("[]")
	return int64(len(encodeCompactJSON(envelope))) - 2
}

// encodeCompactBatch re-emits one batch envelope: the source template's
// fields verbatim (unknown fields included) plus the records array spliced
// from the source record bytes.
func encodeCompactBatch(template []byte, records [][]byte) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(template, &envelope); err != nil {
		return nil // unreachable: classifyCompactPayload built this template
	}
	merged := make([]byte, 0, 2)
	merged = append(merged, '[')
	for i, record := range records {
		if i > 0 {
			merged = append(merged, ',')
		}
		merged = append(merged, record...)
	}
	merged = append(merged, ']')
	envelope["records"] = json.RawMessage(merged)
	return encodeCompactJSON(envelope)
}

// Compact re-partitions a pinned snapshot into a fresh generation under the
// structural bound above. It holds the cross-process writer lock only while
// opening the snapshot, reserving the new generation, publishing it, and
// pruning history; per-output segment writes check the caller's context, so
// compaction stays cancelable between safe checkpoints (§L16).
//
// Generations that do not match the live batch layout are rewritten verbatim
// in their original order: such a rewrite still re-verifies every byte
// through the checksummed frame, retires the pinned generation once its
// leases close, and — unlike the pre-DEF-COMPACT merger — never changes what
// a reader sees. Fragmentation for those stores stays bounded by
// KeepGenerations retirement and the disk budget, as before DEF-COMPACT.
//
// Transaction semantics are those of any build (TXN-002): outputs are staged
// through BeginBuild, published by Commit's atomic manifest swap, and rolled
// back completely on any failure; the compacted generation is pruned from the
// recovery history only after publication, and generation leases keep its
// segments alive for pinned readers.
func (s *FileStore) Compact(ctx context.Context) error {
	writerLock, err := acquireWriterLock(ctx, s.root)
	if err != nil {
		return err
	}
	defer writerLock.release()
	s.mu.Lock()
	err = s.recover()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.cleanupOrphanSegments()
	view, err := s.openSnapshotWithWriterLock(ctx)
	if err != nil {
		return err
	}
	leaseLock, err := acquireGenerationLock(ctx, s.root, view.ID, false, true)
	if err != nil {
		return err
	}
	lease := &GenerationLease{view: view, lock: leaseLock, store: s}
	build, err := s.beginBuildWithWriterLock(ctx, writerLock)
	if err != nil {
		_ = lease.Close()
		return err
	}
	defer lease.Close()
	view = lease.Snapshot()
	// Differential budget billing (§L17, audit batch 1): this build rewrites
	// and retires exactly the pinned generation — its segment files are freed
	// by pruneHistoryGeneration once the commit lands. Crediting those bytes
	// against the §L17 walk bills the eventual footprint (output ≤ input:
	// re-partitioning only merges and compacts, verbatim rewrites are
	// byte-identical), so a store near capacity can compact without holding
	// ≈2× its payload under budget. Normal (append) builds keep the zero
	// credit and the original check verbatim.
	var budgetCreditBytes int64
	for _, seg := range view.Segments {
		budgetCreditBytes += seg.Len
	}
	fb, ok := build.(*fileBuild)
	if !ok {
		_ = build.Abort()
		return fmt.Errorf("persistent: compact build session is %T, want *fileBuild", build)
	}
	fb.budgetCreditBytes = budgetCreditBytes
	payloads := make([][]byte, 0, len(view.Segments))
	for _, seg := range view.Segments {
		payload, err := view.ReadSegment(seg.ID)
		if err != nil {
			_ = build.Abort()
			return err
		}
		payloads = append(payloads, payload)
	}
	// 规划完成后释放输入段引用（复证圈 2 审计发现 9）：plan 的输出是新编码
	// 字节（仅终端 metadata 段与 verbatim 回退路径个别别名），写盘阶段不再
	// 与全部输入同驻——三份同驻的峰值（输入+记录副本+输出）降为 ≈输出。
	plan := planCompaction(payloads, compactBatchBytes, compactSegmentBytes)
	payloads = nil
	for _, payload := range plan {
		if _, err := build.WriteSegment(payload); err != nil {
			_ = build.Abort()
			return err
		}
	}
	if err := build.Commit(ctx); err != nil {
		_ = build.Abort()
		return err
	}
	_ = lease.Close()
	// The previous generation is retained in history for fallback. Retire it
	// from that chain durably before deleting its segments; older fallbacks stay.
	return s.pruneHistoryGeneration(ctx, view.ID)
}
