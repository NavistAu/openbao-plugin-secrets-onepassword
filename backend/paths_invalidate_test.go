package backend

import (
	"context"
	"testing"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func writeInvalidate(t *testing.T, b *Backend, storage logical.Storage, path string) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      path,
		Storage:   storage,
	}
	return b.HandleRequest(context.Background(), req)
}

func TestPathInvalidate_All_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres") // warm the replica
	fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls = 0, 0, 0

	resp, err := writeInvalidate(t, b, storage, "invalidate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if fake.ListVaultsCalls != 0 || fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Fatalf("invalidate (all): ListVaultsCalls=%d ListItemsCalls=%d GetItemsCalls=%d, want 0/0/0",
			fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls)
	}

	_, staleSuspect, invalidatedAt := b.getOrCreateReplica("v1").freshnessSnapshot()
	if !staleSuspect {
		t.Errorf("staleSuspect after invalidate = false, want true")
	}
	if invalidatedAt.IsZero() {
		t.Errorf("invalidatedAt after invalidate = zero, want set")
	}
}

func TestPathInvalidate_Vault_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")
	fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls = 0, 0, 0

	resp, err := writeInvalidate(t, b, storage, "invalidate/Infra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil && resp.IsError() {
		t.Fatalf("resp=%+v, want success", resp)
	}
	if fake.ListVaultsCalls != 0 || fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Fatalf("invalidate (scoped): ListVaultsCalls=%d ListItemsCalls=%d GetItemsCalls=%d, want 0/0/0",
			fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

func TestPathInvalidate_ClearsNegativeCache_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")

	// Prime a negative-cache miss.
	r := b.getOrCreateReplica("v1")
	r.negativeCacheStore("ghost-item", time.Now())
	fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls = 0, 0, 0

	if _, err := writeInvalidate(t, b, storage, "invalidate/Infra"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.ListVaultsCalls != 0 || fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Fatalf("invalidate: unexpected calls %d/%d/%d", fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls)
	}
	if r.negativeCacheHit("ghost-item", time.Hour, time.Now()) {
		t.Errorf("negative cache entry survived invalidate")
	}
}

// invalidate then read under rate_limited serves stale_suspect
// without requests — the exact scenario named in the Task 7 brief:
// invalidate a vault (zero-spend), force the governor into
// rate_limited, then read a known item and confirm it serves the
// last-known (now stale_suspect) data with zero new FakeOPClient
// calls.
func TestPathInvalidate_ThenReadUnderRateLimited_ServesStaleSuspectWithoutRequests(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres") // warm the replica

	if _, err := writeInvalidate(t, b, storage, "invalidate/Infra"); err != nil {
		t.Fatalf("invalidate: unexpected error: %v", err)
	}

	b.gate.recordRequest("v1", 1, &onepassword.RateLimitExceededError{})
	if b.gate.snapshot().State != "rate_limited" {
		t.Fatalf("test setup: governor state = %q, want rate_limited", b.gate.snapshot().State)
	}

	fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls = 0, 0, 0
	resp := mustReadItem(t, b, storage, "Infra", "postgres")

	if fake.ListVaultsCalls != 0 || fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Fatalf("invalidate+rate_limited read: ListVaultsCalls=%d ListItemsCalls=%d GetItemsCalls=%d, want 0/0/0",
			fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls)
	}
	if staleSuspect, _ := resp.Data["stale_suspect"].(bool); !staleSuspect {
		t.Errorf("stale_suspect = %v, want true", resp.Data["stale_suspect"])
	}
}
