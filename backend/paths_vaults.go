package backend

import (
	"context"
	"sort"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathVaults defines `vaults` (list): the allowlisted vaults present
// in the replica (spec §3). Served entirely from the cached vault
// directory (refreshVaultDirectory, built at cold start — Task 7) —
// zero 1Password requests. Keyed by vault ID (always unambiguous,
// spec §3 Addressing); each key's info carries the vault's title.
func pathVaults(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: "vaults/?",

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ListOperation: &framework.PathOperation{
				Callback: b.pathVaultsList,
				Summary:  "List allowlisted 1Password vaults.",
			},
		},

		HelpSynopsis:    "List allowlisted 1Password vaults.",
		HelpDescription: "Lists the vaults config.vaults resolves to, from the cached vault directory.",
	}
}

func (b *Backend) pathVaultsList(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	keys := make([]string, 0, len(b.allowlistIDs))
	keyInfo := make(map[string]interface{}, len(b.allowlistIDs))
	for id := range b.allowlistIDs {
		keys = append(keys, id)
		info := map[string]interface{}{}
		if ov, ok := b.vaultDirectory[id]; ok {
			info["title"] = ov.Title
			info["active_item_count"] = ov.ActiveItemCount
		}
		keyInfo[id] = info
	}
	sort.Strings(keys)

	return logical.ListResponseWithInfo(keys, keyInfo), nil
}
