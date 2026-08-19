package backend

import (
	"context"
	"testing"

	onepassword "github.com/1password/onepassword-sdk-go"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func readField(t *testing.T, b *Backend, storage logical.Storage, vault, path string) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "field/" + vault + "/" + path,
		Storage:   storage,
	}
	return b.HandleRequest(context.Background(), req)
}

func TestPathField_ReadValue(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	resp, err := readField(t, b, storage, "Infra", "postgres/password")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if resp.Data["value"] != "hunter2" {
		t.Errorf("value = %v, want hunter2", resp.Data["value"])
	}
	if _, ok := resp.Data["replica_age_seconds"]; !ok {
		t.Errorf("response missing replica_age_seconds")
	}
}

func TestPathField_SectionQualified(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	sectionID := "sec1"
	fake.ItemBodies["v1"]["i1"] = onepassword.Item{
		ID: "i1", Title: "postgres",
		Sections: []onepassword.ItemSection{{ID: sectionID, Title: "extra"}},
		Fields: []onepassword.ItemField{
			{ID: "f1", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "top-level-pw"},
			{ID: "f2", Title: "password", SectionID: &sectionID, FieldType: onepassword.ItemFieldTypeConcealed, Value: "section-pw"},
		},
	}

	resp, err := readField(t, b, storage, "Infra", "postgres/extra/password")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if resp.Data["value"] != "section-pw" {
		t.Errorf("section-qualified value = %v, want section-pw", resp.Data["value"])
	}
}

func TestPathField_UnqualifiedTakesFirstMatch(t *testing.T) {
	b, storage, fake := setupItemBackend(t, nil)
	sectionID := "sec1"
	fake.ItemBodies["v1"]["i1"] = onepassword.Item{
		ID: "i1", Title: "postgres",
		Sections: []onepassword.ItemSection{{ID: sectionID, Title: "extra"}},
		Fields: []onepassword.ItemField{
			{ID: "f1", Title: "password", FieldType: onepassword.ItemFieldTypeConcealed, Value: "top-level-pw"},
			{ID: "f2", Title: "password", SectionID: &sectionID, FieldType: onepassword.ItemFieldTypeConcealed, Value: "section-pw"},
		},
	}

	resp, err := readField(t, b, storage, "Infra", "postgres/password")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if resp.Data["value"] != "top-level-pw" {
		t.Errorf("unqualified duplicate-label value = %v, want top-level-pw (first match)", resp.Data["value"])
	}
}

func TestPathField_SplitPathAddressing(t *testing.T) {
	b, storage, fake := setupItemBackend(t, map[string]interface{}{
		"path_split": "__",
	})
	fake.Items["v1"] = []onepassword.ItemOverview{
		{ID: "i1", Title: "db.example.com__postgres", State: onepassword.ItemStateActive},
	}
	fake.ItemBodies["v1"] = map[string]onepassword.Item{
		"i1": {
			ID: "i1", Title: "db.example.com__postgres",
			Fields: []onepassword.ItemField{
				{ID: "f1", Title: "credential", FieldType: onepassword.ItemFieldTypeConcealed, Value: "split-secret"},
			},
		},
	}

	resp, err := readField(t, b, storage, "Infra", "db.example.com/postgres/credential")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("resp=%+v, want a successful response", resp)
	}
	if resp.Data["value"] != "split-secret" {
		t.Errorf("split-path field value = %v, want split-secret", resp.Data["value"])
	}
}

func TestPathField_NotFound(t *testing.T) {
	b, storage, _ := setupItemBackend(t, nil)
	resp, err := readField(t, b, storage, "Infra", "postgres/does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("unknown field: resp=%+v, want an error response", resp)
	}
}
