package backend

import (
	"context"
	"sort"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathRefresh defines `refresh` (update): spend-now delta cycle across
// every allowlisted vault (spec §3). Manual refresh is treated as the
// workClassManual priority class — gate-checked like any cycle, but
// exempt from the burst brake's deferral of periodic/eager work (spec
// §4: "manual refreshes still serve up to the cap").
func pathRefresh(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: "refresh/?",

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathRefreshAllWrite,
				Summary:  "Run an immediate delta refresh across all allowlisted vaults.",
			},
		},

		HelpSynopsis:    "Refresh all allowlisted vaults now.",
		HelpDescription: "Spend-now freshness: runs the spec §4 delta cycle for every allowlisted vault immediately, gate-checked.",
	}
}

// pathRefreshVault defines `refresh/<vault>` (update): scoped refresh
// of one vault — the form deploy tasks and post-edit workflows use, so
// a refresh identity's budget-spend is ACL-bounded to its vault.
func pathRefreshVault(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: `refresh/(?P<vault>[^/]+)`,

		Fields: map[string]*framework.FieldSchema{
			"vault": {
				Type:        framework.TypeString,
				Description: "1Password vault name or ID.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathRefreshVaultWrite,
				Summary:  "Run an immediate delta refresh for one vault.",
			},
		},

		HelpSynopsis:    "Refresh one vault now.",
		HelpDescription: "Spend-now freshness for a single vault, gate-checked.",
	}
}

func (b *Backend) pathRefreshAllWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.mu.RLock()
	ids := make([]string, 0, len(b.allowlistIDs))
	for id := range b.allowlistIDs {
		ids = append(ids, id)
	}
	b.mu.RUnlock()
	sort.Strings(ids)

	results := make(map[string]interface{}, len(ids))
	for _, id := range ids {
		if err := b.runVaultCycle(ctx, id, workClassManual); err != nil {
			results[id] = err.Error()
		} else {
			results[id] = "ok"
		}
	}
	return &logical.Response{Data: map[string]interface{}{"results": results}}, nil
}

func (b *Backend) pathRefreshVaultWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	vaultAddr := data.Get("vault").(string)
	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	if cerr := b.runVaultCycle(ctx, vaultID, workClassManual); cerr != nil {
		return logical.ErrorResponse(cerr.Error()), nil
	}
	return nil, nil
}
