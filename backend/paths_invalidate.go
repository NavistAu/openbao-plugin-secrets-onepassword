package backend

import (
	"context"
	"sort"
	"time"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathInvalidate defines `invalidate` (update): the D15 zero-spend
// invalidation across every allowlisted vault — expires freshness
// windows and clears negative caches without issuing a single 1P
// request; the refetch cost lands lazily on whatever a later read (or
// scheduled cycle) actually touches.
func pathInvalidate(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: "invalidate/?",

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathInvalidateAllWrite,
				Summary:  "Zero-spend invalidation of every allowlisted vault's freshness window.",
			},
		},

		HelpSynopsis:    "Invalidate all allowlisted vaults' freshness windows.",
		HelpDescription: "Expires freshness windows and clears negative caches (spec D15) with zero 1Password requests.",
	}
}

// pathInvalidateVault defines `invalidate/<vault>` (update): the
// scoped form — same ACL shape as refresh/<vault>.
func pathInvalidateVault(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: `invalidate/(?P<vault>[^/]+)`,

		Fields: map[string]*framework.FieldSchema{
			"vault": {
				Type:        framework.TypeString,
				Description: "1Password vault name or ID.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathInvalidateVaultWrite,
				Summary:  "Zero-spend invalidation of one vault's freshness window.",
			},
		},

		HelpSynopsis:    "Invalidate one vault's freshness window.",
		HelpDescription: "Expires the vault's freshness window and clears its negative cache with zero 1Password requests.",
	}
}

func (b *Backend) pathInvalidateAllWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.mu.RLock()
	ids := make([]string, 0, len(b.allowlistIDs))
	for id := range b.allowlistIDs {
		ids = append(ids, id)
	}
	b.mu.RUnlock()
	sort.Strings(ids)

	now := time.Now()
	for _, id := range ids {
		b.getOrCreateReplica(id).invalidate(now)
	}
	return &logical.Response{Data: map[string]interface{}{"invalidated_vaults": ids}}, nil
}

func (b *Backend) pathInvalidateVaultWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	vaultAddr := data.Get("vault").(string)
	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	b.getOrCreateReplica(vaultID).invalidate(time.Now())
	return nil, nil
}
