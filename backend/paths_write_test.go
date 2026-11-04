package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const absentID = "abcdefghijklmnopqrstuvwxyz" // 26 lowercase alnum: shaped like a 1Password ID

func writeBackend(t *testing.T, overrides map[string]interface{}) (*Backend, logical.Storage, *FakeOPClient) {
	t.Helper()
	o := map[string]interface{}{"write_vaults": "Infra"}
	for k, v := range overrides {
		o[k] = v
	}
	return setupItemBackend(t, o)
}

func doWrite(t *testing.T, b *Backend, storage logical.Storage, op logical.Operation, path string, data map[string]interface{}) (*logical.Response, error) {
	t.Helper()
	return b.HandleRequest(context.Background(), &logical.Request{Operation: op, Path: path, Storage: storage, Data: data})
}

func mustWrite(t *testing.T, b *Backend, storage logical.Storage, path string, data map[string]interface{}) *logical.Response {
	t.Helper()
	resp, err := doWrite(t, b, storage, logical.UpdateOperation, path, data)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("write %s: resp=%+v, want success", path, resp)
	}
	return resp
}

func wantErrResp(t *testing.T, resp *logical.Response, err error, substr string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if resp == nil || !resp.IsError() || !strings.Contains(resp.Error().Error(), substr) {
		t.Fatalf("resp=%+v, want error response containing %q", resp, substr)
	}
}

func fieldByTitle(item onepassword.Item, title string) *onepassword.ItemField {
	for i := range item.Fields {
		if item.Fields[i].Title == title {
			return &item.Fields[i]
		}
	}
	return nil
}

func TestWrite_CreateOnAbsentTitle(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	resp := mustWrite(t, b, storage, "item/Infra/newthing", map[string]interface{}{
		"fields": map[string]interface{}{"username": "bob", "password": "pw1", "note": "x"},
		"notes":  "hello", "tags": "a,b", "url": "https://example.com",
	})
	if len(fake.CreateCalls) != 1 {
		t.Fatalf("CreateCalls = %d, want 1", len(fake.CreateCalls))
	}
	c := fake.CreateCalls[0]
	if c.Title != "newthing" || c.VaultID != "v1" || c.Category != onepassword.ItemCategoryLogin {
		t.Errorf("create params = %+v", c)
	}
	f := func(title string) onepassword.ItemField {
		for _, x := range c.Fields {
			if x.Title == title {
				return x
			}
		}
		t.Fatalf("no field %q", title)
		return onepassword.ItemField{}
	}
	if f("password").FieldType != onepassword.ItemFieldTypeConcealed || f("username").FieldType != onepassword.ItemFieldTypeText {
		t.Errorf("default types wrong: %+v", c.Fields)
	}
	if c.Notes == nil || *c.Notes != "hello" || len(c.Tags) != 2 || len(c.Websites) != 1 {
		t.Errorf("notes/tags/url not applied: %+v", c)
	}
	for _, k := range []string{"id", "title", "version", "updated_at"} {
		if _, ok := resp.Data[k]; !ok {
			t.Errorf("response missing %q", k)
		}
	}
	if resp.Data["title"] != "newthing" {
		t.Errorf("title = %v", resp.Data["title"])
	}
}

func TestWrite_CreateCategoryAndFieldTypes(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	mustWrite(t, b, storage, "item/Infra/k", map[string]interface{}{
		"category":    "api_credentials",
		"fields":      map[string]interface{}{"token": "t", "site": "https://x"},
		"field_types": map[string]interface{}{"site": "url"},
	})
	c := fake.CreateCalls[0]
	if c.Category != onepassword.ItemCategoryAPICredentials {
		t.Errorf("category = %v", c.Category)
	}
	for _, f := range c.Fields {
		if f.Title == "site" && f.FieldType != onepassword.ItemFieldTypeURL {
			t.Errorf("site type = %v", f.FieldType)
		}
	}
	resp, err := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/k2", map[string]interface{}{"category": "nonsense"})
	wantErrResp(t, resp, err, "category")
}

