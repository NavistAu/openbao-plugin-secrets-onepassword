package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

func readStatus(t *testing.T, b *Backend, storage logical.Storage) *logical.Response {
	t.Helper()
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "status",
		Storage:   storage,
	}
	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("status read: unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("status read: nil response")
	}
	return resp
}

func TestPathStatus_Basic(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres") // warm the replica

	resp := readStatus(t, b, storage)
	if resp.Data["sdk_version"] != sdkVersion {
		t.Errorf("sdk_version = %v, want %v", resp.Data["sdk_version"], sdkVersion)
	}
	gov, ok := resp.Data["governor"].(map[string]interface{})
	if !ok || gov["state"] != "normal" {
		t.Errorf("governor = %+v, want state=normal", resp.Data["governor"])
	}
	vaults, ok := resp.Data["vaults"].(map[string]interface{})
	if !ok {
		t.Fatalf("vaults missing or wrong type: %+v", resp.Data["vaults"])
	}
	v1, ok := vaults["v1"].(map[string]interface{})
	if !ok || v1["item_count"] != 2 {
		t.Errorf("vaults[v1] = %+v, want item_count=2", vaults["v1"])
	}
}

func TestPathStatus_NoSecretMaterial(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")

	resp := readStatus(t, b, storage)
	// Serialize-ish check: none of the top-level string values should
	// contain the token or any field value we seeded.
	for _, secret := range []string{"tok", "hunter2", "swordfish"} {
		if containsAny(resp.Data, secret) {
			t.Errorf("status response contains secret material %q: %+v", secret, resp.Data)
		}
	}
}

func containsAny(data map[string]interface{}, needle string) bool {
	for _, v := range data {
		switch vv := v.(type) {
		case string:
			if strings.Contains(vv, needle) {
				return true
			}
		case map[string]interface{}:
			if containsAny(vv, needle) {
				return true
			}
		}
	}
	return false
}

func TestPathStatus_AlwaysFreshUnmatched(t *testing.T) {
	b, storage, _ := setupItemBackend(t, map[string]interface{}{
		"always_fresh": "Infra/does-not-exist,Infra/postgres",
	})
	mustReadItem(t, b, storage, "Infra", "postgres") // warm the replica so postgres is indexed

	resp := readStatus(t, b, storage)
	unmatched, ok := resp.Data["always_fresh_unmatched"].([]string)
	if !ok {
		t.Fatalf("always_fresh_unmatched missing or wrong type: %+v", resp.Data["always_fresh_unmatched"])
	}
	if len(unmatched) != 1 || unmatched[0] != "Infra/does-not-exist" {
		t.Errorf("always_fresh_unmatched = %v, want [Infra/does-not-exist]", unmatched)
	}
}
