// Package backend implements the "op" OpenBao secrets engine, which
// serves 1Password items through the 1Password service-account Go
// SDK. See docs/specs/2026-08-05-openbao-1p-secrets-engine-design.md
// in the private infrastructure repo for the full engine design.
package backend

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const backendHelp = `
The op secrets engine reads 1Password vaults through the 1Password
service-account Go SDK and serves items and fields as OpenBao-audited,
ACL-gated reads. It holds no durable copy of 1Password item data:
values are cached in memory only, refreshed on a delta cycle.
`

// clientVersion is sent to 1Password as this integration's version
// (onepassword.WithIntegrationInfo). A placeholder until Task 9 (CI
// release) stamps a real build version in here.
const clientVersion = "dev"

// sdkVersion is the pinned github.com/1password/onepassword-sdk-go
// version (go.mod) — surfaced on op/status (spec §3).
const sdkVersion = "0.4.1"

// Backend is the op secrets engine.
type Backend struct {
	*framework.Backend

	mu sync.RWMutex

	// config is the live, validated configuration — the in-memory
	// counterpart of the `op/config` storage entry, refreshed on every
	// successful write (config.go's applyConfig).
	config *Config
	// client is the current OPClient, swapped wholesale on every
	// config write (token rotation = client re-init, spec §3).
	client OPClient
	// clientFactory builds an OPClient from a service-account token.
	// Overridable so tests can substitute a fake and observe
	// re-initialization on config write.
	clientFactory func(ctx context.Context, token string) (OPClient, error)
	// pathSplitRegex is the compiled form of config.PathSplit (D13),
	// recompiled and stored alongside the rest of the derived runtime
	// state on every successful config write.
	pathSplitRegex *regexp.Regexp

	// replicas holds one vaultReplica per vault ID touched so far,
	// created lazily by runVaultCycle (cold start is just the same
	// cycle run against an empty replica, spec §4 Restart).
	replicas map[string]*vaultReplica

	// gate is the rate governor (governor.go, Task 5): consulted
	// before every delta cycle spends a 1P request, and tracks the
	// rolling usage counters, ceiling, burst brake, and 429/
	// auth_failed/backoff state. Concrete (not just requestGate) so
	// callers outside cycle.go can reach usagePct/snapshot/reconfigure.
	gate *governor

	// vaultDirectory mirrors Vaults.List() (ID -> VaultOverview),
	// populated by refreshVaultDirectory — one ListVaults call, done
	// at cold start (Task 7) and re-usable for the lifetime of the
	// current config. allowlistIDs is the subset of directory IDs
	// that config.Vaults (names or IDs) actually resolves to — the
	// scope every vault/item address is resolved against (spec §3
	// Addressing). unresolvedVaults holds allowlist entries that
	// matched zero or >1 directory vaults, surfaced as a status
	// warning rather than failing the whole engine.
	vaultDirectory   map[string]onepassword.VaultOverview
	allowlistIDs     map[string]bool
	unresolvedVaults []string
}

// Factory returns a configured Backend as a logical.Backend, matching
// the logical.Factory signature the OpenBao plugin catalog invokes.
func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := newBackend()
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

func newBackend() *Backend {
	b := &Backend{
		replicas: map[string]*vaultReplica{},
		gate:     newGovernor(time.Now),
	}
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewSDKClient(ctx, token, clientVersion)
	}
	b.Backend = &framework.Backend{
		Help:        strings.TrimSpace(backendHelp),
		BackendType: logical.TypeLogical,
		Paths: []*framework.Path{
			pathConfig(b),
			pathStatus(b),
			pathVaults(b),
			pathVaultItems(b),
			pathItem(b),
			pathField(b),
			pathRefresh(b),
			pathRefreshVault(b),
			pathInvalidate(b),
			pathInvalidateVault(b),
		},
		InitializeFunc: b.initialize,
		PeriodicFunc:   b.periodic,
	}
	return b
}

