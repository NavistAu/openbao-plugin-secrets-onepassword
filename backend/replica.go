package backend

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// errNotFound is returned by title/split-path resolution when nothing
// in the index matches.
var errNotFound = errors.New("op: not found")

// errAmbiguousTitle wraps a resolveTitle failure when ≥2 items share
// a title (spec §3 Addressing).
var errAmbiguousTitle = errors.New("op: ambiguous title")

// vaultReplica is the full in-memory materialization of one
// allowlisted 1Password vault (spec §4 Structure). D4: no item data
// is ever persisted to bao storage — a restart means a cold refetch,
// which is why cold start (backend.go's runVaultCycle) is just the
// same delta cycle run against a freshly zeroed replica.
type vaultReplica struct {
	mu sync.Mutex

	vaultID string

	// overview mirrors Items.List(), Active items only (archived
	// reads as deleted, spec §4): ID -> ItemOverview. UpdatedAt here
	// is the change-detection index the delta cycle diffs against.
	overview map[string]onepassword.ItemOverview

	// titles preserves duplicates (title -> IDs) so an ambiguous
	// title read can list every candidate (spec §3 Addressing).
	titles map[string][]string

	// bodies holds the full Item for every ID in overview (D11 full
	// materialization) — the replica IS the working set; nothing is
	// fetched lazily on read.
	bodies map[string]onepassword.Item

	// splitPaths holds the path_split (D13) form of every title,
	// split-path -> ID, built bijectively: a title whose split form
	// collides with another title's is excluded from splitPaths
	// entirely (fail closed) and recorded in splitCollision. nil when
	// path_split is unset.
	splitPaths     map[string]string
	splitCollision error

	// negativeCache remembers recent misses (title/ID/field not in
	// the index) so a misconfigured consumer can't repeatedly burn
	// the request budget (spec §4). Cleared wholesale by every
	// successful cycle and by a config write.
	negativeCache map[string]time.Time

	// lastCycle stamps the freshness window origin; Task 6 reads it
	// against passthrough_ttl, this file only stamps it.
	lastCycle time.Time
	// staleSuspect / invalidatedAt are set by the (Task 7) invalidate
	// path and cleared here only by a *successful* delta cycle, per
	// spec §4 ("cleared only after the next successful delta cycle,
	// not merely the next attempt").
	staleSuspect  bool
	invalidatedAt time.Time

	// consecutiveFailures counts refresh errors in a row (status
	// surface, spec §4 Failure behaviour); reset to 0 on success.
	consecutiveFailures int

	// cycleMu + inflight implement the single-flight coalescing of
	// concurrent cycle triggers (spec §4: "concurrent triggers for
	// the same vault coalesce into one cycle"). See cycle.go.
	cycleMu  sync.Mutex
	inflight *cycleResult
}

func newVaultReplica(vaultID string) *vaultReplica {
	return &vaultReplica{
		vaultID:       vaultID,
		overview:      map[string]onepassword.ItemOverview{},
		titles:        map[string][]string{},
		bodies:        map[string]onepassword.Item{},
		negativeCache: map[string]time.Time{},
	}
}

// negativeCacheHit reports whether key was miss-cached within ttl of
// now, evicting the entry if the TTL has elapsed.
func (v *vaultReplica) negativeCacheHit(key string, ttl time.Duration, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.negativeCache[key]
	if !ok {
		return false
	}
	if now.Sub(t) >= ttl {
		delete(v.negativeCache, key)
		return false
	}
	return true
}

// negativeCacheStore records key as missed at now.
func (v *vaultReplica) negativeCacheStore(key string, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.negativeCache[key] = now
}

// clearNegativeCache empties the negative cache wholesale — called by
// every delta cycle and by a config write (spec §4).
func (v *vaultReplica) clearNegativeCache() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.negativeCache = map[string]time.Time{}
}

// resolveTitle looks up an exact-match title in the flat title index
// (spec §3 Addressing: title matching is exact and case-sensitive). A
// title shared by ≥2 items is ambiguous; the returned error lists
// every candidate ID (sorted for deterministic output) so a caller
// can present them.
func (v *vaultReplica) resolveTitle(title string) (itemID string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ids := v.titles[title]
	switch len(ids) {
	case 0:
		return "", errNotFound
	case 1:
		return ids[0], nil
	default:
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		return "", fmt.Errorf("%w: title %q matches %d items: %s", errAmbiguousTitle, title, len(sorted), strings.Join(sorted, ", "))
	}
}

// resolveItemAddress resolves addr as either a direct item ID
// (present in the overview index) or an exact-match title (spec §3
// Addressing: "the ID ... as well as the title"). IDs are checked
// first since they are always unambiguous and never require the
// ambiguity-error path.
func (v *vaultReplica) resolveItemAddress(addr string) (itemID string, err error) {
	v.mu.Lock()
	if _, ok := v.overview[addr]; ok {
		v.mu.Unlock()
		return addr, nil
	}
	v.mu.Unlock()
	return v.resolveTitle(addr)
}

