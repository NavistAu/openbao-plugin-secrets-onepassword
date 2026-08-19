package backend

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

func TestVaultReplica_NegativeCache(t *testing.T) {
	v := newVaultReplica("v1")
	t0 := time.Unix(0, 0)

	if v.negativeCacheHit("missing", 30*time.Second, t0) {
		t.Fatalf("negativeCacheHit before any store = true, want false")
	}

	v.negativeCacheStore("missing", t0)
	if !v.negativeCacheHit("missing", 30*time.Second, t0.Add(10*time.Second)) {
		t.Errorf("negativeCacheHit within TTL = false, want true")
	}
	if v.negativeCacheHit("missing", 30*time.Second, t0.Add(31*time.Second)) {
		t.Errorf("negativeCacheHit past TTL = true, want false (and the entry should be evicted)")
	}
	// The expired entry was evicted: even a fresh check "as of t0" (which
	// would otherwise still be within TTL) now misses.
	if v.negativeCacheHit("missing", 30*time.Second, t0.Add(5*time.Second)) {
		t.Errorf("negativeCacheHit after expiry should stay a miss (entry evicted), got true")
	}

	v.negativeCacheStore("other", t0)
	v.clearNegativeCache()
	if v.negativeCacheHit("other", 30*time.Second, t0) {
		t.Errorf("negativeCacheHit after clearNegativeCache = true, want false")
	}
}

func TestVaultReplica_ResolveTitle(t *testing.T) {
	v := newVaultReplica("v1")
	v.mu.Lock()
	v.overview = map[string]onepassword.ItemOverview{
		"i1": {ID: "i1", Title: "postgres", State: onepassword.ItemStateActive},
		"i2": {ID: "i2", Title: "shared", State: onepassword.ItemStateActive},
		"i3": {ID: "i3", Title: "shared", State: onepassword.ItemStateActive},
	}
	v.rebuildIndexesLocked(nil)
	v.mu.Unlock()

	id, err := v.resolveTitle("postgres")
	if err != nil || id != "i1" {
		t.Fatalf("resolveTitle(postgres) = %q, %v, want i1, nil", id, err)
	}

	if _, err := v.resolveTitle("nope"); !errors.Is(err, errNotFound) {
		t.Errorf("resolveTitle(nope) err = %v, want errNotFound", err)
	}

	_, err = v.resolveTitle("shared")
	if !errors.Is(err, errAmbiguousTitle) {
		t.Fatalf("resolveTitle(shared) err = %v, want errAmbiguousTitle", err)
	}
	if got := err.Error(); !strings.Contains(got, "i2") || !strings.Contains(got, "i3") {
		t.Errorf("resolveTitle(shared) error %q does not list both candidate IDs", got)
	}
}

func TestVaultReplica_ResolveSplitPath_Greedy(t *testing.T) {
	v := newVaultReplica("v1")
	v.mu.Lock()
	v.overview = map[string]onepassword.ItemOverview{
		"i1": {ID: "i1", Title: "db.example.com__postgres", State: onepassword.ItemStateActive},
	}
	re := regexp.MustCompile("__")
	v.rebuildIndexesLocked(re)
	v.mu.Unlock()

	// 3-segment address: 2-segment item address + 1 field segment.
	id, remaining, ok := v.resolveSplitPath([]string{"db.example.com", "postgres", "credential"})
	if !ok || id != "i1" {
		t.Fatalf("resolveSplitPath(3-seg) = %q, %v, %v, want i1, [credential], true", id, remaining, ok)
	}
	if len(remaining) != 1 || remaining[0] != "credential" {
		t.Errorf("resolveSplitPath(3-seg) remaining = %v, want [credential]", remaining)
	}

	// 4-segment field form: 2-segment item address + section + field.
	id, remaining, ok = v.resolveSplitPath([]string{"db.example.com", "postgres", "notes", "credential"})
	if !ok || id != "i1" {
		t.Fatalf("resolveSplitPath(4-seg) = %q, %v, %v, want i1, [notes credential], true", id, remaining, ok)
	}
	if len(remaining) != 2 || remaining[0] != "notes" || remaining[1] != "credential" {
		t.Errorf("resolveSplitPath(4-seg) remaining = %v, want [notes credential]", remaining)
	}

	// No match at all.
	if _, _, ok := v.resolveSplitPath([]string{"nowhere"}); ok {
		t.Errorf("resolveSplitPath(nowhere) ok = true, want false")
	}
}

func TestVaultReplica_SplitPath_Collision(t *testing.T) {
	v := newVaultReplica("v1")
	v.mu.Lock()
	// "a__b" and "a__b" both split to "a/b" via a delimiter regex that
	// also matches a literal "/" separator in one of the titles —
	// simplest reproducible collision: two distinct titles that split
	// to the identical segment sequence.
	v.overview = map[string]onepassword.ItemOverview{
		"i1": {ID: "i1", Title: "a__b", State: onepassword.ItemStateActive},
		"i2": {ID: "i2", Title: "a___b", State: onepassword.ItemStateActive}, // "_" is itself matched by "_+", also splits to [a b]
	}
	re := regexp.MustCompile("_+")
	v.rebuildIndexesLocked(re)
	v.mu.Unlock()

	if v.splitCollision == nil {
		t.Fatalf("splitCollision = nil, want a collision error for colliding titles a__b / a___b")
	}
	if _, _, ok := v.resolveSplitPath([]string{"a", "b"}); ok {
		t.Errorf("resolveSplitPath(a/b) should not resolve after a collision (fail closed), got ok=true")
	}
}