// errClientUnavailable is returned by runVaultCycle/refreshVaultDirectory
// when config is loaded but the 1P SDK client hasn't been successfully
// constructed yet — see ensureClient.
var errClientUnavailable = errors.New("op: 1password client unavailable, retrying")

// ensureClient returns the backend's current OPClient, constructing it
// lazily (and retryably) if it doesn't exist yet. Client construction
// performs a live 1P auth handshake (spec §4 Restart) and so can fail
// independently of config load — e.g. a plugin restart during a 1P
// outage. A failure or an active backoff window (governor.go's
// clientInitAllowed/recordClientInitResult, which reuses the same
// exponential backoff and rate_limited/auth_failed classification a
// cycle failure would) returns errClientUnavailable rather than
// attempting the network call again; a rate-limited/auth-failed
// classification also gates every other 1P call engine-wide, same as
// today. Called from runVaultCycle and refreshVaultDirectory — the two
// places that actually need a client — so periodic()/coldStart()'s
// existing retry loop is what drives recovery, with no separate retry
// bookkeeping here.
func (b *Backend) ensureClient(ctx context.Context) (OPClient, error) {
	b.mu.RLock()
	client := b.client
	cfg := b.config
	b.mu.RUnlock()
	if client != nil {
		return client, nil
	}
	if cfg == nil {
		return nil, errNoClient
	}
	if !b.gate.clientInitAllowed() {
		return nil, errClientUnavailable
	}

	newClient, err := b.clientFactory(ctx, cfg.ServiceAccountToken)
	b.gate.recordClientInitResult(err)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errClientUnavailable, err)
	}

	b.mu.Lock()
	b.client = newClient
	b.mu.Unlock()
	return newClient, nil
}

// runVaultCycle runs (or joins an in-flight) delta cycle for vaultID,
// creating its replica on first use. class tells the governor
// (governor.go) why this cycle is being triggered, for the burst
// brake (spec §4): periodic/passthrough-eager work is deferrable,
// miss-triggered and manual-refresh work is not.
func (b *Backend) runVaultCycle(ctx context.Context, vaultID string, class workClass) error {
	r := b.getOrCreateReplica(vaultID)

	client, err := b.ensureClient(ctx)
	if err != nil {
		return err
	}

	b.mu.RLock()
	gate := b.gate
	pathSplitRegex := b.pathSplitRegex
	b.mu.RUnlock()

	return runCycle(ctx, r, client, gate, pathSplitRegex, class)
}

// currentConfig returns the backend's live config, or nil if none has
// been written yet.
func (b *Backend) currentConfig() *Config {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.config
}

// getOrCreateReplica returns vaultID's replica, creating an empty one
// on first use (cold start is just the same delta cycle run against a
// freshly zeroed replica, spec §4 Restart).
func (b *Backend) getOrCreateReplica(vaultID string) *vaultReplica {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.replicas[vaultID]
	if !ok {
		r = newVaultReplica(vaultID)
		b.replicas[vaultID] = r
	}
	return r
}

// refreshVaultDirectory issues the engine's one ListVaults call
// (spec §4 Restart: "vaults + one list per vault") and re-resolves
// config.Vaults (names or IDs) against it into the allowlisted vault
// ID set every other path in the engine addresses against. An
// allowlist entry matching zero or more than one directory vault is
// recorded in unresolvedVaults (status warning) rather than failing
// the whole refresh.
func (b *Backend) refreshVaultDirectory(ctx context.Context) error {
	b.mu.RLock()
	cfg := b.config
	b.mu.RUnlock()
	if cfg == nil {
		return errNoClient
	}
	client, err := b.ensureClient(ctx)
	if err != nil {
		return err
	}

	overviews, err := client.ListVaults(ctx)
	if err != nil {
		return fmt.Errorf("op: list vaults: %w", err)
	}

	dir := make(map[string]onepassword.VaultOverview, len(overviews))
	for _, ov := range overviews {
		dir[ov.ID] = ov
	}

	allow := map[string]bool{}
	var unresolved []string
	for _, entry := range cfg.Vaults {
		if _, ok := dir[entry]; ok {
			allow[entry] = true
			continue
		}
		var matches []string
		for id, ov := range dir {
			if ov.Title == entry {
				matches = append(matches, id)
			}
		}
		if len(matches) == 1 {
			allow[matches[0]] = true
		} else {
			unresolved = append(unresolved, entry)
		}
	}
	sort.Strings(unresolved)

	b.mu.Lock()
	b.vaultDirectory = dir
	b.allowlistIDs = allow
	b.unresolvedVaults = unresolved
	b.mu.Unlock()
	return nil
}

