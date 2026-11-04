package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// Item writes (plan 2026-11-04-item-writes). Every write goes through
// beginWrite (allowlist + governor), spends from the shared budget via
// gate.recordRequest, and folds the SDK's returned item into the
// replica. Field values are never logged or returned.

var itemIDShape = regexp.MustCompile(`^[a-z0-9]{26}$`)

// writeCtx is what beginWrite hands the operation handlers.
type writeCtx struct {
	vaultID string
	client  OPClient
	r       *vaultReplica
	rx      *regexp.Regexp
}

// isWriteVault reports whether vaultID is in write_vaults (by ID or title).
func (b *Backend) isWriteVault(vaultID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.config == nil {
		return false
	}
	title := b.vaultDirectory[vaultID].Title
	for _, w := range b.config.WriteVaults {
		if w == vaultID || (title != "" && w == title) {
			return true
		}
	}
	return false
}

// beginWrite resolves the vault, enforces write_vaults, and asks the
// governor. Exactly one of (wc, resp, err) is meaningful: a deferred
// write fails with the reason, retryable when it is a rate limit.
func (b *Backend) beginWrite(ctx context.Context, vaultAddr string) (*writeCtx, *logical.Response, error) {
	if b.currentConfig() == nil {
		return nil, logical.ErrorResponse(errNoClient.Error()), nil
	}
	vaultID, err := b.resolveVaultAddress(vaultAddr)
	if err != nil {
		return nil, logical.ErrorResponse(err.Error()), nil
	}
	if !b.isWriteVault(vaultID) {
		return nil, logical.ErrorResponse("op: vault %q is not writable: it is not in write_vaults", vaultAddr), nil
	}
	if !b.gate.allow(vaultID, workClassManual) {
		snap := b.gate.snapshot()
		if snap.State == govStateAuthFailed.String() {
			return nil, logical.ErrorResponse("op: write refused: 1Password auth failed; rewrite op/config with a valid token"), nil
		}
		return nil, nil, fmt.Errorf("%w: op: write deferred by the rate governor (state %s, hourly usage %d%%, vault backoff %d); retry later",
			logical.ErrRateLimitQuotaExceeded, snap.State, snap.HourlyUsagePct, snap.Backoff[vaultID])
	}
	client, err := b.ensureClient(ctx)
	if err != nil {
		return nil, logical.ErrorResponse(err.Error()), nil
	}
	b.mu.RLock()
	rx := b.pathSplitRegex
	b.mu.RUnlock()
	return &writeCtx{vaultID: vaultID, client: client, r: b.getOrCreateReplica(vaultID), rx: rx}, nil, nil
}

// sdkErr reports a failed SDK call to the governor and maps it to a
// response: 429 retryable for rate limits, an error response otherwise.
func (b *Backend) sdkErr(wc *writeCtx, err error) (*logical.Response, error) {
	b.gate.recordRequest(wc.vaultID, 1, err)
	if classifyError(err) == errClassRateLimited {
		return nil, fmt.Errorf("%w: 1Password rate limit exceeded; retry later", logical.ErrRateLimitQuotaExceeded)
	}
	return logical.ErrorResponse("op: 1Password write failed: %v", err), nil
}

// locateItem resolves itemAddr in the replica, running at most one miss
// cycle (ensureItemFresh) when it is not known. A write never waits for
// a freshness window: it re-fetches the item itself.
func (b *Backend) locateItem(ctx context.Context, wc *writeCtx, negKey string, resolve func() (string, error)) (id string, found bool, err error) {
	if id, rerr := resolve(); rerr == nil {
		return id, true, nil
	} else if errors.Is(rerr, errAmbiguousTitle) {
		return "", false, rerr
	}
	id, found, err = b.ensureItemFresh(ctx, wc.vaultID, negKey, resolve)
	if errors.Is(err, errReplicaEmpty) {
		// A genuinely empty vault also reads as "empty"; only a replica
		// that has completed a cycle may treat it as absent.
		if last, _, _ := wc.r.freshnessSnapshot(); !last.IsZero() {
			return "", false, nil
		}
	}
	return id, found, err
}

