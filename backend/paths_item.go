package backend

import (
	"context"
	"errors"
	"strings"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathItem defines `item/<vault>/<item...>` (spec §3): a full item
// read, including concealed fields, sections, notes, and tags. The
// item segment is a greedy wildcard so a path_split (D13) multi
// -segment title address parses correctly (spec §3 Addressing).
func pathItem(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: `item/(?P<vault>[^/]+)/(?P<item>.+)`,

		Fields: map[string]*framework.FieldSchema{
			"vault": {
				Type:        framework.TypeString,
				Description: "1Password vault name or ID.",
			},
			"item": {
				Type:        framework.TypeString,
				Description: "1Password item ID, exact title, or path_split (D13) address.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathItemRead,
				Summary:  "Read a 1Password item, including concealed fields, sections, notes, and tags.",
			},
		},

		HelpSynopsis:    "Read a 1Password item.",
		HelpDescription: "Reads a full 1Password item (fields, sections, notes, tags) through the spec §4 passthrough-first read path.",
	}
}

func (b *Backend) pathItemRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	vaultAddr := data.Get("vault").(string)
	itemAddr := data.Get("item").(string)

	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	segs := strings.Split(itemAddr, "/")
	r := b.getOrCreateReplica(vaultID)
	resolve := func() (string, error) { return resolveItemFromSegments(r, segs) }

	itemID, found, err := b.ensureItemFresh(ctx, vaultID, itemAddr, resolve)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	if !found {
		return logical.ErrorResponse(errNotFound.Error()), nil
	}

	cfg := b.currentConfig()
	if cfg != nil {
		if serr := checkServeStaleAllowed(cfg, r, time.Now()); serr != nil {
			return logical.ErrorResponse(serr.Error()), nil
		}
	}

	item, ok := r.body(itemID)
	if !ok {
		// Resolved in the index but the body is missing — shouldn't
		// happen (doCycle always populates both together), but fail
		// closed rather than serve a half response.
		return logical.ErrorResponse(errNotFound.Error()), nil
	}

	respData := itemResponseData(item)
	addReadMeta(respData, b.readMetaFor(vaultID, itemID, r, time.Now()))

	return &logical.Response{Data: respData}, nil
}

// resolveItemFromSegments resolves an item/<vault>/<item...> address
// (spec §3 Addressing): the ID or exact raw title when segs is a
// single segment, or — when path_split (D13) is configured — the
// split-path form fully consuming segs (no leftover segments; a
// leftover would mean a field/section address, which item/ doesn't
// serve — that's what field/ is for).
func resolveItemFromSegments(r *vaultReplica, segs []string) (itemID string, err error) {
	if len(segs) == 1 {
		id, ferr := r.resolveItemAddress(segs[0])
		if ferr == nil {
			return id, nil
		}
		if errors.Is(ferr, errAmbiguousTitle) {
			return "", ferr
		}
		// Not found via the flat lookup; fall through to split-path.
	}
	if id, remaining, ok := r.resolveSplitPath(segs); ok && len(remaining) == 0 {
		return id, nil
	}
	return "", errNotFound
}

// itemResponseData converts a full 1Password Item into the op/item
// read response body (spec §3: "fields (concealed included),
// sections, notes, and tags").
func itemResponseData(item onepassword.Item) map[string]interface{} {
	fields := make([]map[string]interface{}, len(item.Fields))
	for i, f := range item.Fields {
		fields[i] = map[string]interface{}{
			"id":         f.ID,
			"title":      f.Title,
			"type":       string(f.FieldType),
			"value":      f.Value,
			"section_id": f.SectionID,
		}
	}
	sections := make([]map[string]interface{}, len(item.Sections))
	for i, s := range item.Sections {
		sections[i] = map[string]interface{}{
			"id":    s.ID,
			"title": s.Title,
		}
	}
	return map[string]interface{}{
		"id":         item.ID,
		"title":      item.Title,
		"category":   string(item.Category),
		"vault_id":   item.VaultID,
		"fields":     fields,
		"sections":   sections,
		"notes":      item.Notes,
		"tags":       item.Tags,
		"version":    item.Version,
		"created_at": item.CreatedAt,
		"updated_at": item.UpdatedAt,
	}
}

// addReadMeta adds the spec §4 "Staleness metadata" fields
// (replica_age_seconds, the item's updatedAt, stale, stale_suspect)
// to a read response body. updated_at is set here too (redundant with
// itemResponseData's own updated_at for whole-item reads, but this is
// the single field/item reads share via readMetaFor's ItemUpdatedAt).
func addReadMeta(data map[string]interface{}, meta readMeta) {
	data["replica_age_seconds"] = meta.ReplicaAgeSeconds
	data["updated_at"] = meta.ItemUpdatedAt
	data["stale"] = meta.Stale
	data["stale_suspect"] = meta.StaleSuspect
}