// resolveVaultAddress resolves a vault path segment (name or ID, spec
// §3 Addressing) against the allowlisted vault set built by
// refreshVaultDirectory. A title shared by ≥2 allowlisted vaults
// errors, listing the candidate IDs, mirroring vaultReplica's own
// resolveTitle for items.
func (b *Backend) resolveVaultAddress(nameOrID string) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.vaultDirectory == nil {
		// spec §4 Restart: the directory (one ListVaults call) has
		// never successfully been built — distinct from "built, but
		// this name doesn't resolve to anything in it".
		return "", errReplicaEmpty
	}
	if b.allowlistIDs[nameOrID] {
		return nameOrID, nil
	}
	var matches []string
	for id := range b.allowlistIDs {
		if ov, ok := b.vaultDirectory[id]; ok && ov.Title == nameOrID {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", errNotFound
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%w: vault %q matches %d vaults: %s", errAmbiguousTitle, nameOrID, len(matches), strings.Join(matches, ", "))
	}
}

// matchesAlwaysFresh reports whether itemID in vaultID matches a
// configured always_fresh entry (D14). Patterns match raw 1P titles
// and IDs only — never split-path forms (spec §4: "matching is
// independent of D13") — so both the vault and item parts are checked
// against their ID and (if known) their raw Title.
func (b *Backend) matchesAlwaysFresh(vaultID, itemID string) bool {
	b.mu.RLock()
	cfg := b.config
	vaultTitle := ""
	if ov, ok := b.vaultDirectory[vaultID]; ok {
		vaultTitle = ov.Title
	}
	b.mu.RUnlock()
	if cfg == nil || len(cfg.AlwaysFresh) == 0 {
		return false
	}

	itemTitle := b.getOrCreateReplica(vaultID).overviewTitle(itemID)

	for _, pat := range cfg.AlwaysFresh {
		vaultPart, titlePart, ok := strings.Cut(pat, "/")
		if !ok {
			continue
		}
		vaultMatch := vaultPart == vaultID || (vaultTitle != "" && vaultPart == vaultTitle)
		itemMatch := titlePart == itemID || (itemTitle != "" && titlePart == itemTitle)
		if vaultMatch && itemMatch {
			return true
		}
	}
	return false
}

// readMeta is the spec §4 "Staleness metadata" every item/field read
// response carries.
type readMeta struct {
	ReplicaAgeSeconds float64
	ItemUpdatedAt     time.Time
	Stale             bool
	StaleSuspect      bool
}

// readMetaFor builds a readMeta for itemID's current state in r.
// Stale reflects the D6 "outage" sense specifically — the governor
// reporting a failure state (rate_limited/auth_failed/backoff) for
// this vault right now — not merely "the passthrough window happens
// to be past its TTL", which is the normal, healthy case a successful
// cycle already resolved before this call.
func (b *Backend) readMetaFor(vaultID, itemID string, r *vaultReplica, now time.Time) readMeta {
	lastCycle, staleSuspect, _ := r.freshnessSnapshot()
	return readMeta{
		ReplicaAgeSeconds: now.Sub(lastCycle).Seconds(),
		ItemUpdatedAt:     r.overviewUpdatedAt(itemID),
		Stale:             b.gate.isVaultFailing(vaultID),
		StaleSuspect:      staleSuspect,
	}
}

