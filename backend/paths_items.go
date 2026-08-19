package backend

import (
	"context"
	"sort"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathVaultItems defines `vaults/<vault>/items` (list): item titles +
// IDs + updatedAt from a vault's replica overview index (spec §3),
// through the same window/ceiling passthrough logic as an item read
// (ensureVaultWindowFresh) — just without a specific item's
// always_fresh/miss handling, since a listing can't be "unknown".
func pathVaultItems(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: `vaults/(?P<vault>[^/]+)/items/?`,

		Fields: map[string]*framework.FieldSchema{
			"vault": {
				Type:        framework.TypeString,
				Description: "1Password vault name or ID.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ListOperation: &framework.PathOperation{
				Callback: b.pathVaultItemsList,
				Summary:  "List items in a 1Password vault.",
			},
		},

		HelpSynopsis:    "List items in a 1Password vault.",
		HelpDescription: "Lists item titles, IDs, and updatedAt from the vault's replica overview index.",
	}
}

func (b *Backend) pathVaultItemsList(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	vaultAddr := data.Get("vault").(string)

	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	b.ensureVaultWindowFresh(ctx, vaultID)

	r := b.getOrCreateReplica(vaultID)
	overviews := r.overviewList()

	keys := make([]string, 0, len(overviews))
	keyInfo := make(map[string]interface{}, len(overviews))
	for _, ov := range overviews {
		keys = append(keys, ov.ID)
		keyInfo[ov.ID] = map[string]interface{}{
			"title":      ov.Title,
			"updated_at": ov.UpdatedAt,
		}
	}
	sort.Strings(keys)

	return logical.ListResponseWithInfo(keys, keyInfo), nil
}
