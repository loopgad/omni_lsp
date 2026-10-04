package semantic

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/omnilsp/omni/internal/index/model"
)

const (
	idDeclared   byte = 1
	idReferenced byte = 2

	ledgerBufferBytes = 2 << 20
	mergeFanIn        = 32
)

type idRecord struct {
	ID      string
	ScopeID string
	Kind    byte
	Fact    model.FactKind
}

type idLedger struct {
	dir       string
	path      string
	file      *os.File
	writer    *bufio.Writer
	runs      []string
	closed    bool
	validated bool
}

func newIDLedger() (*idLedger, error) {
	dir, err := os.MkdirTemp("", "omnilsp-semantic-ids-")
	if err != nil {
		return nil, fmt.Errorf("semantic: create ID ledger: %w", err)
	}
	path := filepath.Join(dir, "ids.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("semantic: create ID ledger: %w", err)
	}
	return &idLedger{dir: dir, path: path, file: file, writer: bufio.NewWriterSize(file, 64<<10)}, nil
}

func (l *idLedger) Add(scopeID, id string, kind byte, fact model.FactKind) error {
	if l == nil || l.closed || l.validated {
		return errors.New("semantic: ID ledger is closed")
	}
	if id == "" || scopeID == "" || len(id) > MaxRecordBytes || len(scopeID) > MaxRecordBytes || len(fact) > 64 ||
		(kind != idDeclared && kind != idReferenced) {
		return fmt.Errorf("%w: invalid symbol ID ledger record", ErrMalformedPayload)
	}
	if err := binary.Write(l.writer, binary.BigEndian, uint32(len(id))); err != nil {
		return err
	}
	if _, err := l.writer.WriteString(id); err != nil {
		return err
	}
	if err := binary.Write(l.writer, binary.BigEndian, uint32(len(scopeID))); err != nil {
		return err
	}
	if _, err := l.writer.WriteString(scopeID); err != nil {
		return err
	}
	if err := l.writer.WriteByte(kind); err != nil {
		return err
	}
	if err := l.writer.WriteByte(byte(len(fact))); err != nil {
		return err
	}
	_, err := l.writer.WriteString(string(fact))
	return err
}

// Validate externally sorts ID records with bounded memory and open file
// counts. A referenced ID may resolve in any source scope in this generation.
func (l *idLedger) Validate(ctx context.Context, coverage []model.Coverage) error {
	if l == nil || l.closed {
		return errors.New("semantic: ID ledger is closed")
	}
	if l.validated {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.writer.Flush(); err != nil {
		return err
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	l.writer = nil
	if err := l.makeRuns(ctx); err != nil {
		return err
	}
	for len(l.runs) > 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		runs, err := l.mergePass(ctx, l.runs)
		if err != nil {
			return err
		}
		l.runs = runs
	}
	if len(l.runs) == 1 {
		if err := l.checkRun(ctx, l.runs[0], coverage); err != nil {
			return err
		}
	}
	l.validated = true
	return nil
}

func (l *idLedger) makeRuns(ctx context.Context) error {
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	items := make([]idRecord, 0, 4096)
	used := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := readIDRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("semantic: read ID ledger: %w", err)
		}
		itemBytes := len(item.ID) + len(item.ScopeID) + len(item.Fact) + 10
		if len(items) != 0 && used+itemBytes > ledgerBufferBytes {
			if err := l.writeRun(ctx, items); err != nil {
				return err
			}
			items = items[:0]
			used = 0
		}
		items = append(items, item)
		used += itemBytes
	}
	if len(items) != 0 {
		if err := l.writeRun(ctx, items); err != nil {
			return err
		}
	}
	return nil
}

func (l *idLedger) writeRun(ctx context.Context, items []idRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return lessIDRecord(items[i], items[j]) })
	f, err := os.CreateTemp(l.dir, "run-*.bin")
	if err != nil {
		return err
	}
	path := f.Name()
	bw := bufio.NewWriterSize(f, 64<<10)
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		if err := writeIDRecord(bw, item); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	l.runs = append(l.runs, path)
	// Bound run-path and descriptor growth during very large generations.
	if len(l.runs) >= mergeFanIn*2 {
		runs, err := l.mergePass(ctx, l.runs)
		if err != nil {
			return err
		}
		l.runs = runs
	}
	return nil
}