// fetchFresh is the one GetAll an update spends on top of the Put.
func (b *Backend) fetchFresh(ctx context.Context, wc *writeCtx, itemID string) (onepassword.Item, *logical.Response, error) {
	res, err := wc.client.GetItems(ctx, wc.vaultID, []string{itemID})
	b.gate.recordRequest(wc.vaultID, 1, err)
	if err != nil {
		resp, herr := b.sdkErrNoRecord(err)
		return onepassword.Item{}, resp, herr
	}
	if len(res) != 1 || res[0].Err != nil || res[0].Item == nil {
		return onepassword.Item{}, logical.ErrorResponse("op: not found: item %q vanished before it could be updated", itemID), nil
	}
	return *res[0].Item, nil, nil
}

// sdkErrNoRecord is sdkErr for calls whose spend was already recorded.
func (b *Backend) sdkErrNoRecord(err error) (*logical.Response, error) {
	if classifyError(err) == errClassRateLimited {
		return nil, fmt.Errorf("%w: 1Password rate limit exceeded; retry later", logical.ErrRateLimitQuotaExceeded)
	}
	return logical.ErrorResponse("op: 1Password write failed: %v", err), nil
}

// writeResponse is the value-free write response.
func writeResponse(item onepassword.Item) *logical.Response {
	return &logical.Response{Data: map[string]interface{}{
		"id":         item.ID,
		"title":      item.Title,
		"version":    item.Version,
		"updated_at": item.UpdatedAt,
	}}
}

// --- request body parsing ---

var (
	categoryByName  = map[string]onepassword.ItemCategory{}
	fieldTypeByName = map[string]onepassword.ItemFieldType{}
)

func init() {
	norm := func(s string) string {
		return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
	}
	for _, c := range []onepassword.ItemCategory{
		onepassword.ItemCategoryLogin, onepassword.ItemCategorySecureNote, onepassword.ItemCategoryCreditCard,
		onepassword.ItemCategoryCryptoWallet, onepassword.ItemCategoryIdentity, onepassword.ItemCategoryPassword,
		onepassword.ItemCategoryAPICredentials, onepassword.ItemCategoryBankAccount, onepassword.ItemCategoryDatabase,
		onepassword.ItemCategoryDriverLicense, onepassword.ItemCategoryEmail, onepassword.ItemCategoryMedicalRecord,
		onepassword.ItemCategoryMembership, onepassword.ItemCategoryOutdoorLicense, onepassword.ItemCategoryPassport,
		onepassword.ItemCategoryRewards, onepassword.ItemCategoryRouter, onepassword.ItemCategoryServer,
		onepassword.ItemCategorySSHKey, onepassword.ItemCategorySocialSecurityNumber, onepassword.ItemCategorySoftwareLicense,
		onepassword.ItemCategoryPerson,
	} {
		categoryByName[norm(string(c))] = c
	}
	// Documents/files, OTP and unsupported are out of scope (plan).
	for _, t := range []onepassword.ItemFieldType{
		onepassword.ItemFieldTypeText, onepassword.ItemFieldTypeConcealed, onepassword.ItemFieldTypeURL,
		onepassword.ItemFieldTypeEmail, onepassword.ItemFieldTypePhone, onepassword.ItemFieldTypeDate,
		onepassword.ItemFieldTypeMonthYear, onepassword.ItemFieldTypeMenu, onepassword.ItemFieldTypeReference,
	} {
		fieldTypeByName[norm(string(t))] = t
	}
}

func normName(s string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
}

// stringMap reads a map-of-strings request field. Non-string values are
// rejected without echoing them.
func stringMap(raw interface{}, name string) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a map of label to string value", name)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: value for %q must be a string", name, k)
		}
		out[k] = s
	}
	return out, nil
}

