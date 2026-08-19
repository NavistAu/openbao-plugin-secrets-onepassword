package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func TestBackend_Initialize_NoConfigYet_NoOp(t *testing.T) {
	b, storage := newTestBackend(t)
	if err := b.Initialize(context.Background(), &logical.InitializationRequest{Storage: storage}); err != nil {
		t.Fatalf("Initialize with no persisted config: %v", err)
	}
	if b.currentConfig() != nil {
		t.Errorf("currentConfig = %+v, want nil", b.currentConfig())
	}
}

// Initialize (the framework.Backend InitializeFunc, invoked on mount)
// must load a config already durable in storage into runtime state
// and fully materialize every allowlisted vault before a read is
// served — spec §4 Restart.
func TestBackend_Initialize_LoadsConfigAndColdStarts(t *testing.T) {
	storage := &logical.InmemStorage{}

	fake := NewFakeOPClient()
	fake.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}
	fake.Items["v1"] = []onepassword.ItemOverview{{ID: "i1", Title: "postgres", State: onepassword.ItemStateActive}}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{"i1": {ID: "i1", Title: "postgres"}}

	// First "mount": write config (like a normal operator config write).
	b1 := newBackend()
	conf1 := logical.TestBackendConfig()
	conf1.StorageView = storage
	if err := b1.Setup(context.Background(), conf1); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	b1.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }
	if _, err := writeConfig(t, b1, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A "restart": a fresh Backend over the same storage, never
	// written to directly — everything must come from Initialize.
	b2 := newBackend()
	conf2 := logical.TestBackendConfig()
	conf2.StorageView = storage
	if err := b2.Setup(context.Background(), conf2); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	b2.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }

	if err := b2.Initialize(context.Background(), &logical.InitializationRequest{Storage: storage}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if b2.currentConfig() == nil {
		t.Fatalf("Initialize did not load config from storage into runtime state")
	}
	if b2.getOrCreateReplica("v1").itemCount() != 1 {
		t.Fatalf("Initialize did not fully materialize the allowlisted vault before returning")
	}

	resp := mustReadItem(t, b2, storage, "Infra", "postgres")
	if resp.Data["title"] != "postgres" {
		t.Errorf("title = %v, want postgres", resp.Data["title"])
	}
}

// Cold start during a 1Password outage: reads must fail with the
// explicit replica-empty error (not a generic not-found), and cold
// start must keep retrying (paced by the per-vault governor backoff)
// until 1Password becomes reachable again.
func TestColdStart_DuringOutage_RetriesThenRecovers(t *testing.T) {
	b, storage := newTestBackend(t)

	fake := NewFakeOPClient()
	fake.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}
	fake.Items["v1"] = []onepassword.ItemOverview{{ID: "i1", Title: "postgres", State: onepassword.ItemStateActive}}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{"i1": {ID: "i1", Title: "postgres"}}
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }

	if _, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A settable clock on the governor lets the test step through
	// backoff windows instantly instead of sleeping.
	clock := &fakeClock{t: time.Now()}
	b.gate.now = clock.now

	// 1Password is unreachable for the first two cold-start attempts.
	fake.ItemsErr["v1"] = errors.New("network unreachable")

	b.coldStart(context.Background())
	resp, err := readItem(t, b, storage, "Infra", "postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("read during cold-start outage: resp=%+v, want an explicit error response", resp)
	}
	if errStr, _ := resp.Data["error"].(string); !strings.Contains(errStr, "cold start incomplete") {
		t.Errorf("error = %q, want it to mention cold start incomplete", errStr)
	}

	clock.advance(time.Hour) // clear this vault's backoff window
	b.coldStart(context.Background())
	if b.getOrCreateReplica("v1").itemCount() != 0 {
		t.Fatalf("replica populated despite the outage still being in effect")
	}

	// 1Password recovers.
	clock.advance(time.Hour)
	fake.ItemsErr["v1"] = nil
	b.coldStart(context.Background())

	resp2 := mustReadItem(t, b, storage, "Infra", "postgres")
	if resp2.Data["title"] != "postgres" {
		t.Errorf("title after recovery = %v, want postgres", resp2.Data["title"])
	}
}

// Regression test for the bug found in the 2026-08-05 bench gate run
// (step 7b, README "Bench gate record"): a plugin restart during a 1P
// outage must not permanently wedge the mount. Client construction
// (clientFactory) performs a live 1P auth handshake, so it can fail on
// its own — independently of loading config from storage — and must
// not prevent config from loading into runtime state.
func TestBackend_Initialize_ClientFactoryFails_ConfigStillLoadsAndStatusShowsIt(t *testing.T) {
	storage := &logical.InmemStorage{}

	fake := NewFakeOPClient()
	fake.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}
	fake.Items["v1"] = []onepassword.ItemOverview{{ID: "i1", Title: "postgres", State: onepassword.ItemStateActive}}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{"i1": {ID: "i1", Title: "postgres"}}

	// First "mount": write config normally (client factory succeeds).
	b1 := newBackend()
	conf1 := logical.TestBackendConfig()
	conf1.StorageView = storage
	if err := b1.Setup(context.Background(), conf1); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	b1.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }
	if _, err := writeConfig(t, b1, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A restart that lands mid-outage: the client factory's live auth
	// handshake fails every time.
	b2 := newBackend()
	conf2 := logical.TestBackendConfig()
	conf2.StorageView = storage
	if err := b2.Setup(context.Background(), conf2); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	b2.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return nil, errors.New("network unreachable")
	}

	if err := b2.Initialize(context.Background(), &logical.InitializationRequest{Storage: storage}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// (a) config loaded despite the client factory failing.
	if b2.currentConfig() == nil {
		t.Fatalf("Initialize did not load config from storage despite client factory failure — this is the bug: loadPersistedConfig must not depend on network success")
	}

	// (a) the failure is visible in status, not silently swallowed.
	resp := readStatus(t, b2, storage)
	gov, ok := resp.Data["governor"].(map[string]interface{})
	if !ok {
		t.Fatalf("governor missing or wrong type: %+v", resp.Data["governor"])
	}
	if failures, _ := gov["client_init_failures"].(int); failures < 1 {
		t.Errorf("governor.client_init_failures = %v, want >= 1", gov["client_init_failures"])
	}
	if errStr, _ := gov["client_init_last_err"].(string); !strings.Contains(errStr, "network unreachable") {
		t.Errorf("governor.client_init_last_err = %q, want it to mention the factory error", errStr)
	}

	// (a) reads fail with the explicit replica-empty error, not a hang
	// or a generic not-found.
	readResp, err := readItem(t, b2, storage, "Infra", "postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if readResp == nil || !readResp.IsError() {
		t.Fatalf("read during client-init failure: resp=%+v, want an explicit error response", readResp)
	}
	if errStr, _ := readResp.Data["error"].(string); !strings.Contains(errStr, "cold start incomplete") {
		t.Errorf("error = %q, want it to mention cold start incomplete", errStr)
	}
}

