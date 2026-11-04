package backend

import (
	"context"
	"fmt"
	"sync"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// FakeOPClient is a scriptable, in-memory OPClient for tests. Callers
// script per-vault responses (including errors) and read back
// per-method call counts to assert request budgets — the load-bearing
// test class for the spec §4 economy (e.g. "cold start of a 184-item
// vault costs exactly 1 ListVaults + 1 ListItems + 4 GetAll-chunks").
//
// GetItems mirrors the concrete client's own 50-ID chunking (using
// the same chunkIDs helper) so its call counter reflects the same
// request-budget unit a real GetAll call would cost, regardless of
// how many items a single GetItems invocation asks for.
type FakeOPClient struct {
	mu sync.Mutex

	// Vaults is returned by ListVaults, unless VaultsErr is set.
	Vaults    []onepassword.VaultOverview
	VaultsErr error

	// Items, keyed by vault ID, backs ListItems. ItemsErr, keyed by
	// vault ID, if set, makes ListItems for that vault fail instead.
	Items    map[string][]onepassword.ItemOverview
	ItemsErr map[string]error

	// ItemBodies, keyed by vault ID then item ID, backs GetItems.
	// An id absent from the inner map produces a per-entry not-found
	// ItemResult, mirroring a real itemNotFound response.
	ItemBodies map[string]map[string]onepassword.Item

	// GetItemsErr, keyed by vault ID, if set, fails the first chunk
	// call issued against that vault (a whole-call error, as GetAll
	// itself can return, distinct from a per-entry error).
	GetItemsErr map[string]error

	// GetItemsChunkSize overrides maxGetAllIDs for tests that need to
	// observe chunk boundaries at a manageable scale. Zero means use
	// maxGetAllIDs.
	GetItemsChunkSize int

	// Write-side scripting and call records. The fake mutates Items and
	// ItemBodies like 1Password would (new ID, version bump, removal),
	// so tests can then make them "lag" by resetting those maps.
	CreateErr   error
	PutErr      error
	DeleteErr   error
	CreateCalls []onepassword.ItemCreateParams
	PutCalls    []onepassword.Item
	// DeleteCalls holds "vaultID/itemID" strings.
	DeleteCalls []string
	nextID      int

	ListVaultsCalls int
	ListItemsCalls  int
	// GetItemsCalls counts chunk-level GetAll-equivalent calls, not
	// GetItems() invocations — the request-budget unit.
	GetItemsCalls int
}

var _ OPClient = (*FakeOPClient)(nil)

// NewFakeOPClient returns a FakeOPClient with its maps initialized.
func NewFakeOPClient() *FakeOPClient {
	return &FakeOPClient{
		Items:       map[string][]onepassword.ItemOverview{},
		ItemsErr:    map[string]error{},
		ItemBodies:  map[string]map[string]onepassword.Item{},
		GetItemsErr: map[string]error{},
	}
}

func (f *FakeOPClient) ListVaults(ctx context.Context) ([]onepassword.VaultOverview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListVaultsCalls++
	if f.VaultsErr != nil {
		return nil, f.VaultsErr
	}
	return f.Vaults, nil
}

func (f *FakeOPClient) ListItems(ctx context.Context, vaultID string) ([]onepassword.ItemOverview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListItemsCalls++
	if err := f.ItemsErr[vaultID]; err != nil {
		return nil, err
	}
	return f.Items[vaultID], nil
}

func (f *FakeOPClient) GetItems(ctx context.Context, vaultID string, ids []string) ([]ItemResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	chunkSize := f.GetItemsChunkSize
	if chunkSize <= 0 {
		chunkSize = maxGetAllIDs
	}

	bodies := f.ItemBodies[vaultID]
	results := make([]ItemResult, 0, len(ids))
	for _, chunk := range chunkIDs(ids, chunkSize) {
		f.GetItemsCalls++
		if err := f.GetItemsErr[vaultID]; err != nil {
			return nil, err
		}
		for _, id := range chunk {
			if item, ok := bodies[id]; ok {
				item := item
				results = append(results, ItemResult{ID: id, Item: &item})
				continue
			}
			results = append(results, ItemResult{ID: id, Err: fmt.Errorf("1password: item %q not found", id)})
		}
	}
	return results, nil
}

func (f *FakeOPClient) CreateItem(ctx context.Context, params onepassword.ItemCreateParams) (onepassword.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls = append(f.CreateCalls, params)
	if f.CreateErr != nil {
		return onepassword.Item{}, f.CreateErr
	}
	f.nextID++
	now := time.Now().UTC()
	item := onepassword.Item{
		ID: fmt.Sprintf("new%d", f.nextID), Title: params.Title, Category: params.Category, VaultID: params.VaultID,
		Fields: params.Fields, Sections: params.Sections, Tags: params.Tags, Websites: params.Websites,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if params.Notes != nil {
		item.Notes = *params.Notes
	}
	f.storeLocked(item)
	return item, nil
}

func (f *FakeOPClient) PutItem(ctx context.Context, item onepassword.Item) (onepassword.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PutCalls = append(f.PutCalls, item)
	if f.PutErr != nil {
		return onepassword.Item{}, f.PutErr
	}
	cur, ok := f.ItemBodies[item.VaultID][item.ID]
	if !ok {
		return onepassword.Item{}, fmt.Errorf("1password: item %q not found", item.ID)
	}
	item.Version = cur.Version + 1
	item.UpdatedAt = time.Now().UTC()
	if !item.UpdatedAt.After(cur.UpdatedAt) {
		item.UpdatedAt = cur.UpdatedAt.Add(time.Second)
	}
	f.storeLocked(item)
	return item, nil
}

func (f *FakeOPClient) DeleteItem(ctx context.Context, vaultID, itemID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCalls = append(f.DeleteCalls, vaultID+"/"+itemID)
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	if _, ok := f.ItemBodies[vaultID][itemID]; !ok {
		return fmt.Errorf("1password: item %q not found", itemID)
	}
	delete(f.ItemBodies[vaultID], itemID)
	kept := f.Items[vaultID][:0:0]
	for _, ov := range f.Items[vaultID] {
		if ov.ID != itemID {
			kept = append(kept, ov)
		}
	}
	f.Items[vaultID] = kept
	return nil
}

// storeLocked upserts item's body and overview. Caller holds f.mu.
func (f *FakeOPClient) storeLocked(item onepassword.Item) {
	if f.ItemBodies[item.VaultID] == nil {
		f.ItemBodies[item.VaultID] = map[string]onepassword.Item{}
	}
	f.ItemBodies[item.VaultID][item.ID] = item
	ov := onepassword.ItemOverview{ID: item.ID, Title: item.Title, Category: item.Category, VaultID: item.VaultID,
		Websites: item.Websites, Tags: item.Tags, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, State: onepassword.ItemStateActive}
	list := append([]onepassword.ItemOverview(nil), f.Items[item.VaultID]...)
	for i := range list {
		if list[i].ID == item.ID {
			list[i] = ov
			f.Items[item.VaultID] = list
			return
		}
	}
	f.Items[item.VaultID] = append(list, ov)
}
