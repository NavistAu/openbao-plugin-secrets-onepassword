package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// seedVault scripts n active items (ids "i0".."i(n-1)") with distinct
// UpdatedAt timestamps into a FakeOPClient, vault "v1".
func seedVault(f *FakeOPClient, n int) {
	f.Items["v1"] = nil
	f.ItemBodies["v1"] = map[string]onepassword.Item{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("i%d", i)
		ov := onepassword.ItemOverview{
			ID:        id,
			Title:     id,
			State:     onepassword.ItemStateActive,
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}
		f.Items["v1"] = append(f.Items["v1"], ov)
		f.ItemBodies["v1"][id] = onepassword.Item{ID: id, Title: id}
	}
}

func TestCycle_ColdStart184(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 184)
	v := newVaultReplica("v1")

	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if f.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls = %d, want 1", f.ListItemsCalls)
	}
	if f.GetItemsCalls != 4 {
		t.Errorf("GetItemsCalls = %d, want 4 (ceil(184/50))", f.GetItemsCalls)
	}
	if len(v.bodies) != 184 || len(v.overview) != 184 {
		t.Errorf("replica has %d bodies / %d overview entries, want 184/184", len(v.bodies), len(v.overview))
	}
}

func TestCycle_Unchanged(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 184)
	v := newVaultReplica("v1")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("initial runCycle: %v", err)
	}

	f.ListItemsCalls, f.GetItemsCalls = 0, 0
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("second runCycle: %v", err)
	}
	if f.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls (unchanged cycle) = %d, want 1", f.ListItemsCalls)
	}
	if f.GetItemsCalls != 0 {
		t.Errorf("GetItemsCalls (unchanged cycle) = %d, want 0", f.GetItemsCalls)
	}
}

func TestCycle_60Changed(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 184)
	v := newVaultReplica("v1")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("initial runCycle: %v", err)
	}

	// Bump UpdatedAt (and body content) for 60 of the 184 items.
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("i%d", i)
		for idx, ov := range f.Items["v1"] {
			if ov.ID == id {
				ov.UpdatedAt = ov.UpdatedAt.Add(time.Hour)
				f.Items["v1"][idx] = ov
			}
		}
		f.ItemBodies["v1"][id] = onepassword.Item{ID: id, Title: id + "-changed"}
	}

	f.ListItemsCalls, f.GetItemsCalls = 0, 0
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("second runCycle: %v", err)
	}
	if f.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls (60 changed) = %d, want 1", f.ListItemsCalls)
	}
	if f.GetItemsCalls != 2 {
		t.Errorf("GetItemsCalls (60 changed) = %d, want 2 (ceil(60/50))", f.GetItemsCalls)
	}
	if v.bodies["i0"].Title != "i0-changed" {
		t.Errorf("changed item i0 body not updated: %+v", v.bodies["i0"])
	}
}

func TestCycle_ArchivedPurgesBody(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 3)
	v := newVaultReplica("v1")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("initial runCycle: %v", err)
	}
	if _, ok := v.bodies["i1"]; !ok {
		t.Fatalf("i1 body missing after initial cycle")
	}

	for idx, ov := range f.Items["v1"] {
		if ov.ID == "i1" {
			ov.State = onepassword.ItemStateArchived
			f.Items["v1"][idx] = ov
		}
	}

	f.ListItemsCalls, f.GetItemsCalls = 0, 0
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("second runCycle: %v", err)
	}
	if _, ok := v.bodies["i1"]; ok {
		t.Errorf("archived item i1 body should be purged")
	}
	if _, ok := v.overview["i1"]; ok {
		t.Errorf("archived item i1 should be removed from the overview index")
	}
	if f.GetItemsCalls != 0 {
		t.Errorf("GetItemsCalls = %d, want 0 (archived items are never fetched)", f.GetItemsCalls)
	}
	if len(v.bodies) != 2 {
		t.Errorf("remaining bodies = %d, want 2", len(v.bodies))
	}
}

