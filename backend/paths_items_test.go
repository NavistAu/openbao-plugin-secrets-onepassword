package backend

import (
	"context"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

func TestPathVaultItems_List(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	req := &logical.Request{
		Operation: logical.ListOperation,
		Path:      "vaults/Infra/items",
		Storage:   storage,
	}
	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}

	// First list triggers a cycle (replica never cycled): 1 list call.
	if fake.ListItemsCalls != 1 {
		t.Errorf("first list: ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}

	keys, ok := resp.Data["keys"].([]string)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys = %v, want 2 item IDs", resp.Data["keys"])
	}
	info, ok := resp.Data["key_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("key_info missing or wrong type: %+v", resp.Data["key_info"])
	}
	i1, ok := info["i1"].(map[string]interface{})
	if !ok || i1["title"] != "postgres" {
		t.Errorf("key_info[i1] = %+v, want title=postgres", info["i1"])
	}

	// Second list, immediately after, is window-fresh: 0 calls.
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0
	if _, err := b.HandleRequest(context.Background(), req); err != nil {
		t.Fatalf("second list: unexpected error: %v", err)
	}
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("window-fresh second list: ListItemsCalls=%d GetItemsCalls=%d, want 0/0", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}
