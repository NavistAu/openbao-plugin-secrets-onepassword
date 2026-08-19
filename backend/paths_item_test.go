package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// setupItemBackend builds a Backend with a single-vault ("Infra" / ID
// "v1") FakeOPClient seeded with two items, writes config (with any
// overrides), and populates the vault directory — everything Task 7's
// cold start will eventually do automatically, done by hand here
// since Task 6 predates the cold-start wiring. Fake call counters are
// reset to zero after setup so every test only sees its own calls.
func setupItemBackend(t *testing.T, overrides map[string]interface{}) (*Backend, logical.Storage, *FakeOPClient) {
	t.Helper()
	b, storage := newTestBackend(t)

	fake := NewFakeOPClient()
	fake.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}
	fake.Items["v1"] = []onepassword.ItemOverview{
		{ID: "i1", Title: "postgres", State: onepassword.ItemStateActive, UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "i2", Title: "redis", State: onepassword.ItemStateActive, UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{
		"i1": {
			ID: "i1", Title: "postgres",
			Fields: []onepassword.ItemField{
				{ID: "f1", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "hunter2"},
			},
		},
		"i2": {
			ID: "i2", Title: "redis",
			Fields: []onepassword.ItemField{
				{ID: "f2", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "swordfish"},
			},
		},
	}
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }

	data := map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}
	for k, v := range overrides {
		data[k] = v
	}
	if resp, err := writeConfig(t, b, storage, data); err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("write config: resp=%+v err=%v", resp, err)
	}
	if err := b.refreshVaultDirectory(context.Background()); err != nil {
		t.Fatalf("refreshVaultDirectory: %v", err)
	}

	fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls = 0, 0, 0
	return b, storage, fake
}

func readItem(t *testing.T, b *Backend, storage logical.Storage, vault, item string) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "item/" + vault + "/" + item,
		Storage:   storage,
	}
	return b.HandleRequest(context.Background(), req)
}

func mustReadItem(t *testing.T, b *Backend, storage logical.Storage, vault, item string) *logical.Response {
	t.Helper()
	resp, err := readItem(t, b, storage, vault, item)
	if err != nil {
		t.Fatalf("read item %s/%s: unexpected error: %v", vault, item, err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("read item %s/%s: resp=%+v, want a successful response", vault, item, resp)
	}
	return resp
}

// --- the passthrough matrix (spec §4, spec §7 bench list) ---

func TestPathItem_WindowFresh_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	// First read warms the (never-cycled) replica: 1 list + 1 get.
	mustReadItem(t, b, storage, "Infra", "postgres")
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	// Second read, immediately after, is within the default 1m
	// passthrough_ttl window: 0 calls.
	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("window-fresh read: ListItemsCalls=%d GetItemsCalls=%d, want 0/0", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

func TestPathItem_ExpiredWindow_TriggersCycleThenServes(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"passthrough_ttl": "1s",
	})

	mustReadItem(t, b, storage, "Infra", "postgres")
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	time.Sleep(1100 * time.Millisecond) // window (1s) now expired
	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 1 {
		t.Errorf("expired-window read: ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}
	if fake.GetItemsCalls != 0 {
		t.Errorf("expired-window read (item unchanged): GetItemsCalls = %d, want 0", fake.GetItemsCalls)
	}
}

func TestPathItem_TTLZero_CyclesEveryRead(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"passthrough_ttl": "0s",
	})

	mustReadItem(t, b, storage, "Infra", "postgres")
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 1 {
		t.Errorf("ttl=0, 2nd read: ListItemsCalls = %d, want 1 (always-fresh)", fake.ListItemsCalls)
	}

	fake.ListItemsCalls = 0
	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 1 {
		t.Errorf("ttl=0, 3rd read: ListItemsCalls = %d, want 1 (every single read cycles)", fake.ListItemsCalls)
	}
}