func TestCycle_ConcurrentSingleFlight(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 184)
	v := newVaultReplica("v1")

	gate := make(chan struct{})
	started := make(chan struct{})
	var closeOnce sync.Once
	client := &blockingOPClient{
		FakeOPClient: f,
		onListItems: func() {
			closeOnce.Do(func() { close(started) })
			<-gate
		},
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = runCycle(context.Background(), v, client, allowAllGate{}, nil, workClassPeriodic)
	}()

	<-started // the leader is now blocked inside ListItems

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = runCycle(context.Background(), v, client, allowAllGate{}, nil, workClassPeriodic)
		}(i)
	}
	// Give followers a chance to reach the "join as follower" point
	// before releasing the leader — best-effort, correctness does not
	// depend on this actually being enough time, only the request
	// budget assertion below does, and that only holds if followers
	// really did coalesce rather than each running their own cycle.
	time.Sleep(50 * time.Millisecond)
	close(gate)

	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: unexpected error %v", i, err)
		}
	}
	if f.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls = %d, want 1 (10 concurrent triggers coalesce to one cycle)", f.ListItemsCalls)
	}
	if f.GetItemsCalls != 4 {
		t.Errorf("GetItemsCalls = %d, want 4 (ceil(184/50), one cycle's worth)", f.GetItemsCalls)
	}
}

// blockingOPClient wraps a FakeOPClient to let a test control exactly
// when ListItems proceeds, without needing latency support in the
// (already-frozen, Task 2) FakeOPClient itself.
type blockingOPClient struct {
	*FakeOPClient
	onListItems func()
}

func (c *blockingOPClient) ListItems(ctx context.Context, vaultID string) ([]onepassword.ItemOverview, error) {
	if c.onListItems != nil {
		c.onListItems()
	}
	return c.FakeOPClient.ListItems(ctx, vaultID)
}

func TestCycle_GateDenied(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 3)
	v := newVaultReplica("v1")

	err := runCycle(context.Background(), v, f, denyGate{}, nil, workClassPeriodic)
	if !errors.Is(err, errGateDenied) {
		t.Fatalf("runCycle with denyGate error = %v, want errGateDenied", err)
	}
	if f.ListItemsCalls != 0 || f.GetItemsCalls != 0 {
		t.Errorf("ListItemsCalls=%d GetItemsCalls=%d, want 0/0 (gate denied before any request)", f.ListItemsCalls, f.GetItemsCalls)
	}
}

type denyGate struct{}

func (denyGate) allow(vaultID string, class workClass) bool         { return false }
func (denyGate) recordRequest(vaultID string, units int, err error) {}
func (denyGate) recordSuccess(vaultID string)                       {}

func TestCycle_ListErrorLeavesReplicaUntouched(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 3)
	v := newVaultReplica("v1")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("initial runCycle: %v", err)
	}
	bodiesBefore := len(v.bodies)

	f.ItemsErr["v1"] = errors.New("rate limited")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err == nil {
		t.Fatalf("runCycle with ListItems error: want an error, got nil")
	}
	if v.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", v.consecutiveFailures)
	}
	if len(v.bodies) != bodiesBefore {
		t.Errorf("replica bodies changed after a failed cycle: %d, want %d", len(v.bodies), bodiesBefore)
	}
}

func TestCycle_GetItemsErrorLeavesReplicaUntouched(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 3)
	v := newVaultReplica("v1")
	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("initial runCycle: %v", err)
	}
	bodiesBefore := len(v.bodies)

	// Force a change so GetItems is actually invoked, then fail it.
	ov := f.Items["v1"][0]
	ov.UpdatedAt = ov.UpdatedAt.Add(time.Hour)
	f.Items["v1"][0] = ov
	f.GetItemsErr["v1"] = errors.New("internal error")

	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err == nil {
		t.Fatalf("runCycle with GetItems error: want an error, got nil")
	}
	if v.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", v.consecutiveFailures)
	}
	if len(v.bodies) != bodiesBefore {
		t.Errorf("replica bodies changed after a failed cycle: %d, want %d", len(v.bodies), bodiesBefore)
	}
}

func TestCycle_ClearsNegativeCacheAndStaleSuspectOnSuccess(t *testing.T) {
	f := NewFakeOPClient()
	seedVault(f, 3)
	v := newVaultReplica("v1")
	now := time.Now()
	v.negativeCacheStore("missing", now)
	v.staleSuspect = true
	v.invalidatedAt = now

	if err := runCycle(context.Background(), v, f, allowAllGate{}, nil, workClassPeriodic); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if v.negativeCacheHit("missing", time.Hour, now) {
		t.Errorf("negative cache entry survived a successful cycle")
	}
	if v.staleSuspect {
		t.Errorf("staleSuspect not cleared by a successful cycle")
	}
	if !v.invalidatedAt.IsZero() {
		t.Errorf("invalidatedAt not cleared by a successful cycle, got %v", v.invalidatedAt)
	}
}