// newID returns a short random ID for new sections and section fields.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// applyFields merges fields (label or "<section>/<label>" -> value) over
// item: named fields are updated in place (type kept unless types names
// one), unnamed fields and sections are untouched, absent fields and
// sections are created. creating marks a new item, whose top-level
// fields take their label as ID (the SDK's built-in field convention).
func applyFields(item *onepassword.Item, fields, types map[string]string, creating bool) error {
	labels := make([]string, 0, len(fields))
	for k := range fields {
		labels = append(labels, k)
	}
	sort.Strings(labels)

	for _, key := range labels {
		section, label, hasSection := strings.Cut(key, "/")
		if !hasSection {
			section, label = "", key
		}
		if label == "" || (hasSection && section == "") {
			return fmt.Errorf("invalid field key %q", key)
		}

		var sectionID *string
		if hasSection {
			for _, s := range item.Sections {
				if s.Title == section {
					id := s.ID
					sectionID = &id
					break
				}
			}
			if sectionID == nil {
				id := newID()
				item.Sections = append(item.Sections, onepassword.ItemSection{ID: id, Title: section})
				sectionID = &id
			}
		}

		var wantType onepassword.ItemFieldType
		if tn, ok := types[key]; ok {
			t, ok := fieldTypeByName[normName(tn)]
			if !ok {
				return fmt.Errorf("field_types: unknown type %q for %q", tn, key)
			}
			wantType = t
		}

		var existing *onepassword.ItemField
		for i := range item.Fields {
			f := &item.Fields[i]
			if f.Title != label {
				continue
			}
			if (sectionID == nil) != (f.SectionID == nil) || (sectionID != nil && *f.SectionID != *sectionID) {
				continue
			}
			existing = f
			break
		}
		if existing != nil {
			existing.Value = fields[key]
			if wantType != "" {
				existing.FieldType = wantType
			}
			continue
		}

		if wantType == "" {
			wantType = onepassword.ItemFieldTypeText
			if l := strings.ToLower(label); l == "password" || l == "credential" {
				wantType = onepassword.ItemFieldTypeConcealed
			}
		}
		f := onepassword.ItemField{ID: newID(), Title: label, SectionID: sectionID, FieldType: wantType, Value: fields[key]}
		if creating && sectionID == nil {
			f.ID = label
		}
		item.Fields = append(item.Fields, f)
	}
	for k := range types {
		if _, ok := fields[k]; !ok {
			return fmt.Errorf("field_types names %q, which is not in fields", k)
		}
	}
	return nil
}

// --- item/<vault>/<item> ---

func (b *Backend) pathItemExistence(ctx context.Context, req *logical.Request, data *framework.FieldData) (bool, error) {
	vaultID, err := b.resolveVaultAddress(data.Get("vault").(string))
	if err != nil {
		return false, nil
	}
	_, rerr := resolveItemFromSegments(b.getOrCreateReplica(vaultID), strings.Split(data.Get("item").(string), "/"))
	return rerr == nil || errors.Is(rerr, errAmbiguousTitle), nil
}

func (b *Backend) pathItemWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	itemAddr := data.Get("item").(string)

	fields, err := stringMap(data.Raw["fields"], "fields")
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	types, err := stringMap(data.Raw["field_types"], "field_types")
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	var category onepassword.ItemCategory
	if cn := data.Get("category").(string); cn != "" {
		c, ok := categoryByName[normName(cn)]
		if !ok {
			return logical.ErrorResponse("category: unknown category %q", cn), nil
		}
		category = c
	}

	wc, resp, err := b.beginWrite(ctx, data.Get("vault").(string))
	if wc == nil {
		return resp, err
	}

	segs := strings.Split(itemAddr, "/")
	itemID, found, err := b.locateItem(ctx, wc, itemAddr, func() (string, error) { return resolveItemFromSegments(wc.r, segs) })
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	notes, hasNotes := data.GetOk("notes")
	tags, hasTags := data.GetOk("tags")
	url := data.Get("url").(string)

	var out onepassword.Item
	if found {
		cur, resp, err := b.fetchFresh(ctx, wc, itemID)
		if cur.ID == "" {
			return resp, err
		}
		var warnings []string
		if category != "" && category != cur.Category {
			warnings = append(warnings, "category is ignored on update; the item keeps its existing category")
		}
		if err := applyFields(&cur, fields, types, false); err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
		if hasNotes {
			cur.Notes = notes.(string)
		}
		if hasTags {
			cur.Tags = tags.([]string)
		}
		if url != "" {
			setWebsite(&cur.Websites, url)
		}
		out, err = wc.client.PutItem(ctx, cur)
		if err != nil {
			return b.sdkErr(wc, err)
		}
		b.gate.recordRequest(wc.vaultID, 1, nil)
		wc.r.applyWrite(out, wc.rx)
		b.Logger().Info("op: item updated", "vault_id", wc.vaultID, "item_id", out.ID, "version", out.Version)
		r := writeResponse(out)
		for _, w := range warnings {
			r.AddWarning(w)
		}
		return r, nil
	}

	if itemIDShape.MatchString(itemAddr) {
		return logical.ErrorResponse("op: not found: item ID %q does not exist (an ID address never creates an item)", itemAddr), nil
	}
	if len(segs) != 1 {
		return logical.ErrorResponse("op: cannot create an item with a multi-segment address; use a single-segment title"), nil
	}
	if category == "" {
		category = onepassword.ItemCategoryLogin
	}
	n := onepassword.Item{Title: itemAddr, Category: category, VaultID: wc.vaultID}
	if err := applyFields(&n, fields, types, true); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	params := onepassword.ItemCreateParams{Category: category, VaultID: wc.vaultID, Title: itemAddr, Fields: n.Fields, Sections: n.Sections}
	if hasNotes {
		s := notes.(string)
		params.Notes = &s
	}
	if hasTags {
		params.Tags = tags.([]string)
	}
	if url != "" {
		setWebsite(&params.Websites, url)
	}
	out, err = wc.client.CreateItem(ctx, params)
	if err != nil {
		return b.sdkErr(wc, err)
	}
	b.gate.recordRequest(wc.vaultID, 1, nil)
	wc.r.applyWrite(out, wc.rx)
	b.Logger().Info("op: item created", "vault_id", wc.vaultID, "item_id", out.ID, "version", out.Version)
	return writeResponse(out), nil
}

