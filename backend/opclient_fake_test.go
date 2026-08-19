package backend

import (
	"context"
	"errors"
	"testing"

	onepassword "github.com/1password/onepassword-sdk-go"
)

func TestFakeOPClient_ListVaults(t *testing.T) {
	f := NewFakeOPClient()
	f.Vaults = []onepassword.VaultOverview{{ID: "v1", Title: "Infra"}}

	got, err := f.ListVaults(context.Background())
	if err != nil {
		t.Fatalf("ListVaults: unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v1" {
		t.Errorf("ListVaults = %+v, want [{ID: v1}]", got)
	}
	if f.ListVaultsCalls != 1 {
		t.Errorf("ListVaultsCalls = %d, want 1", f.ListVaultsCalls)
	}

	// A second call increments the counter again.
	if _, err := f.ListVaults(context.Background()); err != nil {
		t.Fatalf("ListVaults (2nd): unexpected error: %v", err)
	}
	if f.ListVaultsCalls != 2 {
		t.Errorf("ListVaultsCalls after 2 calls = %d, want 2", f.ListVaultsCalls)
	}
}

func TestFakeOPClient_ListVaultsError(t *testing.T) {
	f := NewFakeOPClient()
	wantErr := errors.New("boom")
	f.VaultsErr = wantErr

	_, err := f.ListVaults(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("ListVaults error = %v, want %v", err, wantErr)
	}
	if f.ListVaultsCalls != 1 {
		t.Errorf("ListVaultsCalls = %d, want 1 (a failed call still counts as a request)", f.ListVaultsCalls)
	}
}

func TestFakeOPClient_ListItems(t *testing.T) {
	f := NewFakeOPClient()
	f.Items["v1"] = []onepassword.ItemOverview{{ID: "i1", Title: "postgres"}}

	got, err := f.ListItems(context.Background(), "v1")
	if err != nil {
		t.Fatalf("ListItems: unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "i1" {
		t.Errorf("ListItems = %+v, want [{ID: i1}]", got)
	}
	if f.ListItemsCalls != 1 {
		t.Errorf("ListItemsCalls = %d, want 1", f.ListItemsCalls)
	}

	// A vault with no scripted items returns an empty, not nil-error, list.
	got, err = f.ListItems(context.Background(), "v-unscripted")
	if err != nil {
		t.Fatalf("ListItems(unscripted vault): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListItems(unscripted vault) = %+v, want empty", got)
	}
}

func TestFakeOPClient_ListItemsError(t *testing.T) {
	f := NewFakeOPClient()
	wantErr := errors.New("rate limited")
	f.ItemsErr["v1"] = wantErr

	_, err := f.ListItems(context.Background(), "v1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("ListItems error = %v, want %v", err, wantErr)
	}
}

func TestFakeOPClient_GetItems(t *testing.T) {
	f := NewFakeOPClient()
	f.ItemBodies["v1"] = map[string]onepassword.Item{
		"i1": {ID: "i1", Title: "postgres"},
		"i2": {ID: "i2", Title: "redis"},
	}

	results, err := f.GetItems(context.Background(), "v1", []string{"i1", "i2", "i-missing"})
	if err != nil {
		t.Fatalf("GetItems: unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("GetItems = %d results, want 3", len(results))
	}
	if results[0].ID != "i1" || results[0].Item == nil || results[0].Item.Title != "postgres" {
		t.Errorf("results[0] = %+v, want i1/postgres", results[0])
	}
	if results[1].ID != "i2" || results[1].Item == nil || results[1].Item.Title != "redis" {
		t.Errorf("results[1] = %+v, want i2/redis", results[1])
	}
	if results[2].ID != "i-missing" || results[2].Item != nil || results[2].Err == nil {
		t.Errorf("results[2] = %+v, want a not-found error and no item", results[2])
	}

	// One request budget unit was spent (single chunk, well under the cap).
	if f.GetItemsCalls != 1 {
		t.Errorf("GetItemsCalls = %d, want 1", f.GetItemsCalls)
	}
}

func TestFakeOPClient_GetItems_ChunkBudget(t *testing.T) {
	f := NewFakeOPClient()
	f.ItemBodies["v1"] = map[string]onepassword.Item{}
	ids := idsN(184)
	for _, id := range ids {
		f.ItemBodies["v1"][id] = onepassword.Item{ID: id}
	}

	results, err := f.GetItems(context.Background(), "v1", ids)
	if err != nil {
		t.Fatalf("GetItems: unexpected error: %v", err)
	}
	if len(results) != 184 {
		t.Fatalf("GetItems = %d results, want 184", len(results))
	}
	// 184 ids at the 50-ID cap = ceil(184/50) = 4 request-budget units,
	// regardless of GetItems() being called exactly once — this is the
	// request-budget accounting the replica/cycle logic (Task 4) relies on.
	if f.GetItemsCalls != 4 {
		t.Errorf("GetItemsCalls = %d, want 4", f.GetItemsCalls)
	}
}

func TestFakeOPClient_GetItems_CustomChunkSize(t *testing.T) {
	f := NewFakeOPClient()
	f.GetItemsChunkSize = 2
	f.ItemBodies["v1"] = map[string]onepassword.Item{
		"i1": {ID: "i1"}, "i2": {ID: "i2"}, "i3": {ID: "i3"},
	}

	if _, err := f.GetItems(context.Background(), "v1", []string{"i1", "i2", "i3"}); err != nil {
		t.Fatalf("GetItems: unexpected error: %v", err)
	}
	if f.GetItemsCalls != 2 {
		t.Errorf("GetItemsCalls = %d, want 2 (chunks of 2 for 3 ids)", f.GetItemsCalls)
	}
}

func TestFakeOPClient_GetItemsError(t *testing.T) {
	f := NewFakeOPClient()
	wantErr := errors.New("auth failed")
	f.GetItemsErr["v1"] = wantErr

	_, err := f.GetItems(context.Background(), "v1", []string{"i1"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("GetItems error = %v, want %v", err, wantErr)
	}
	if f.GetItemsCalls != 1 {
		t.Errorf("GetItemsCalls = %d, want 1 (the failed attempt still counts)", f.GetItemsCalls)
	}
}

func TestFakeOPClient_GetItems_Empty(t *testing.T) {
	f := NewFakeOPClient()

	results, err := f.GetItems(context.Background(), "v1", nil)
	if err != nil {
		t.Fatalf("GetItems(empty ids): unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("GetItems(empty ids) = %+v, want empty", results)
	}
	if f.GetItemsCalls != 0 {
		t.Errorf("GetItemsCalls = %d, want 0 (no ids means no request)", f.GetItemsCalls)
	}
}
