package backend

import (
	"context"
	"strings"
	"time"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathField defines `field/<vault>/<item...>/<field>` and its
// section-qualified 4-segment form `field/<vault>/<item...>/<section>/
// <field>` (spec §3) — a single field value, 1:1 with `op://` URIs.
func pathField(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: `field/(?P<vault>[^/]+)/(?P<rest>.+)`,

		Fields: map[string]*framework.FieldSchema{
			"vault": {
				Type:        framework.TypeString,
				Description: "1Password vault name or ID.",
			},
			"rest": {
				Type:        framework.TypeString,
				Description: "Item address followed by /<field> or /<section>/<field>.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathFieldRead,
				Summary:  "Read a single 1Password item field value.",
			},
		},

		HelpSynopsis:    "Read a single 1Password item field.",
		HelpDescription: "Reads one field's value — the op:// URI equivalent — through the spec §4 passthrough-first read path.",
	}
}

func (b *Backend) pathFieldRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	vaultAddr := data.Get("vault").(string)
	rest := data.Get("rest").(string)

	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	segs := strings.Split(rest, "/")
	r := b.getOrCreateReplica(vaultID)

	var section, fieldLabel string
	resolve := func() (string, error) {
		id, sec, fld, ferr := resolveFieldAddress(r, segs)
		section, fieldLabel = sec, fld
		return id, ferr
	}

	itemID, found, err := b.ensureItemFresh(ctx, vaultID, rest, resolve)
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
		return logical.ErrorResponse(errNotFound.Error()), nil
	}

	field, ferr := lookupField(item, section, fieldLabel)
	if ferr != nil {
		return logical.ErrorResponse(ferr.Error()), nil
	}

	respData := map[string]interface{}{
		"id":    field.ID,
		"title": field.Title,
		"type":  string(field.FieldType),
		"value": field.Value,
	}
	addReadMeta(respData, b.readMetaFor(vaultID, itemID, r, time.Now()))

	return &logical.Response{Data: respData}, nil
}

// resolveFieldAddress resolves a <item...>/<field> or
// <item...>/<section>/<field> address (spec §3). It tries the
// path_split (D13) greedy index first — its natural remaining
// segments already distinguish the unqualified (1 segment: field)
// from the section-qualified (2 segments: section, field) form; a
// bare vaultReplica with path_split unset (splitPaths nil) always
// misses here, falling through to flat parsing. Flat parsing treats
// the first segment as the item address (ID or raw title — never
// containing "/") and the remaining 1 or 2 segments as
// field[/section-field].
func resolveFieldAddress(r *vaultReplica, segs []string) (itemID, section, field string, err error) {
	if id, remaining, ok := r.resolveSplitPath(segs); ok {
		switch len(remaining) {
		case 1:
			return id, "", remaining[0], nil
		case 2:
			return id, remaining[0], remaining[1], nil
		default:
			return "", "", "", errNotFound
		}
	}

	switch len(segs) {
	case 2:
		id, ferr := r.resolveItemAddress(segs[0])
		if ferr != nil {
			return "", "", "", ferr
		}
		return id, "", segs[1], nil
	case 3:
		id, ferr := r.resolveItemAddress(segs[0])
		if ferr != nil {
			return "", "", "", ferr
		}
		return id, segs[1], segs[2], nil
	default:
		return "", "", "", errNotFound
	}
}

// lookupField finds item's field by label, optionally qualified by a
// section title (spec §3: "duplicate field labels ... a documented
// reason to prefer the section-qualified 4-segment form"). An
// unqualified lookup takes the first matching field in the SDK's own
// field order when multiple share a label — the SDK gives no other
// resolution signal (spec §3).
func lookupField(item onepassword.Item, section, field string) (*onepassword.ItemField, error) {
	var sectionID *string
	if section != "" {
		found := false
		for _, s := range item.Sections {
			if s.Title == section {
				id := s.ID
				sectionID = &id
				found = true
				break
			}
		}
		if !found {
			return nil, errNotFound
		}
	}

	for i := range item.Fields {
		f := &item.Fields[i]
		if f.Title != field {
			continue
		}
		if sectionID != nil {
			if f.SectionID == nil || *f.SectionID != *sectionID {
				continue
			}
		}
		return f, nil
	}
	return nil, errNotFound
}