func TestWrite_UpdateKeepsUnnamedFields(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	sec := "sec1"
	fake.ItemBodies["v1"]["i1"] = onepassword.Item{
		ID: "i1", Title: "postgres", Category: onepassword.ItemCategoryLogin, VaultID: "v1", Version: 3, Notes: "keep me", Tags: []string{"t"},
		Sections: []onepassword.ItemSection{{ID: sec, Title: "extra"}},
		Fields: []onepassword.ItemField{
			{ID: "f1", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "hunter2"},
			{ID: "f2", Title: "user", FieldType: onepassword.ItemFieldTypeText, Value: "alice"},
			{ID: "f3", Title: "key", SectionID: &sec, FieldType: onepassword.ItemFieldTypeConcealed, Value: "k1"},
		},
	}
	mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{
		"fields":   map[string]interface{}{"password": "new-pw"},
		"category": "securenote",
	})
	if len(fake.PutCalls) != 1 || len(fake.CreateCalls) != 0 {
		t.Fatalf("puts=%d creates=%d", len(fake.PutCalls), len(fake.CreateCalls))
	}
	p := fake.PutCalls[0]
	if fieldByTitle(p, "password").Value != "new-pw" || fieldByTitle(p, "user").Value != "alice" || fieldByTitle(p, "key").Value != "k1" {
		t.Errorf("fields not merged: %+v", p.Fields)
	}
	if p.Notes != "keep me" || len(p.Tags) != 1 || len(p.Sections) != 1 || p.Category != onepassword.ItemCategoryLogin {
		t.Errorf("unnamed data dropped or category changed: %+v", p)
	}
}

func TestWrite_UpdateWarnsOnCategoryMismatch(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	fake.ItemBodies["v1"]["i1"] = onepassword.Item{ID: "i1", Title: "postgres", Category: onepassword.ItemCategoryLogin, VaultID: "v1", Version: 1}
	resp := mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{"category": "securenote"})
	if len(resp.Warnings) == 0 {
		t.Errorf("want a category warning")
	}
}

func TestWrite_SectionFieldCreateAndUpdate(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	mustWrite(t, b, storage, "item/Infra/sectioned", map[string]interface{}{
		"fields": map[string]interface{}{"db/password": "p1", "top": "t"},
	})
	c := fake.CreateCalls[0]
	if len(c.Sections) != 1 || c.Sections[0].Title != "db" {
		t.Fatalf("sections = %+v", c.Sections)
	}
	var sf *onepassword.ItemField
	for i := range c.Fields {
		if c.Fields[i].Title == "password" {
			sf = &c.Fields[i]
		}
	}
	if sf == nil || sf.SectionID == nil || *sf.SectionID != c.Sections[0].ID || sf.FieldType != onepassword.ItemFieldTypeConcealed {
		t.Fatalf("section field wrong: %+v", c.Fields)
	}

	mustWrite(t, b, storage, "item/Infra/sectioned", map[string]interface{}{"fields": map[string]interface{}{"db/password": "p2"}})
	p := fake.PutCalls[0]
	if len(p.Sections) != 1 {
		t.Errorf("update duplicated the section: %+v", p.Sections)
	}
	n := 0
	for _, f := range p.Fields {
		if f.Title == "password" {
			n++
			if f.Value != "p2" {
				t.Errorf("section password = %q", f.Value)
			}
		}
		if f.Title == "top" && f.Value != "t" {
			t.Errorf("unnamed field changed")
		}
	}
	if n != 1 {
		t.Errorf("password fields = %d, want 1", n)
	}
}

func TestWrite_SectionTitleWithSlash(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	// the last "/" splits: labels rarely hold one, section titles sometimes do
	mustWrite(t, b, storage, "item/Infra/token", map[string]interface{}{
		"fields": map[string]interface{}{"Proxmox API token (scoped /vms/556)/credential": "c1"},
	})
	c := fake.CreateCalls[0]
	if len(c.Sections) != 1 || c.Sections[0].Title != "Proxmox API token (scoped /vms/556)" {
		t.Fatalf("sections = %+v", c.Sections)
	}
	if len(c.Fields) != 1 || c.Fields[0].Title != "credential" || c.Fields[0].SectionID == nil {
		t.Fatalf("fields = %+v", c.Fields)
	}
}

