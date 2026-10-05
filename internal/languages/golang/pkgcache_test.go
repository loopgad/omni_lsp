package golang

import (
	"testing"

	"github.com/omnilsp/omni/internal/identity"
)

// TestK0_PackageCacheEvictsFIFOAtLimit pins the §K0 bound: caches MUST be
// bounded, and this one is a FIFO of pkgCacheLimit keys. Nothing exercised
// the eviction — cache_test.go only proved that a single miss repopulates
// after rotation, so an off-by-one that let the cache grow past 64 (a real
// leak on any repo touching more than 64 packages) would have gone unnoticed.
func TestK0_PackageCacheEvictsFIFOAtLimit(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()

	key := func(i int) string { return "key" + string(rune('a'+i%26)) + string(rune('a'+i/26)) }
	for i := 0; i < pkgCacheLimit; i++ {
		b.cachePackage(key(i), nil, identity.ContentHash(""))
	}
	if len(b.pkgOrder) != pkgCacheLimit || len(b.pkgCache) != pkgCacheLimit {
		t.Fatalf("after %d inserts: order %d, cache %d; want %d each",
			pkgCacheLimit, len(b.pkgOrder), len(b.pkgCache), pkgCacheLimit)
	}

	oldest := key(0)
	newest := key(pkgCacheLimit)
	b.cachePackage(newest, nil, identity.ContentHash(""))

	if len(b.pkgOrder) != pkgCacheLimit {
		t.Errorf("order grew to %d past the %d limit", len(b.pkgOrder), pkgCacheLimit)
	}
	if len(b.pkgCache) != pkgCacheLimit {
		t.Errorf("cache grew to %d entries past the %d limit", len(b.pkgCache), pkgCacheLimit)
	}
	if _, ok := b.pkgCache[oldest]; ok {
		t.Errorf("oldest key %q survived; eviction is not FIFO", oldest)
	}
	if _, ok := b.pkgCache[newest]; !ok {
		t.Errorf("newest key %q missing after insert", newest)
	}
	// The entry that was second-oldest becomes the new tail of the order.
	if b.pkgOrder[0] != key(1) {
		t.Errorf("order head = %q, want %q (the previous second-oldest)", b.pkgOrder[0], key(1))
	}
	if b.pkgOrder[len(b.pkgOrder)-1] != newest {
		t.Errorf("order tail = %q, want the newly inserted %q", b.pkgOrder[len(b.pkgOrder)-1], newest)
	}
}

// TestK0_RemovePackageCacheOrderKeyDropsAllDuplicates guards the in-place
// filter against leaving a stale occurrence behind: the order slice and the
// cache map must agree on which keys exist.
func TestK0_RemovePackageCacheOrderKeyDropsAllDuplicates(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	for _, k := range []string{"a", "b", "a", "c", "a"} {
		b.cachePackage(k, nil, identity.ContentHash(""))
	}
	b.removePackageCacheOrderKey("a")
	for _, k := range b.pkgOrder {
		if k == "a" {
			t.Fatalf("order still holds a removed key: %v", b.pkgOrder)
		}
	}
	if len(b.pkgOrder) != 2 || b.pkgOrder[0] != "b" || b.pkgOrder[1] != "c" {
		t.Errorf("order after removal = %v, want [b c]", b.pkgOrder)
	}
}
