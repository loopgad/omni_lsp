package persistent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// FreshnessTuple is the §L10 semantic identity of a segment payload: a
// record is fresh only if every identity input still matches. Callers seal
// payloads with SealPayload and verify on read with VerifyPayload; the store
// layer stays content-agnostic (§L5 boundary).
type FreshnessTuple struct {
	SourceHash   string `json:"sourceHash"`
	BuildContext string `json:"buildContext"`
	Toolchain    string `json:"toolchain"`
	BackendVer   string `json:"backendVersion"`
	Revision     uint64 `json:"revision"`
}

// freshnessHeaderSz is the length prefix of the embedded JSON tuple.
const freshnessHeaderSz = 4

// SealPayload prefixes payload with its freshness tuple:
//
//	<u32 jsonLen><json tuple><payload>
//
// Sealed data flows through WriteSegment like any other bytes; the framing
// CRC covers it, so bitrot in the tuple is caught by L7 before L10 runs.
func SealPayload(payload []byte, f FreshnessTuple) []byte {
	hdr, err := json.Marshal(f)
	if err != nil {
		// FreshnessTuple is all scalar fields; marshalling cannot fail.
		panic(fmt.Sprintf("persistent: seal freshness: %v", err))
	}
	out := make([]byte, freshnessHeaderSz+len(hdr)+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(hdr)))
	copy(out[4:], hdr)
	copy(out[4+len(hdr):], payload)
	return out
}

// VerifyPayload splits a sealed payload and checks the tuple against want.
// A mismatch is staleness (§L10), reported as ErrStaleFreshness: the caller
// must treat the record as absent and recompute.
var ErrStaleFreshness = fmt.Errorf("persistent: stale freshness tuple")

func VerifyPayload(sealed []byte, want FreshnessTuple) ([]byte, error) {
	if len(sealed) < freshnessHeaderSz {
		return nil, fmt.Errorf("%w: sealed payload shorter than header", ErrStaleFreshness)
	}
	n := binary.BigEndian.Uint32(sealed[:4])
	if uint64(len(sealed)) < freshnessHeaderSz+uint64(n) {
		return nil, fmt.Errorf("%w: declared tuple %d bytes, payload holds fewer", ErrStaleFreshness, n)
	}
	var got FreshnessTuple
	if err := json.Unmarshal(sealed[4:4+n], &got); err != nil {
		return nil, fmt.Errorf("%w: corrupt tuple: %v", ErrStaleFreshness, err)
	}
	if got != want {
		return nil, fmt.Errorf("%w: stored %+v want %+v", ErrStaleFreshness, got, want)
	}
	return sealed[4+n:], nil
}
