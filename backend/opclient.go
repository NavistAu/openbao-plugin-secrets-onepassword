package backend

import (
	"context"
	"errors"
	"fmt"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// integrationName identifies this plugin to 1Password (WithIntegrationInfo).
const integrationName = "openbao-plugin-secrets-onepassword"

// maxGetAllIDs is the SDK's hard per-call cap on Items().GetAll: >50
// IDs is a client-side validation error, at zero cost (plan "verified
// facts", spike-measured 2026-08-05).
const maxGetAllIDs = 50

// ItemResult is one entry of a GetItems response: either a fetched
// Item or a per-ID error, positionally correlated with the requested
// ids. onepassword.ItemsGetAllResponse.IndividualResponses carries no
// ID of its own, so the position in the slice is the only link back
// to the request; GetItems reconstructs that link from the chunk
// order it issued.
type ItemResult struct {
	ID   string
	Item *onepassword.Item
	Err  error
}

// OPClient is the seam between the backend and the 1Password SDK.
// Every code path in this repo goes through here rather than the SDK
// client directly, so tests can substitute a fake.
//
// GetItems is the ONLY way item bodies are fetched: it never calls
// Items().Get — spec D11: a single Get costs 2 reads, while GetAll
// costs 1 read per call (up to 50 IDs), so even a single-item fetch
// goes through GetAll.
type OPClient interface {
	ListVaults(ctx context.Context) ([]onepassword.VaultOverview, error)
	ListItems(ctx context.Context, vaultID string) ([]onepassword.ItemOverview, error)
	GetItems(ctx context.Context, vaultID string, ids []string) ([]ItemResult, error)
}

// sdkClient is the concrete OPClient wrapping the real 1Password SDK.
type sdkClient struct {
	client *onepassword.Client
}

var _ OPClient = (*sdkClient)(nil)

// NewSDKClient builds an OPClient backed by a real 1Password SDK
// client authenticated with a service-account token.
func NewSDKClient(ctx context.Context, serviceAccountToken, version string) (OPClient, error) {
	c, err := onepassword.NewClient(ctx,
		onepassword.WithServiceAccountToken(serviceAccountToken),
		onepassword.WithIntegrationInfo(integrationName, version),
	)
	if err != nil {
		return nil, err
	}
	return &sdkClient{client: c}, nil
}

func (s *sdkClient) ListVaults(ctx context.Context) ([]onepassword.VaultOverview, error) {
	return s.client.Vaults().List(ctx)
}

func (s *sdkClient) ListItems(ctx context.Context, vaultID string) ([]onepassword.ItemOverview, error) {
	return s.client.Items().List(ctx, vaultID)
}

// GetItems fetches item bodies for ids, chunking into ≤50-ID
// Items().GetAll calls (spec D11) and returning results in the same
// order as ids.
func (s *sdkClient) GetItems(ctx context.Context, vaultID string, ids []string) ([]ItemResult, error) {
	results := make([]ItemResult, 0, len(ids))
	for _, chunk := range chunkIDs(ids, maxGetAllIDs) {
		resp, err := s.client.Items().GetAll(ctx, vaultID, chunk)
		if err != nil {
			return nil, err
		}
		if len(resp.IndividualResponses) != len(chunk) {
			return nil, fmt.Errorf("1password: GetAll returned %d responses for %d requested ids", len(resp.IndividualResponses), len(chunk))
		}
		for i, r := range resp.IndividualResponses {
			id := chunk[i]
			switch {
			case r.Content != nil:
				item := *r.Content
				results = append(results, ItemResult{ID: id, Item: &item})
			case r.Error != nil:
				results = append(results, ItemResult{ID: id, Err: getAllError{id: id, err: *r.Error}})
			default:
				results = append(results, ItemResult{ID: id, Err: errEmptyGetAllResponse})
			}
		}
	}
	return results, nil
}

var errEmptyGetAllResponse = errors.New("1password: GetAll response entry had neither content nor error")

// getAllError wraps a per-entry onepassword.ItemsGetAllError with the
// requested item ID it applies to.
type getAllError struct {
	id  string
	err onepassword.ItemsGetAllError
}

func (e getAllError) Error() string {
	return fmt.Sprintf("1password: get item %q failed: %s", e.id, e.err.Type)
}

// chunkIDs splits ids into slices of at most size, preserving order.
// An empty input yields no chunks (zero calls, not one empty call).
func chunkIDs(ids []string, size int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	chunks := make([][]string, 0, (len(ids)+size-1)/size)
	for len(ids) > 0 {
		n := size
		if n > len(ids) {
			n = len(ids)
		}
		chunks = append(chunks, ids[:n])
		ids = ids[n:]
	}
	return chunks
}