// (b): once the client factory starts succeeding, the SAME backend
// (never given a config rewrite) must recover on its own via the
// periodic()/coldStart() retry loop — proving the fix actually closes
// the gap the bug left open (7+ minutes stuck in the live bench,
// unblocked only by a manual op/config rewrite).
func TestBackend_ClientFactoryRecovers_NoConfigRewrite(t *testing.T) {
	storage := &logical.InmemStorage{}

	fake := NewFakeOPClient()
	fake.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}
	fake.Items["v1"] = []onepassword.ItemOverview{{ID: "i1", Title: "postgres", State: onepassword.ItemStateActive}}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{"i1": {ID: "i1", Title: "postgres"}}

	b1 := newBackend()
	conf1 := logical.TestBackendConfig()
	conf1.StorageView = storage
	if err := b1.Setup(context.Background(), conf1); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	b1.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return fake, nil }
	if _, err := writeConfig(t, b1, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A restart mid-outage, with a factory scripted to fail twice then
	// recover — like 1Password coming back up partway through the
	// retry loop.
	b2 := newBackend()
	conf2 := logical.TestBackendConfig()
	conf2.StorageView = storage
	if err := b2.Setup(context.Background(), conf2); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	attempts := 0
	b2.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("network unreachable")
		}
		return fake, nil
	}

	clock := &fakeClock{t: time.Now()}
	b2.gate.now = clock.now

	// Attempt 1 (inside Initialize's coldStart): fails.
	if err := b2.Initialize(context.Background(), &logical.InitializationRequest{Storage: storage}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after Initialize = %d, want 1", attempts)
	}
	if resp, _ := readItem(t, b2, storage, "Infra", "postgres"); resp == nil || !resp.IsError() {
		t.Fatalf("read after attempt 1: resp=%+v, want an explicit error response", resp)
	}

	// Attempt 2 (a later PeriodicFunc tick): fails.
	clock.advance(time.Hour) // clear clientInitState's backoff window
	req := &logical.Request{Storage: storage}
	if err := b2.periodic(context.Background(), req); err != nil {
		t.Fatalf("periodic (attempt 2): %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts after periodic #1 = %d, want 2", attempts)
	}
	if b2.getOrCreateReplica("v1").itemCount() != 0 {
		t.Fatalf("replica populated despite the client factory still failing")
	}

	// Attempt 3: the client factory recovers. No config rewrite ever
	// happens on b2 — recovery must be fully automatic.
	clock.advance(time.Hour)
	if err := b2.periodic(context.Background(), req); err != nil {
		t.Fatalf("periodic (attempt 3): %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts after periodic #2 = %d, want 3", attempts)
	}

	resp := mustReadItem(t, b2, storage, "Infra", "postgres")
	if resp.Data["title"] != "postgres" {
		t.Errorf("title after recovery = %v, want postgres", resp.Data["title"])
	}

	// The failure state clears once the client is up.
	statusResp := readStatus(t, b2, storage)
	gov := statusResp.Data["governor"].(map[string]interface{})
	if failures, _ := gov["client_init_failures"].(int); failures != 0 {
		t.Errorf("governor.client_init_failures after recovery = %v, want 0", gov["client_init_failures"])
	}
}

func TestPeriodic_SelfThrottlesToRefreshInterval(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"refresh_interval": "1h",
	})
	mustReadItem(t, b, storage, "Infra", "postgres") // warms the replica
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	req := &logical.Request{Storage: storage}
	if err := b.periodic(context.Background(), req); err != nil {
		t.Fatalf("periodic: %v", err)
	}
	if fake.ListItemsCalls != 0 {
		t.Errorf("periodic before refresh_interval elapsed: ListItemsCalls = %d, want 0", fake.ListItemsCalls)
	}

	r := b.getOrCreateReplica("v1")
	r.mu.Lock()
	r.lastCycle = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	if err := b.periodic(context.Background(), req); err != nil {
		t.Fatalf("periodic: %v", err)
	}
	if fake.ListItemsCalls != 1 {
		t.Errorf("periodic after refresh_interval elapsed: ListItemsCalls = %d, want 1", fake.ListItemsCalls)
	}
}

func TestPeriodic_NoConfig_NoOp(t *testing.T) {
	b, storage := newTestBackend(t)
	req := &logical.Request{Storage: storage}
	if err := b.periodic(context.Background(), req); err != nil {
		t.Fatalf("periodic with no config: %v", err)
	}
}
