// Package replay records and replays LSP protocol sessions (goal.md §P9/P10).
//
// A session is a JSONL file: line 1 is a Meta record carrying the P9
// environment digests (config hash, backend versions, toolchain identity);
// every following line is one protocol message with its direction ("in" =
// client→server, "out" = server→client) and the snapshot revision observed
// at capture time. Replay feeds "in" entries in logical sequence to a fresh
// server and compares the resulting "out" stream against the recording —
// wall-clock timing never participates.
package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// FormatVersion guards the on-disk shape of replay files.
const FormatVersion = 1

// Meta is the P9 header record: everything needed to reconstruct context.
type Meta struct {
	FormatVersion int               `json:"formatVersion"`
	ConfigHash    string            `json:"configHash"`
	Backends      map[string]string `json:"backends,omitempty"`  // langID → engine version
	Toolchain     map[string]string `json:"toolchain,omitempty"` // tool → version
	WorkspaceDir  string            `json:"workspaceDir,omitempty"`
}

// Entry is one recorded protocol message.
type Entry struct {
	Seq     int             `json:"seq"`
	Dir     string          `json:"dir"` // in | out | meta
	SnapRev uint64          `json:"snapRev,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// Session is a loaded replay file.
type Session struct {
	Meta    Meta
	Entries []Entry // excludes the meta entry
}

// ConfigHash derives the P9 config digest from any deterministic value.
func ConfigHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Save writes the session as JSONL (meta first, then entries in order).
func Save(path string, s *Session) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("replay: create %q: %w", path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	metaEntry := Entry{Seq: 0, Dir: "meta", Payload: mustJSON(s.Meta)}
	if err := enc.Encode(metaEntry); err != nil {
		return err
	}
	for _, e := range s.Entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return w.Flush()
}

// LoadSession parses a JSONL replay file.
func LoadSession(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open %q: %w", path, err)
	}
	defer f.Close()

	s := &Session{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // large didChange payloads
	line := 0
	for sc.Scan() {
		line++
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("replay: %s:%d: %w", path, line, err)
		}
		switch e.Dir {
		case "meta":
			if err := json.Unmarshal(e.Payload, &s.Meta); err != nil {
				return nil, fmt.Errorf("replay: %s:%d meta: %w", path, line, err)
			}
			if s.Meta.FormatVersion != FormatVersion {
				return nil, fmt.Errorf("replay: format v%d unsupported (want v%d)",
					s.Meta.FormatVersion, FormatVersion)
			}
		case "in", "out":
			s.Entries = append(s.Entries, e)
		default:
			return nil, fmt.Errorf("replay: %s:%d unknown dir %q", path, line, e.Dir)
		}
	}
	return s, sc.Err()
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