func TestWrite_FieldUpdate(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	fake.ItemBodies["v1"]["i1"] = onepassword.Item{
		ID: "i1", Title: "postgres", VaultID: "v1", Version: 1,
		Fields: []onepassword.ItemField{
			{ID: "f1", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "hunter2"},
			{ID: "f2", Title: "user", FieldType: onepassword.ItemFieldTypeText, Value: "alice"},
		},
	}
	resp := mustWrite(t, b, storage, "field/Infra/postgres/password", map[string]interface{}{"value": "rotated"})
	if _, ok := resp.Data["value"]; ok {
		t.Errorf("field write response carries a value")
	}
	p := fake.PutCalls[0]
	if fieldByTitle(p, "password").Value != "rotated" || fieldByTitle(p, "user").Value != "alice" {
		t.Errorf("fields = %+v", p.Fields)
	}
	got := mustReadField(t, b, storage, "Infra", "postgres/password")
	if got != "rotated" {
		t.Errorf("read after field write = %q", got)
	}

	// absent item is an error, not a create
	r, err := doWrite(t, b, storage, logical.UpdateOperation, "field/Infra/nope/password", map[string]interface{}{"value": "x"})
	wantErrResp(t, r, err, "not found")
	if len(fake.CreateCalls) != 0 {
		t.Errorf("field write created an item")
	}
	// missing value
	r, err = doWrite(t, b, storage, logical.UpdateOperation, "field/Infra/postgres/password", map[string]interface{}{})
	wantErrResp(t, r, err, "value")
}

func mustReadField(t *testing.T, b *Backend, storage logical.Storage, vault, path string) string {
	t.Helper()
	resp, err := readField(t, b, storage, vault, path)
	if err != nil || resp == nil || resp.IsError() {
		t.Fatalf("read field: resp=%+v err=%v", resp, err)
	}
	return resp.Data["value"].(string)
}

func TestWrite_DeleteIdempotent(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	resp, err := doWrite(t, b, storage, logical.DeleteOperation, "item/Infra/postgres", nil)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("delete: resp=%+v err=%v", resp, err)
	}
	if len(fake.DeleteCalls) != 1 {
		t.Fatalf("DeleteCalls = %v", fake.DeleteCalls)
	}
	// replica no longer serves it
	rr, _ := readItem(t, b, storage, "Infra", "postgres")
	if rr == nil || !rr.IsError() {
		t.Errorf("deleted item still readable: %+v", rr)
	}
	// second delete and delete of never-existing: success, no SDK call
	for _, name := range []string{"postgres", "never-there"} {
		resp, err = doWrite(t, b, storage, logical.DeleteOperation, "item/Infra/"+name, nil)
		if err != nil || (resp != nil && resp.IsError()) {
			t.Fatalf("idempotent delete %s: resp=%+v err=%v", name, resp, err)
		}
	}
	if len(fake.DeleteCalls) != 1 {
		t.Errorf("DeleteCalls = %v, want still 1", fake.DeleteCalls)
	}
}

func TestWrite_RefusedOutsideWriteVaults(t *testing.T) {
	// read-only engine
	b, storage, fake := setupItemBackend(t, nil)
	resp, err := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	wantErrResp(t, resp, err, "write_vaults")
	resp, err = doWrite(t, b, storage, logical.DeleteOperation, "item/Infra/postgres", nil)
	wantErrResp(t, resp, err, "write_vaults")
	resp, err = doWrite(t, b, storage, logical.UpdateOperation, "field/Infra/postgres/password", map[string]interface{}{"value": "x"})
	wantErrResp(t, resp, err, "write_vaults")
	if len(fake.CreateCalls)+len(fake.PutCalls)+len(fake.DeleteCalls) != 0 {
		t.Errorf("read-only engine called the write API")
	}

	// a readable vault that is not writable
	b, storage, fake = writeBackend(t, map[string]interface{}{"vaults": "Infra,Other"})
	fake.Vaults = append(fake.Vaults, onepassword.VaultOverview{ID: "v2", Title: "Other"})
	fake.Items["v2"] = nil
	if err := b.refreshVaultDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err = doWrite(t, b, storage, logical.UpdateOperation, "item/Other/thing", map[string]interface{}{"fields": map[string]interface{}{"a": "b"}})
	wantErrResp(t, resp, err, "write_vaults")
	if len(fake.CreateCalls) != 0 {
		t.Errorf("wrote to a non-writable vault")
	}
}

