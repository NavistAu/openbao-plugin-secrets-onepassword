package backend

import (
	"context"
	"sort"
	"strings"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathStatus defines `status` (read): per-vault replica age, item
// counts, last refresh result, failure counters, rate_limited/
// auth_failed state, throttled mode, invalidated_at, probe warnings,
// and SDK version (spec §3). Never includes secret material.
func pathStatus(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: "status",

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathStatusRead,
				Summary:  "Read op engine status.",
			},
		},

		HelpSynopsis:    "op engine status.",
		HelpDescription: "Per-vault replica/refresh state, rate governor state, and probe status. No secret material.",
	}
}

func (b *Backend) pathStatusRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.mu.RLock()
	cfg := b.config
	allowlistIDs := b.allowlistIDs
	vaultDirectory := b.vaultDirectory
	unresolvedVaults := b.unresolvedVaults
	// Snapshot the replicas map (not just the reference) under lock:
	// getOrCreateReplica mutates b.replicas under b.mu.Lock(), so
	// iterating the live map after releasing b.mu would race with a
	// concurrent write.
	replicas := make(map[string]*vaultReplica, len(b.replicas))
	for id, r := range b.replicas {
		replicas[id] = r
	}
	b.mu.RUnlock()

	snap := b.gate.snapshot()

	governorData := map[string]interface{}{
		"state":                snap.State,
		"resume_at":            snap.ResumeAt,
		"throttled":            snap.Throttled,
		"hourly_usage_pct":     snap.HourlyUsagePct,
		"daily_usage_pct":      snap.DailyUsagePct,
		"usage_pct":            snap.UsagePct,
		"client_init_failures": snap.ClientInitFailures,
		"client_init_last_err": snap.ClientInitLastErr,
		"probe": map[string]interface{}{
			"configured":       snap.ProbeConfigured,
			"healthy":          snap.ProbeHealthy,
			"error":            snap.ProbeErr,
			"warning":          snap.ProbeWarning,
			"probed_daily_pct": snap.ProbedAccountPct,
			"last_probe_at":    snap.LastProbeAt,
		},
	}

	vaultsData := map[string]interface{}{}
	ids := make([]string, 0, len(allowlistIDs))
	for id := range allowlistIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := map[string]interface{}{}
		if ov, ok := vaultDirectory[id]; ok {
			entry["title"] = ov.Title
		}
		if r, ok := replicas[id]; ok {
			lastCycle, staleSuspect, invalidatedAt := r.freshnessSnapshot()
			entry["item_count"] = r.itemCount()
			entry["last_refresh"] = lastCycle
			entry["consecutive_failures"] = r.consecutiveFailureCount()
			entry["stale_suspect"] = staleSuspect
			entry["invalidated_at"] = invalidatedAt
		} else {
			entry["item_count"] = 0
			entry["consecutive_failures"] = 0
		}
		if failures, ok := snap.Backoff[id]; ok {
			entry["backoff_consecutive_failures"] = failures
		}
		vaultsData[id] = entry
	}

	var alwaysFreshUnmatched []string
	if cfg != nil {
		alwaysFreshUnmatched = unmatchedAlwaysFreshPatterns(cfg, allowlistIDs, replicas, vaultDirectory)
	}

	return &logical.Response{Data: map[string]interface{}{
		"sdk_version":            sdkVersion,
		"governor":               governorData,
		"vaults":                 vaultsData,
		"unresolved_vaults":      unresolvedVaults,
		"always_fresh_unmatched": alwaysFreshUnmatched,
	}}, nil
}

// unmatchedAlwaysFreshPatterns reports configured always_fresh (D14)
// entries that match no item in any allowlisted vault's current
// replica (spec §4: "patterns matching nothing in the index are
// surfaced in status").
func unmatchedAlwaysFreshPatterns(cfg *Config, allowlistIDs map[string]bool, replicas map[string]*vaultReplica, vaultDirectory map[string]onepassword.VaultOverview) []string {
	var unmatched []string
	for _, pat := range cfg.AlwaysFresh {
		vaultPart, titlePart, ok := strings.Cut(pat, "/")
		if !ok {
			unmatched = append(unmatched, pat)
			continue
		}
		if !patternMatchesAnyItem(vaultPart, titlePart, allowlistIDs, replicas, vaultDirectory) {
			unmatched = append(unmatched, pat)
		}
	}
	sort.Strings(unmatched)
	return unmatched
}

func patternMatchesAnyItem(vaultPart, titlePart string, allowlistIDs map[string]bool, replicas map[string]*vaultReplica, vaultDirectory map[string]onepassword.VaultOverview) bool {
	for id := range allowlistIDs {
		vaultTitle := ""
		if ov, ok := vaultDirectory[id]; ok {
			vaultTitle = ov.Title
		}
		if vaultPart != id && vaultPart != vaultTitle {
			continue
		}
		r, ok := replicas[id]
		if !ok {
			continue
		}
		if _, err := r.resolveItemAddress(titlePart); err == nil {
			return true
		}
	}
	return false
}