// errStaleServeDisabled is returned when serve_stale is false and the
// replica is older than the spec §4 staleness bound (2 ×
// refresh_interval) — the read fails outright instead of serving.
var errStaleServeDisabled = errors.New("op: replica data exceeds staleness bound and serve_stale is disabled")

// checkServeStaleAllowed enforces that bound. A replica that has
// never completed a cycle (zero lastCycle) is always "too stale" —
// Go's time.Time.Sub clamps a from-year-1 subtraction to the maximum
// representable Duration, so it naturally exceeds any bound without a
// separate IsZero check.
func checkServeStaleAllowed(cfg *Config, r *vaultReplica, now time.Time) error {
	if cfg.ServeStale {
		return nil
	}
	lastCycle, _, _ := r.freshnessSnapshot()
	if now.Sub(lastCycle) > 2*cfg.RefreshInterval {
		return errStaleServeDisabled
	}
	return nil
}

// ensureItemFresh implements the spec §4 passthrough-first read path
// for a single item within vaultID: window-fresh serves as-is (0
// calls); an expired window, an always_fresh match, or an unknown
// address triggers at most one gate-checked delta cycle; above the
// passthrough ceiling a known item serves without cycling at all
// (only a miss may still trigger one, gate-permitting). Negative-cache
// hits on an already-known-missing address short-circuit without
// attempting a cycle. found is false (with no error) for a clean
// "not found" — callers negative-cache that themselves so the miss
// path stays uniform whether or not a cycle was attempted.
//
// resolve looks up the current itemID from vaultID's replica; it is
// called once up front and, if a cycle runs, again afterward to pick
// up newly-fetched data. It's a closure rather than a flat address
// string so callers needing split-path (D13) resolution — which also
// derives section/field boundaries alongside the item ID (Task 6's
// paths_field.go) — can capture that extra state on each call.
// negCacheKey scopes the negative-cache entry for a miss; callers pass
// the original raw request path so repeated misses on equivalent
// addresses (flat or split-path) are suppressed uniformly.
func (b *Backend) ensureItemFresh(ctx context.Context, vaultID, negCacheKey string, resolve func() (string, error)) (itemID string, found bool, err error) {
	b.mu.RLock()
	cfg := b.config
	b.mu.RUnlock()
	if cfg == nil {
		return "", false, errNoClient
	}

	r := b.getOrCreateReplica(vaultID)
	now := time.Now()

	id, rerr := resolve()
	if rerr != nil && errors.Is(rerr, errAmbiguousTitle) {
		return "", false, rerr
	}
	known := rerr == nil

	aboveCeiling := cfg.Passthrough && b.gate.usagePct() >= cfg.PassthroughCeilingPct

	var needCycle bool
	var class workClass
	switch {
	case !cfg.Passthrough:
		needCycle = false
	case !known:
		needCycle = true
		class = workClassMiss
	case aboveCeiling:
		needCycle = false
	default:
		lastCycle, _, _ := r.freshnessSnapshot()
		windowExpired := cfg.PassthroughTTL == 0 || now.Sub(lastCycle) >= cfg.PassthroughTTL
		needCycle = windowExpired || b.matchesAlwaysFresh(vaultID, id)
		class = workClassPeriodic
	}

	if needCycle {
		if !known && r.negativeCacheHit(negCacheKey, cfg.NegativeCacheTTL, now) {
			return "", false, nil
		}
		_ = b.runVaultCycle(ctx, vaultID, class) // failure/denial just means "couldn't refresh"; fall through and serve what's known
		id, rerr = resolve()
		if rerr != nil && errors.Is(rerr, errAmbiguousTitle) {
			return "", false, rerr
		}
		known = rerr == nil
	}

	if !known {
		// spec §4 Restart: cold start during an outage must fail
		// reads with an explicit "replica empty" error rather than a
		// generic not-found — but only when the replica truly has no
		// data at all (itemCount==0). A replica that was previously
		// warm and then invalidated (D15) also has a zeroed lastCycle
		// (vaultReplica.invalidate) but still holds its last-known
		// items, so it must NOT be mistaken for an incomplete cold
		// start here.
		if r.itemCount() == 0 {
			return "", false, errReplicaEmpty
		}
		r.negativeCacheStore(negCacheKey, now)
		return "", false, nil
	}
	return id, true, nil
}

