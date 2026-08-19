package backend

import (
	"context"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

// newTestBackend builds a *Backend directly (bypassing Factory) so
// tests can reach in and override clientFactory before exercising
// requests — the standard way to observe client re-initialization
// without a real 1Password SDK client.
func newTestBackend(t *testing.T) (*Backend, logical.Storage) {
	t.Helper()

	b := newBackend()
	conf := logical.TestBackendConfig()
	conf.StorageView = &logical.InmemStorage{}
	if err := b.Setup(context.Background(), conf); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return b, conf.StorageView
}

func TestBackend_RunVaultCycle_NoClient(t *testing.T) {
	b, _ := newTestBackend(t)

	if err := b.runVaultCycle(context.Background(), "v1", workClassPeriodic); err != errNoClient {
		t.Fatalf("runVaultCycle before config = %v, want errNoClient", err)
	}
}

func TestBackend_RunVaultCycle_LazyReplica(t *testing.T) {
	b, storage := newTestBackend(t)

	fake := NewFakeOPClient()
	fake.Items["v1"] = nil // vault exists, no items
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return fake, nil
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config",
		Storage:   storage,
		Data: map[string]interface{}{
			"service_account_token": "tok",
			"vaults":                "v1",
		},
	}
	if _, err := b.HandleRequest(context.Background(), req); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, ok := b.replicas["v1"]; ok {
		t.Fatalf("replica for v1 should not exist before the first cycle")
	}
	if err := b.runVaultCycle(context.Background(), "v1", workClassPeriodic); err != nil {
		t.Fatalf("runVaultCycle: %v", err)
	}
	if _, ok := b.replicas["v1"]; !ok {
		t.Fatalf("runVaultCycle should have lazily created the v1 replica")
	}
	if fake.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}
}
