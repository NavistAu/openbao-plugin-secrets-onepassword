package backend

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// errGateDenied is returned when requestGate.allow() refuses a cycle;
// no 1P request is issued in that case.
var errGateDenied = errors.New("op: request gate denied cycle")

// errNoClient is returned by runVaultCycle/refreshVaultDirectory when
// no config has ever been written (ensureClient, backend.go). Distinct
// from errClientUnavailable, which covers a config that IS loaded but
// whose client hasn't been successfully constructed yet (spec §4
// Restart: client construction is a live network call and can fail —
// or still be backing off — independently of config load).
var errNoClient = errors.New("op: engine not configured")

// workClass distinguishes why a cycle is being triggered, for the
// governor's burst brake (spec §4): periodic/passthrough-eager work
// is deferrable, miss-triggered and manual-refresh work is not.
type workClass int

const (
	// workClassPeriodic covers both PeriodicFunc-driven cycles and
	// passthrough window-expiry "eager refetch" cycles — the spec
	// names both as the deferrable class the burst brake pauses
	// first ("periodic refresh cycles and eager refetches pause").
	workClassPeriodic workClass = iota
	// workClassMiss is a cycle triggered by an unknown item/vault
	// name, needed to discover it — allowed past the burst brake
	// (up to the hard cap) since the alternative is failing a read
	// outright for something that may simply be new.
	workClassMiss
	// workClassManual is an operator- or deploy-triggered op/refresh
	// call — also allowed past the burst brake (spec §4: "manual
	// refreshes still serve up to the cap").
	workClassManual
)

// requestGate is the seam the delta cycle consults before spending a
// 1P request, and reports back what happened so the governor (Task 5,
// governor.go) can track rolling usage and classify failures into
// rate_limited/auth_failed/backoff state. allowAllGate is the
// zero-value stand-in a bare requestGate value would need before the
// governor exists; the real Backend always uses *governor (see
// backend.go), which satisfies this interface.
type requestGate interface {
	// allow reports whether a cycle of class may proceed for vaultID
	// right now.
	allow(vaultID string, class workClass) bool
	// recordRequest counts units of consumed request budget (0 if
	// none were actually spent, e.g. an empty GetItems batch) and, if
	// err is non-nil, classifies it into a state transition.
	recordRequest(vaultID string, units int, err error)
	// recordSuccess clears vaultID's backoff state after a fully
	// successful cycle.
	recordSuccess(vaultID string)
}

// allowAllGate is a trivial requestGate used by tests that don't care
// about gating.
type allowAllGate struct{}

func (allowAllGate) allow(vaultID string, class workClass) bool         { return true }
func (allowAllGate) recordRequest(vaultID string, units int, err error) {}
func (allowAllGate) recordSuccess(vaultID string)                       {}

// cycleResult is the shared outcome of a single-flighted cycle: every
// caller that coalesces onto one in-flight run waits on wg and then
// reads err, which is only written by the leader before calling
// wg.Done() (so the read after Wait is race-free).
type cycleResult struct {
	wg  sync.WaitGroup
	err error
}

// runCycle executes the spec §4 delta cycle for v, coalescing
// concurrent callers into a single underlying fetch: the first caller
// becomes the leader and runs doCycle; anyone arriving while a cycle
// is in flight waits for and shares the leader's result instead of
// starting a second one (spec §4: "concurrent triggers for the same
// vault coalesce into one cycle").
func runCycle(ctx context.Context, v *vaultReplica, client OPClient, gate requestGate, pathSplitRegex *regexp.Regexp, class workClass) error {
	v.cycleMu.Lock()
	if v.inflight != nil {
		r := v.inflight
		v.cycleMu.Unlock()
		r.wg.Wait()
		return r.err
	}
	r := &cycleResult{}
	r.wg.Add(1)
	v.inflight = r
	v.cycleMu.Unlock()

	r.err = doCycle(ctx, v, client, gate, pathSplitRegex, class)

	v.cycleMu.Lock()
	v.inflight = nil
	v.cycleMu.Unlock()
	r.wg.Done()

	return r.err
}

// doCycle is the D5 delta cycle: List -> diff UpdatedAt against the
// index -> chunked GetItems for changed+new items -> apply. A whole
// -call error (List or GetItems) leaves the replica untouched and
// only bumps consecutiveFailures (spec §4 Failure behaviour); a
// per-item GetItems error just skips that item, leaving its previous
// overview/body (if any) in place — its UpdatedAt stays what it was,
// so the next cycle's diff naturally retries it without separate
// retry bookkeeping.
func doCycle(ctx context.Context, v *vaultReplica, client OPClient, gate requestGate, pathSplitRegex *regexp.Regexp, class workClass) error {
	if !gate.allow(v.vaultID, class) {
		return errGateDenied
	}

	overviews, err := client.ListItems(ctx, v.vaultID)
	gate.recordRequest(v.vaultID, 1, err)
	if err != nil {
		v.mu.Lock()
		v.consecutiveFailures++
		v.mu.Unlock()
		return fmt.Errorf("op: list items for vault %q: %w", v.vaultID, err)
	}

	// Archived items read as deleted (spec §4): only Active overviews
	// count toward the index. Filtering here on ItemOverview.State —
	// rather than an SDK list filter — matches the frozen OPClient
	// seam (Task 2), which takes no filter argument.
	active := make(map[string]onepassword.ItemOverview, len(overviews))
	for _, ov := range overviews {
		if ov.State == onepassword.ItemStateActive {
			active[ov.ID] = ov
		}
	}

	v.mu.Lock()
	var toFetch, toRemove []string
	for id, ov := range active {
		old, existed := v.overview[id]
		if !existed || !old.UpdatedAt.Equal(ov.UpdatedAt) {
			toFetch = append(toFetch, id)
		}
	}
	for id := range v.overview {
		if _, ok := active[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	v.mu.Unlock()

	// GetItems chunks at ≤50 IDs/call internally (Task 2 seam) and
	// costs zero request-budget units when toFetch is empty — the
	// unchanged-vault steady state is exactly "1 list, 0 gets". The
	// governor is told the same chunk count as the request-budget
	// unit (spec §4 conservative accounting: 1 read per ≤50-ID call).
	results, err := client.GetItems(ctx, v.vaultID, toFetch)
	gate.recordRequest(v.vaultID, len(chunkIDs(toFetch, maxGetAllIDs)), err)
	if err != nil {
		v.mu.Lock()
		v.consecutiveFailures++
		v.mu.Unlock()
		return fmt.Errorf("op: get items for vault %q: %w", v.vaultID, err)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	for _, id := range toRemove {
		delete(v.overview, id)
		delete(v.bodies, id)
	}
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		v.overview[r.ID] = active[r.ID]
		v.bodies[r.ID] = *r.Item
	}

	v.rebuildIndexesLocked(pathSplitRegex)
	v.negativeCache = map[string]time.Time{}
	v.lastCycle = time.Now()
	v.staleSuspect = false
	v.invalidatedAt = time.Time{}
	v.consecutiveFailures = 0
	gate.recordSuccess(v.vaultID)
	return nil
}