// setWebsite sets the item's primary "website" entry.
func setWebsite(ws *[]onepassword.Website, url string) {
	for i := range *ws {
		if (*ws)[i].Label == "website" {
			(*ws)[i].URL = url
			return
		}
	}
	*ws = append(*ws, onepassword.Website{URL: url, Label: "website", AutofillBehavior: onepassword.AutofillBehaviorAnywhereOnWebsite})
}

func (b *Backend) pathItemDelete(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	itemAddr := data.Get("item").(string)
	wc, resp, err := b.beginWrite(ctx, data.Get("vault").(string))
	if wc == nil {
		return resp, err
	}
	segs := strings.Split(itemAddr, "/")
	itemID, found, err := b.locateItem(ctx, wc, itemAddr, func() (string, error) { return resolveItemFromSegments(wc.r, segs) })
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	if !found {
		return nil, nil // idempotent: already absent
	}
	if err := wc.client.DeleteItem(ctx, wc.vaultID, itemID); err != nil {
		return b.sdkErr(wc, err)
	}
	b.gate.recordRequest(wc.vaultID, 1, nil)
	wc.r.applyDelete(itemID, wc.rx)
	b.Logger().Info("op: item deleted", "vault_id", wc.vaultID, "item_id", itemID)
	return nil, nil
}

// --- field/<vault>/<item>/<field> ---

func (b *Backend) pathFieldWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	rest := data.Get("rest").(string)
	value, ok := data.GetOk("value")
	if !ok {
		return logical.ErrorResponse("value is required"), nil
	}

	wc, resp, err := b.beginWrite(ctx, data.Get("vault").(string))
	if wc == nil {
		return resp, err
	}

	segs := strings.Split(rest, "/")
	var section, label string
	itemID, found, err := b.locateItem(ctx, wc, rest, func() (string, error) {
		id, sec, fld, ferr := resolveFieldAddress(wc.r, segs)
		section, label = sec, fld
		return id, ferr
	})
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	if !found {
		return logical.ErrorResponse(errNotFound.Error()), nil
	}

	key := label
	if section != "" {
		key = section + "/" + label
	}
	types := map[string]string{}
	if t := data.Get("type").(string); t != "" {
		types[key] = t
	}

	cur, resp, err := b.fetchFresh(ctx, wc, itemID)
	if cur.ID == "" {
		return resp, err
	}
	if err := applyFields(&cur, map[string]string{key: value.(string)}, types, false); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	out, err := wc.client.PutItem(ctx, cur)
	if err != nil {
		return b.sdkErr(wc, err)
	}
	b.gate.recordRequest(wc.vaultID, 1, nil)
	wc.r.applyWrite(out, wc.rx)
	b.Logger().Info("op: field updated", "vault_id", wc.vaultID, "item_id", out.ID, "version", out.Version)
	return writeResponse(out), nil
}
