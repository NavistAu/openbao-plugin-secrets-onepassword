package backend

import (
	"context"
	"fmt"
	"sync"

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
