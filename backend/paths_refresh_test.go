package backend

import (
	"context"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

func writeRefresh(t *testing.T, b *Backend, storage logical.Storage, path string) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      path,
		Storage:   storage,
	}
	return b.HandleRequest(context.Background(), req)
}

func TestPathRefresh_All_SpendsNow(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	resp, err := writeRefresh(t, b, storage, "refresh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if fake.ListItemsCalls != 1 {
		t.Errorf("refresh (never-cycled vault): ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}
	results, ok := resp.Data["results"].(map[string]interface{})
	if !ok || results["v1"] != "ok" {
		t.Errorf("results = %+v, want v1=ok", resp.Data["results"])
	}
}

func TestPathRefresh_Vault_Scoped(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	resp, err := writeRefresh(t, b, storage, "refresh/Infra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil && resp.IsError() {
		t.Fatalf("resp=%+v, want success", resp)
	}
	if fake.ListItemsCalls != 1 {
		t.Errorf("scoped refresh: ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}
}

func TestPathRefresh_Vault_UnknownVault_Errors(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)

	resp, err := writeRefresh(t, b, storage, "refresh/NoSuchVault")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("refresh of unknown vault: resp=%+v, want an error response", resp)
	}
}

// Manual refresh is exempt from the burst brake's deferral of
// deferrable (periodic) work — spec §4: "manual refreshes still serve
// up to the cap".
func TestPathRefresh_ExemptFromBurstBrake(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"hourly_read_limit": 100,
	})
	mustReadItem(t, b, storage, "Infra", "postgres") // warms the replica (1 list + 1 get chunk = 2 units)

	// Push hourly usage to 87% (past the 80% burst-brake threshold,
	// short of the 100% hard cap).
	b.gate.recordRequest("v1", 85, nil)

	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0
	resp, err := writeRefresh(t, b, storage, "refresh/Infra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil && resp.IsError() {
		t.Fatalf("manual refresh past burst-brake threshold: resp=%+v, want success", resp)
	}
	if fake.ListItemsCalls != 1 {
		t.Errorf("manual refresh past burst-brake threshold: ListItemsCalls = %d, want 1 (manual is exempt)", fake.ListItemsCalls)
	}
}