// errReplicaEmpty is the spec §4 Restart "cold start during an
// outage" error: the mount is up but a vault's replica has never
// completed a single delta cycle, so there is nothing to serve even
// stale (D4: no on-disk copy). Distinct from a genuine not-found.
var errReplicaEmpty = errors.New("op: replica empty, cold start incomplete")

// coldStart attempts to fully materialize every allowlisted vault
// (spec §4 Restart): build the vault directory if it isn't built yet,
// then run a delta cycle for any vault that has never completed one.
// Both refreshVaultDirectory and runVaultCycle route through
// ensureClient, so a not-yet-constructed 1P SDK client (e.g. a restart
// during a 1P outage — the bug this retry loop exists to fix) is
// retried here too, not just vault cycles. Safe to call repeatedly and
// idempotent when everything is already warm — PeriodicFunc calls it
// every tick, which is how a failed attempt gets retried: a failure
// leaves that vault's governor backoff (or, for a client-construction
// failure, clientInitState's engine-wide backoff) active (governor.go),
// which self-paces the next retry exactly like any other cycle
// failure, satisfying "cold start retries with backoff" without
// separate retry bookkeeping here.
func (b *Backend) coldStart(ctx context.Context) {
	cfg := b.currentConfig()
	if cfg == nil {
		return
	}

	b.mu.RLock()
	dirBuilt := b.vaultDirectory != nil
	b.mu.RUnlock()
	if !dirBuilt {
		_ = b.refreshVaultDirectory(ctx)
	}

	b.mu.RLock()
	ids := make([]string, 0, len(b.allowlistIDs))
	for id := range b.allowlistIDs {
		ids = append(ids, id)
	}
	b.mu.RUnlock()

	for _, id := range ids {
		r := b.getOrCreateReplica(id)
		if r.itemCount() == 0 {
			_ = b.runVaultCycle(ctx, id, workClassPeriodic)
		}
	}
}

// loadPersistedConfig loads a config already durable in storage into
// runtime state (spec §4 Restart / D4: config itself is the one
// durable 1P artifact — item data is not). It's the Initialize hook's
// counterpart to applyConfig's post-validation steps (config.go),
// without re-persisting (already there) or re-validating (a
// previously-accepted config is trusted as-is on reload).
//
// This is a pure storage read and MUST NEVER depend on network
// success: unlike applyConfig (an explicit operator write, where a bad
// token should fail synchronously), loadPersistedConfig runs on every
// plugin restart, including one that lands mid-1P-outage. Client
// construction — which performs a live auth handshake, spec §4
// Restart — is deliberately NOT done here; b.config (and thus
// currentConfig()) is set unconditionally whenever storage holds a
// config, which is what lets periodic()/coldStart() reach ensureClient
// and retry client construction on every tick even though this call
// already returned. The only failure mode left is a corrupt persisted
// path_split regex, which is unreachable in practice (applyConfig
// validates it before ever persisting, config.go) — genuinely nothing
// to load in that case.
func (b *Backend) loadPersistedConfig(ctx context.Context, cfg *Config) error {
	var pathSplitRegex *regexp.Regexp
	if cfg.PathSplit != "" {
		re, err := regexp.Compile(cfg.PathSplit)
		if err != nil {
			return fmt.Errorf("path_split: invalid delimiter/regex: %w", err)
		}
		pathSplitRegex = re
	}

	b.mu.Lock()
	b.config = cfg
	b.pathSplitRegex = pathSplitRegex
	b.mu.Unlock()

	b.gate.reconfigure(cfg)
	return nil
}