func (l *idLedger) mergePass(ctx context.Context, inputs []string) ([]string, error) {
	outputs := make([]string, 0, (len(inputs)+mergeFanIn-1)/mergeFanIn)
	for start := 0; start < len(inputs); start += mergeFanIn {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + mergeFanIn
		if end > len(inputs) {
			end = len(inputs)
		}
		group := inputs[start:end]
		if len(group) == 1 {
			outputs = append(outputs, group[0])
			continue
		}
		f, err := os.CreateTemp(l.dir, "merge-*.bin")
		if err != nil {
			return nil, err
		}
		path := f.Name()
		if err := mergeIDRuns(ctx, group, f); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		outputs = append(outputs, path)
		for _, old := range group {
			if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	return outputs, nil
}

func (l *idLedger) checkRun(ctx context.Context, path string, coverage []model.Coverage) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var current string
	var declarationScope string
	var hasDeclaration bool
	var referenceScope string
	var hasReference bool
	var completeFact model.FactKind
	coverageByScope := make(map[string]map[model.FactKind]model.Completeness)
	for _, item := range coverage {
		states := coverageByScope[item.ScopeID]
		if states == nil {
			states = make(map[model.FactKind]model.Completeness)
			coverageByScope[item.ScopeID] = states
		}
		states[item.Fact] = item.State
	}
	flush := func() error {
		if current != "" && hasReference && !hasDeclaration && completeFact != "" {
			return fmt.Errorf("%w: symbol %q referenced from scope %q with complete %s coverage", ErrUnknownSymbol, shortID(current), referenceScope, completeFact)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := readIDRecord(r)
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			return fmt.Errorf("semantic: read sorted ID run: %w", err)
		}
		if item.ID != current {
			if err := flush(); err != nil {
				return err
			}
			current = item.ID
			declarationScope = ""
			hasDeclaration = false
			referenceScope = ""
			hasReference = false
			completeFact = ""
		}
		switch item.Kind {
		case idDeclared:
			if hasDeclaration && declarationScope != item.ScopeID {
				return fmt.Errorf("%w: symbol ID %q is declared in multiple scopes", ErrDuplicateSymbol, shortID(current))
			}
			declarationScope = item.ScopeID
			hasDeclaration = true
		case idReferenced:
			if !hasReference {
				referenceScope = item.ScopeID
			}
			hasReference = true
			if coverageByScope[item.ScopeID][item.Fact] == model.Complete {
				completeFact = item.Fact
			}
		default:
			return fmt.Errorf("%w: invalid ID record kind", ErrMalformedPayload)
		}
	}
}

func (l *idLedger) Close() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	var result error
	if l.writer != nil {
		if err := l.writer.Flush(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if err := os.RemoveAll(l.dir); err != nil {
		result = errors.Join(result, err)
	}
	return result
}

func mergeIDRuns(ctx context.Context, paths []string, dst *os.File) error {
	cursors := make([]*idRunCursor, 0, len(paths))
	defer func() {
		for _, cursor := range cursors {
			_ = cursor.file.Close()
		}
	}()
	queue := make(idRunHeap, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		cursor := &idRunCursor{file: f, reader: bufio.NewReaderSize(f, 32<<10)}
		cursors = append(cursors, cursor)
		item, err := readIDRecord(cursor.reader)
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil {
			return err
		}
		cursor.item = item
		queue = append(queue, cursor)
	}
	heap.Init(&queue)
	bw := bufio.NewWriterSize(dst, 64<<10)
	for queue.Len() != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor := heap.Pop(&queue).(*idRunCursor)
		if err := writeIDRecord(bw, cursor.item); err != nil {
			return err
		}
		item, err := readIDRecord(cursor.reader)
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil {
			return err
		}
		cursor.item = item
		heap.Push(&queue, cursor)
	}
	return bw.Flush()
}

type idRunCursor struct {
	file   *os.File
	reader *bufio.Reader
	item   idRecord
}

type idRunHeap []*idRunCursor

func (h idRunHeap) Len() int           { return len(h) }
func (h idRunHeap) Less(i, j int) bool { return lessIDRecord(h[i].item, h[j].item) }
func (h idRunHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *idRunHeap) Push(value any)    { *h = append(*h, value.(*idRunCursor)) }
func (h *idRunHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return value
}

func lessIDRecord(a, b idRecord) bool {
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	if a.ScopeID != b.ScopeID {
		return a.ScopeID < b.ScopeID
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind // declarations sort before references
	}
	return a.Fact < b.Fact
}

func writeIDRecord(w io.Writer, item idRecord) error {
	if err := binary.Write(w, binary.BigEndian, uint32(len(item.ID))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, item.ID); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(item.ScopeID))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, item.ScopeID); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, item.Kind); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint8(len(item.Fact))); err != nil {
		return err
	}
	_, err := io.WriteString(w, string(item.Fact))
	return err
}

func readIDRecord(r io.Reader) (idRecord, error) {
	var idLen uint32
	if err := binary.Read(r, binary.BigEndian, &idLen); err != nil {
		if errors.Is(err, io.EOF) {
			return idRecord{}, io.EOF
		}
		return idRecord{}, err
	}
	if idLen == 0 || idLen > MaxRecordBytes {
		return idRecord{}, errors.New("invalid symbol ID length")
	}
	id := make([]byte, idLen)
	if _, err := io.ReadFull(r, id); err != nil {
		return idRecord{}, err
	}
	var scopeLen uint32
	if err := binary.Read(r, binary.BigEndian, &scopeLen); err != nil {
		return idRecord{}, err
	}
	if scopeLen == 0 || scopeLen > MaxRecordBytes {
		return idRecord{}, errors.New("invalid scope ID length")
	}
	scope := make([]byte, scopeLen)
	if _, err := io.ReadFull(r, scope); err != nil {
		return idRecord{}, err
	}
	var kind byte
	if err := binary.Read(r, binary.BigEndian, &kind); err != nil {
		return idRecord{}, err
	}
	var factLen uint8
	if err := binary.Read(r, binary.BigEndian, &factLen); err != nil {
		return idRecord{}, err
	}
	if factLen > 64 {
		return idRecord{}, errors.New("invalid coverage fact length")
	}
	fact := make([]byte, factLen)
	if _, err := io.ReadFull(r, fact); err != nil {
		return idRecord{}, err
	}
	return idRecord{ID: string(id), ScopeID: string(scope), Kind: kind, Fact: model.FactKind(fact)}, nil
}

func shortID(value string) string {
	if len(value) > 80 {
		return value[:77] + "..."
	}
	return value
}