func TestPathItem_AlwaysFresh_CyclesWhileWarm_PlainItemStaysWarm(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"always_fresh": "Infra/postgres",
	})

	// Warm the whole vault via the plain item.
	mustReadItem(t, b, storage, "Infra", "redis")
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	// always_fresh item: cycles even though the vault's window is warm.
	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 1 {
		t.Errorf("always_fresh read: ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}

	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	// Plain item, same (still-warm) vault: 0 calls.
	mustReadItem(t, b, storage, "Infra", "redis")
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("plain item read after always_fresh sibling read: ListItemsCalls=%d GetItemsCalls=%d, want 0/0", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

func TestPathItem_AboveCeiling_KnownItem_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"passthrough_ttl":         "1s",
		"hourly_read_limit":       10,
		"passthrough_ceiling_pct": 25,
	})

	mustReadItem(t, b, storage, "Infra", "postgres")
	time.Sleep(1100 * time.Millisecond) // ensure the window would otherwise be expired

	// Push local usage over the 25% ceiling (25% of 10 = 2.5 -> 3+).
	b.gate.recordRequest("v1", 3, nil)
	if pct := b.gate.usagePct(); pct < 25 {
		t.Fatalf("test setup: usagePct = %d, want >= 25", pct)
	}

	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0
	mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("above-ceiling known-item read: ListItemsCalls=%d GetItemsCalls=%d, want 0/0", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

func TestPathItem_FailureState_KnownItem_ServesStale_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"passthrough_ttl": "1s",
	})

	mustReadItem(t, b, storage, "Infra", "postgres")
	time.Sleep(1100 * time.Millisecond) // window now expired, would otherwise trigger a cycle

	// Force the governor into rate_limited.
	b.gate.recordRequest("v1", 1, &onepassword.RateLimitExceededError{})
	if b.gate.snapshot().State != "rate_limited" {
		t.Fatalf("test setup: governor state = %q, want rate_limited", b.gate.snapshot().State)
	}

	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0
	resp := mustReadItem(t, b, storage, "Infra", "postgres")
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("failure-state known-item read: ListItemsCalls=%d GetItemsCalls=%d, want 0/0 (gate denies before any client call)", fake.ListItemsCalls, fake.GetItemsCalls)
	}
	if stale, _ := resp.Data["stale"].(bool); !stale {
		t.Errorf("failure-state read: stale = %v, want true", resp.Data["stale"])
	}
}

func TestPathItem_FailureState_UnknownItem_FailsFast_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	b.gate.recordRequest("v1", 1, &onepassword.RateLimitExceededError{})

	resp, err := readItem(t, b, storage, "Infra", "never-seen-item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("unknown item under rate_limited: resp=%+v, want an error response", resp)
	}
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("unknown-item read under rate_limited: ListItemsCalls=%d GetItemsCalls=%d, want 0/0 (fails fast, no cycle attempt)", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

// --- addressing ---

func TestPathItem_ByID(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	resp := mustReadItem(t, b, storage, "Infra", "i1")
	if resp.Data["title"] != "postgres" {
		t.Errorf("read by ID: title = %v, want postgres", resp.Data["title"])
	}
}

func TestPathItem_ByVaultID(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	resp := mustReadItem(t, b, storage, "v1", "postgres")
	if resp.Data["id"] != "i1" {
		t.Errorf("read by vault ID: id = %v, want i1", resp.Data["id"])
	}
}

func TestPathItem_AmbiguousTitle_Errors(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	fake.Items["v1"] = append(fake.Items["v1"], onepassword.ItemOverview{
		ID: "i3", Title: "postgres", State: onepassword.ItemStateActive, UpdatedAt: time.Now(),
	})
	fake.ItemBodies["v1"]["i3"] = onepassword.Item{ID: "i3", Title: "postgres"}

	resp, err := readItem(t, b, storage, "Infra", "postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("ambiguous title read: resp=%+v, want an error response", resp)
	}
	errStr, _ := resp.Data["error"].(string)
	if !strings.Contains(errStr, "i1") || !strings.Contains(errStr, "i3") {
		t.Errorf("ambiguous title error %q does not list both candidate IDs", errStr)
	}
}

func TestPathItem_NotFound(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	resp, err := readItem(t, b, storage, "Infra", "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("not-found read: resp=%+v, want an error response", resp)
	}
}

func TestPathItem_SplitPathAddressing(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"path_split": "__",
	})
	fake.Items["v1"] = []onepassword.ItemOverview{
		{ID: "i1", Title: "db.example.com__postgres", State: onepassword.ItemStateActive, UpdatedAt: time.Now()},
	}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{
		"i1": {ID: "i1", Title: "db.example.com__postgres"},
	}

	resp := mustReadItem(t, b, storage, "Infra", "db.example.com/postgres")
	if resp.Data["id"] != "i1" {
		t.Errorf("split-path item read: id = %v, want i1", resp.Data["id"])
	}
}

func TestPathItem_ServeStaleDisabled_FailsPastBound(t *testing.T) {
	b, storage, _ := setupItemBackend(t, map[string]interface{}{
		"serve_stale": false,
		// refresh_interval stays at its 15m default: TypeDurationSecond
		// truncates to whole seconds, so a sub-second bound isn't
		// expressible — the replica is backdated below instead of
		// relying on a real sleep past 2x the interval.
	})
	mustReadItem(t, b, storage, "Infra", "postgres") // warms the replica

	// Force the governor into auth_failed so the read path's own
	// expired-window cycle attempt (default passthrough_ttl=1m, and
	// the backdate below trivially exceeds it) can't refresh lastCycle
	// back to "now" before the staleness-bound check runs.
	b.gate.recordRequest("v1", 1, authFailedError("you are not authenticated"))

	r := b.getOrCreateReplica("v1")
	r.mu.Lock()
	r.lastCycle = time.Now().Add(-time.Hour) // >> 2x the 15m refresh_interval
	r.mu.Unlock()

	resp, err := readItem(t, b, storage, "Infra", "postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("serve_stale=false past bound: resp=%+v, want an error response", resp)
	}
}