// initialize is the framework.Backend InitializeFunc, invoked just
// after the plugin is mounted (spec §4 Restart). If config is already
// present in storage (a restart, not a fresh mount), it's loaded into
// runtime state unconditionally (loadPersistedConfig has no network
// dependency), then cold start attempts to materialize every
// allowlisted vault — including constructing the 1P SDK client, which
// coldStart's own retry path (ensureClient, via
// runVaultCycle/refreshVaultDirectory) now owns rather than this hook.
// Never returns an error — a cold-start failure (1P unreachable, or
// client construction itself failing/backing off) must not prevent the
// mount from coming up; reads simply fail with errReplicaEmpty until a
// later PeriodicFunc tick's retry succeeds. Because config load no
// longer depends on client construction, periodic() always reaches
// coldStart() on every tick regardless of how initialize() went, which
// is what makes a restart-during-outage recover automatically once the
// outage clears, with no operator action (spec §4: "the engine retries
// initial load with the same backoff").
func (b *Backend) initialize(ctx context.Context, req *logical.InitializationRequest) error {
	cfg, err := getConfigFromStorage(ctx, req.Storage)
	if err != nil || cfg == nil {
		return nil
	}
	if err := b.loadPersistedConfig(ctx, cfg); err != nil {
		return nil
	}
	b.coldStart(ctx)
	return nil
}

// periodic is the framework.Backend PeriodicFunc. OpenBao invokes it
// roughly once a minute regardless of refresh_interval, so it
// self-throttles: a vault's cycle only actually runs once
// refresh_interval has elapsed since its last successful cycle (spec
// §4). It also drives cold-start retries (coldStart is a no-op once
// everything is warm) and the optional ratelimit probe's own
// self-throttled cadence (governor.go).
func (b *Backend) periodic(ctx context.Context, req *logical.Request) error {
	cfg := b.currentConfig()
	if cfg == nil {
		return nil
	}

	b.coldStart(ctx)

	b.mu.RLock()
	ids := make([]string, 0, len(b.allowlistIDs))
	for id := range b.allowlistIDs {
		ids = append(ids, id)
	}
	b.mu.RUnlock()

	now := time.Now()
	for _, id := range ids {
		r := b.getOrCreateReplica(id)
		lastCycle, _, _ := r.freshnessSnapshot()
		if lastCycle.IsZero() || now.Sub(lastCycle) < cfg.RefreshInterval {
			continue
		}
		_ = b.runVaultCycle(ctx, id, workClassPeriodic)
	}

	b.gate.runProbeIfDue(ctx)
	return nil
}

// ensureVaultWindowFresh applies the same window/ceiling logic as
// ensureItemFresh but for a whole-vault listing (vaults/<vault>/items,
// Task 6's paths_vaults.go) rather than a single item — there is no
// per-item always_fresh concept for a listing, and no miss/negative-
// cache handling since a listing can't be "unknown".
func (b *Backend) ensureVaultWindowFresh(ctx context.Context, vaultID string) {
	b.mu.RLock()
	cfg := b.config
	b.mu.RUnlock()
	if cfg == nil || !cfg.Passthrough {
		return
	}
	if b.gate.usagePct() >= cfg.PassthroughCeilingPct {
		return
	}

	r := b.getOrCreateReplica(vaultID)
	lastCycle, _, _ := r.freshnessSnapshot()
	windowExpired := cfg.PassthroughTTL == 0 || time.Since(lastCycle) >= cfg.PassthroughTTL
	if !windowExpired {
		return
	}
	_ = b.runVaultCycle(ctx, vaultID, workClassPeriodic)
}