func TestConfig_WriteVaultsMustBeSubset(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) { return NewFakeOPClient(), nil }
	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok", "vaults": "Infra", "write_vaults": "Infra,Other",
	})
	wantErrResp(t, resp, err, "write_vaults")
	resp, err = writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok", "vaults": "Infra", "write_vaults": "Infra",
	})
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("valid write_vaults rejected: %+v %v", resp, err)
	}
	rr, _ := readConfig(t, b, storage)
	if wv, _ := rr.Data["write_vaults"].([]string); len(wv) != 1 || wv[0] != "Infra" {
		t.Errorf("write_vaults read = %v", rr.Data["write_vaults"])
	}
}

func TestWrite_IDAddressAbsentIsError(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	resp, err := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/"+absentID, map[string]interface{}{"fields": map[string]interface{}{"a": "b"}})
	wantErrResp(t, resp, err, "not found")
	if len(fake.CreateCalls) != 0 {
		t.Errorf("an absent ID address created an item")
	}
}

func TestWrite_ExistenceCheck(t *testing.T) {
	b, storage, _ := writeBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres") // warm replica
	check := func(name string) bool {
		ok, found, err := b.HandleExistenceCheck(context.Background(), &logical.Request{Operation: logical.CreateOperation, Path: "item/Infra/" + name, Storage: storage})
		if err != nil || !ok {
			t.Fatalf("existence check: ok=%v err=%v", ok, err)
		}
		return found
	}
	if !check("postgres") || check("fresh-title") {
		t.Errorf("existence check wrong")
	}
}

func TestWrite_ReadAfterWriteNoExtraFetch(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "after-write"}})
	mustWrite(t, b, storage, "item/Infra/created-now", map[string]interface{}{"fields": map[string]interface{}{"password": "fresh"}})
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0

	if v := mustReadField(t, b, storage, "Infra", "postgres/password"); v != "after-write" {
		t.Errorf("updated value = %q", v)
	}
	if v := mustReadField(t, b, storage, "Infra", "created-now/password"); v != "fresh" {
		t.Errorf("created value = %q", v)
	}
	if fake.ListItemsCalls != 0 || fake.GetItemsCalls != 0 {
		t.Errorf("read after write fetched: list=%d get=%d", fake.ListItemsCalls, fake.GetItemsCalls)
	}
}

func TestWrite_DeltaCycleAfterWriteDoesNotRegress(t *testing.T) {
	b, storage, fake := writeBackend(t, map[string]interface{}{"passthrough_ttl": "0s"})
	oldOverviews := append([]onepassword.ItemOverview(nil), fake.Items["v1"]...)
	oldBody := fake.ItemBodies["v1"]["i1"]
	oldBody.Version = 1
	fake.ItemBodies["v1"]["i1"] = oldBody
	mustReadItem(t, b, storage, "Infra", "postgres") // warm

	mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "newer"}})

	// 1Password lags: list and get still show the pre-write state.
	fake.Items["v1"] = oldOverviews
	fake.ItemBodies["v1"]["i1"] = oldBody
	if err := b.runVaultCycle(context.Background(), "v1", workClassManual); err != nil {
		t.Fatal(err)
	}
	if v := mustReadField(t, b, storage, "Infra", "postgres/password"); v != "newer" {
		t.Errorf("delta cycle regressed the write: %q", v)
	}
}

func TestWrite_DeltaCycleAfterDeleteDoesNotResurrect(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")
	staleOv := append([]onepassword.ItemOverview(nil), fake.Items["v1"]...)
	staleBody := fake.ItemBodies["v1"]["i1"]
	if resp, err := doWrite(t, b, storage, logical.DeleteOperation, "item/Infra/postgres", nil); err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	fake.Items["v1"] = staleOv
	fake.ItemBodies["v1"]["i1"] = staleBody
	if err := b.runVaultCycle(context.Background(), "v1", workClassManual); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.getOrCreateReplica("v1").body("i1"); ok {
		t.Errorf("deleted item resurrected by a lagging delta cycle")
	}
}

