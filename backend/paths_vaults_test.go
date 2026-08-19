package backend

import (
	"context"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

func TestPathVaults_List_ZeroCalls(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)

	req := &logical.Request{
		Operation: logical.ListOperation,
		Path:      "vaults",
		Storage:   storage,
	}
	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}

	keys, ok := resp.Data["keys"].([]string)
	if !ok || len(keys) != 1 || keys[0] != "v1" {
		t.Fatalf("keys = %v, want [v1]", resp.Data["keys"])
	}
	info, ok := resp.Data["key_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("key_info missing or wrong type: %+v", resp.Data["key_info"])
	}
	v1Info, ok := info["v1"].(map[string]interface{})
	if !ok || v1Info["title"] != "Infra" {
		t.Errorf("key_info[v1] = %+v, want title=Infra", info["v1"])
	}

	if fake.ListVaultsCalls != 0 || fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("vaults list: ListVaultsCalls=%d ListItemsCalls=%d GetItemsCalls=%d, want 0/0/0 (served from cached directory)",
			fake.ListVaultsCalls, fake.ListItemsCalls, fake.GetItemsCalls)
	}
}