// overviewTitle returns itemID's title, or "" if itemID is not in the
// current overview index.
func (v *vaultReplica) overviewTitle(itemID string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ov, ok := v.overview[itemID]; ok {
		return ov.Title
	}
	return ""
}

// overviewList returns a snapshot of every active item overview
// currently in the replica (Task 6's paths_items.go).
func (v *vaultReplica) overviewList() []onepassword.ItemOverview {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]onepassword.ItemOverview, 0, len(v.overview))
	for _, ov := range v.overview {
		out = append(out, ov)
	}
	return out
}

// body returns a copy of itemID's full Item, or false if it's not in
// the replica.
func (v *vaultReplica) body(itemID string) (onepassword.Item, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	item, ok := v.bodies[itemID]
	return item, ok
}

// overviewUpdatedAt returns itemID's UpdatedAt, or the zero Time if
// itemID is not in the current overview index.
func (v *vaultReplica) overviewUpdatedAt(itemID string) time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ov, ok := v.overview[itemID]; ok {
		return ov.UpdatedAt
	}
	return time.Time{}
}

// freshnessSnapshot returns the passthrough-window origin and the
// invalidate lifecycle fields (Task 6/7 read paths) in one
// mutex-guarded call.
func (v *vaultReplica) freshnessSnapshot() (lastCycle time.Time, staleSuspect bool, invalidatedAt time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastCycle, v.staleSuspect, v.invalidatedAt
}

// invalidate implements the D15 zero-spend invalidation (spec §4):
// expires the freshness window (by zeroing lastCycle — Go's
// time.Time.Sub clamps a from-year-1 subtraction to the maximum
// Duration, so any windowExpired check against it is trivially true,
// with no separate "invalidated" flag needed there), sets
// stale_suspect and invalidated_at, and clears the negative cache (a
// pre-existing miss must not outlive an invalidation). Cleared only
// by the next *successful* delta cycle (doCycle, cycle.go) — not
// merely the next attempt.
func (v *vaultReplica) invalidate(now time.Time) {
	v.mu.Lock()
	v.lastCycle = time.Time{}
	v.staleSuspect = true
	v.invalidatedAt = now
	v.mu.Unlock()
	v.clearNegativeCache()
}

// itemCount and consecutiveFailureCount are small status-surface
// accessors (Task 6's paths_status.go).
func (v *vaultReplica) itemCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.overview)
}

func (v *vaultReplica) consecutiveFailureCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.consecutiveFailures
}

// resolveSplitPath greedily matches path segments against the
// split-path index (D13), consuming the longest known prefix as the
// item address (spec §3 Addressing: "resolves the longest index match
// as the item, remaining segments as section/field"). The search
// order is strictly longest-to-shortest, so the first hit wins by
// construction — no separate disambiguation is needed here; a title
// whose split form collides with another's is excluded from the index
// entirely at build time (rebuildIndexesLocked), not resolved
// ambiguously here.
func (v *vaultReplica) resolveSplitPath(segs []string) (itemID string, remaining []string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for n := len(segs); n >= 1; n-- {
		if id, found := v.splitPaths[strings.Join(segs[:n], "/")]; found {
			return id, segs[n:], true
		}
	}
	return "", nil, false
}

// rebuildIndexesLocked recomputes titles and splitPaths from the
// current overview. Caller must hold v.mu.
func (v *vaultReplica) rebuildIndexesLocked(pathSplitRegex *regexp.Regexp) {
	titles := make(map[string][]string, len(v.overview))
	for id, ov := range v.overview {
		titles[ov.Title] = append(titles[ov.Title], id)
	}
	v.titles = titles

	if pathSplitRegex == nil {
		v.splitPaths = nil
		v.splitCollision = nil
		return
	}

	splitPaths := make(map[string]string, len(v.overview))
	poisoned := map[string]bool{}
	titlesAtKey := map[string][]string{}
	for id, ov := range v.overview {
		key := strings.Join(pathSplitRegex.Split(ov.Title, -1), "/")
		titlesAtKey[key] = append(titlesAtKey[key], ov.Title)
		if poisoned[key] {
			continue
		}
		if existingID, exists := splitPaths[key]; exists && existingID != id {
			poisoned[key] = true
			delete(splitPaths, key) // fail closed: neither title resolves via this path
			continue
		}
		splitPaths[key] = id
	}

	var collisions []string
	for key := range poisoned {
		collisions = append(collisions, fmt.Sprintf("%q (titles: %s)", key, strings.Join(titlesAtKey[key], ", ")))
	}
	sort.Strings(collisions) // deterministic message regardless of map iteration order

	v.splitPaths = splitPaths
	if len(collisions) > 0 {
		v.splitCollision = fmt.Errorf("op: split-path collision in vault %q: %s", v.vaultID, strings.Join(collisions, "; "))
	} else {
		v.splitCollision = nil
	}
}