func TestWrite_GovernorDeferredFailsLoudly(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	b.gate.recordRequest("v1", 1, &onepassword.RateLimitExceededError{})
	resp, err := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	if !errors.Is(err, logical.ErrRateLimitQuotaExceeded) {
		t.Fatalf("resp=%+v err=%v, want a retryable rate-limit error", resp, err)
	}
	if len(fake.CreateCalls)+len(fake.PutCalls) != 0 {
		t.Errorf("deferred write still called the SDK")
	}
	resp, err = doWrite(t, b, storage, logical.DeleteOperation, "item/Infra/postgres", nil)
	if !errors.Is(err, logical.ErrRateLimitQuotaExceeded) {
		t.Fatalf("delete: resp=%+v err=%v", resp, err)
	}
}

func TestWrite_SDKRateLimitSurfacedRetryable(t *testing.T) {
	b, storage, fake := writeBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")
	fake.PutErr = &onepassword.RateLimitExceededError{}
	_, err := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	if !errors.Is(err, logical.ErrRateLimitQuotaExceeded) {
		t.Fatalf("err = %v, want retryable", err)
	}
	if b.gate.snapshot().State != "rate_limited" {
		t.Errorf("governor not told: %s", b.gate.snapshot().State)
	}
}

func TestWrite_SpendsFromSameBudget(t *testing.T) {
	b, storage, _ := writeBackend(t, nil)
	mustReadItem(t, b, storage, "Infra", "postgres")
	n := func() int { b.gate.mu.Lock(); defer b.gate.mu.Unlock(); return len(b.gate.events) }
	e0 := n()
	mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	if d := n() - e0; d != 2 { // fresh GetAll + Put
		t.Errorf("update spent %d units, want 2", d)
	}
	e0 = n()
	mustWrite(t, b, storage, "item/Infra/brand-new", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	if d := n() - e0; d < 1 {
		t.Errorf("create spent %d units", d)
	}
}

func TestWrite_NoValueInResponseOrLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := hclog.New(&hclog.LoggerOptions{Output: &buf, Level: hclog.Trace})
	b, storage, fake := setupItemBackendLogger(t, map[string]interface{}{"write_vaults": "Infra"}, logger)
	const secret = "TOP-SECRET-VALUE-9f3a"

	var all []*logical.Response
	all = append(all, mustWrite(t, b, storage, "item/Infra/leaktest", map[string]interface{}{
		"fields": map[string]interface{}{"password": secret, "s/api": secret + "2"}, "notes": "n",
	}))
	all = append(all, mustWrite(t, b, storage, "item/Infra/leaktest", map[string]interface{}{"fields": map[string]interface{}{"password": secret + "3"}}))
	all = append(all, mustWrite(t, b, storage, "field/Infra/leaktest/password", map[string]interface{}{"value": secret + "4"}))
	// error paths too: bad type, with the value in the same request
	r, _ := doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/leaktest", map[string]interface{}{
		"fields": map[string]interface{}{"password": secret + "5"}, "field_types": map[string]interface{}{"password": "bogus"},
	})
	all = append(all, r)
	fake.PutErr = errors.New("boom")
	r, _ = doWrite(t, b, storage, logical.UpdateOperation, "item/Infra/leaktest", map[string]interface{}{"fields": map[string]interface{}{"password": secret + "6"}})
	all = append(all, r)

	for i, r := range all {
		if r == nil {
			continue
		}
		j, _ := json.Marshal(r)
		if strings.Contains(string(j), "TOP-SECRET") {
			t.Errorf("response %d contains a secret value: %s", i, j)
		}
	}
	if buf.Len() == 0 {
		t.Errorf("expected the write to be logged (without values)")
	}
	if strings.Contains(buf.String(), "TOP-SECRET") {
		t.Errorf("log contains a secret value:\n%s", buf.String())
	}
}

func TestWrite_ReplicaCoherenceTimestamps(t *testing.T) {
	// the written item's overview carries the SDK's updated_at so the next
	// list does not mark it changed.
	b, storage, fake := writeBackend(t, nil)
	mustWrite(t, b, storage, "item/Infra/postgres", map[string]interface{}{"fields": map[string]interface{}{"password": "x"}})
	fake.ListItemsCalls, fake.GetItemsCalls = 0, 0
	if err := b.runVaultCycle(context.Background(), "v1", workClassManual); err != nil {
		t.Fatal(err)
	}
	if fake.GetItemsCalls != 0 {
		t.Errorf("cycle after write refetched the written item (%d gets)", fake.GetItemsCalls)
	}
}
