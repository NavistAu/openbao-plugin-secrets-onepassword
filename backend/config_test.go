package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
)

func writeConfig(t *testing.T, b *Backend, storage logical.Storage, data map[string]interface{}) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}
	return b.HandleRequest(context.Background(), req)
}

func readConfig(t *testing.T, b *Backend, storage logical.Storage) (*logical.Response, error) {
	t.Helper()
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "config",
		Storage:   storage,
	}
	return b.HandleRequest(context.Background(), req)
}

func TestPathConfig_WriteReadRedaction(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "super-secret-token",
		"vaults":                "Infra",
		"path_split":            "__",
	})
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("write config: resp=%+v err=%v", resp, err)
	}

	resp, err = readConfig(t, b, storage)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if resp == nil {
		t.Fatalf("read config: nil response")
	}
	if _, present := resp.Data["service_account_token"]; present {
		t.Errorf("service_account_token leaked in read response: %+v", resp.Data)
	}
	for k, v := range resp.Data {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret-token") {
			t.Errorf("token leaked via field %q: %v", k, v)
		}
	}
	if got := resp.Data["path_split"]; got != "__" {
		t.Errorf("path_split = %v, want __", got)
	}
	vaults, ok := resp.Data["vaults"].([]string)
	if !ok || len(vaults) != 1 || vaults[0] != "Infra" {
		t.Errorf("vaults = %v, want [Infra]", resp.Data["vaults"])
	}
	if got := resp.Data["refresh_interval"]; got != "15m0s" {
		t.Errorf("refresh_interval default = %v, want 15m0s", got)
	}
	if got := resp.Data["daily_request_limit"]; got != 1000 {
		t.Errorf("daily_request_limit default = %v, want 1000", got)
	}
}

func TestPathConfig_RequiresToken(t *testing.T) {
	b, storage := newTestBackend(t)

	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"vaults": "Infra",
	})
	if err != nil {
		t.Fatalf("write config: unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("write config without token: resp=%+v, want an error response", resp)
	}
}

func TestPathConfig_GuardrailRejection(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	// Defaults: daily_request_limit=1000, budget=250/day. 1 vault at a
	// 5m interval costs 86400/300=288 lists/day — over budget.
	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
		"refresh_interval":      "5m",
	})
	if err != nil {
		t.Fatalf("write config: unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("5m interval at defaults: resp=%+v, want a guardrail rejection", resp)
	}

	got, rerr := readConfig(t, b, storage)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	if got != nil {
		t.Fatalf("rejected config must not be persisted, got %+v", got.Data)
	}

	// Raising daily_request_limit clears the guardrail (budget=1250).
	resp, err = writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
		"refresh_interval":      "5m",
		"daily_request_limit":   5000,
	})
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("5m interval with raised daily_request_limit: resp=%+v err=%v, want acceptance", resp, err)
	}
}

func TestPathConfig_TokenRotationSwapsClient(t *testing.T) {
	b, storage := newTestBackend(t)

	var factoryCalls []string
	fakes := map[string]*FakeOPClient{}
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		factoryCalls = append(factoryCalls, token)
		f := NewFakeOPClient()
		fakes[token] = f
		return f, nil
	}

	if _, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "token-a",
	}); err != nil {
		t.Fatalf("write config (token-a): %v", err)
	}
	firstClient := b.client

	if _, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "token-b",
	}); err != nil {
		t.Fatalf("write config (token-b): %v", err)
	}
	secondClient := b.client

	if len(factoryCalls) != 2 || factoryCalls[0] != "token-a" || factoryCalls[1] != "token-b" {
		t.Fatalf("factoryCalls = %v, want [token-a token-b]", factoryCalls)
	}
	if firstClient == secondClient {
		t.Errorf("client was not swapped on token rotation")
	}
	if secondClient != fakes["token-b"] {
		t.Errorf("backend client is not the fake built for the rotated token")
	}
}

func TestPathConfig_InvalidPathSplitRejected(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"path_split":            "(unclosed",
	})
	if err != nil {
		t.Fatalf("write config: unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("invalid path_split: resp=%+v, want an error response", resp)
	}
}

func TestPathConfig_RelativeProbeCmdRejected(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"ratelimit_probe_cmd":   "op",
	})
	if err != nil {
		t.Fatalf("write config: unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("relative ratelimit_probe_cmd: resp=%+v, want an error response", resp)
	}

	resp, err = writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"ratelimit_probe_cmd":   "/usr/local/bin/op",
	})
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("absolute ratelimit_probe_cmd: resp=%+v err=%v, want acceptance", resp, err)
	}
}

func TestPathConfig_AlwaysFreshPatternValidation(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	resp, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"always_fresh":          "no-slash-here",
	})
	if err != nil {
		t.Fatalf("write config: unexpected error: %v", err)
	}
	if resp == nil || !resp.IsError() {
		t.Fatalf("malformed always_fresh entry: resp=%+v, want an error response", resp)
	}

	resp, err = writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"always_fresh":          "Infra/some-item",
	})
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("well-formed always_fresh entry: resp=%+v err=%v, want acceptance", resp, err)
	}
}

func TestPathConfig_WriteClearsReplicaNegativeCache(t *testing.T) {
	b, storage := newTestBackend(t)
	b.clientFactory = func(ctx context.Context, token string) (OPClient, error) {
		return NewFakeOPClient(), nil
	}

	if _, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	b.mu.Lock()
	r := newVaultReplica("v1")
	b.replicas["v1"] = r
	b.mu.Unlock()

	now := time.Now()
	r.negativeCacheStore("missing-item", now)
	if !r.negativeCacheHit("missing-item", 30*time.Second, now) {
		t.Fatalf("negative cache entry should be present before rewrite")
	}

	if _, err := writeConfig(t, b, storage, map[string]interface{}{
		"service_account_token": "tok2",
		"vaults":                "Infra",
	}); err != nil {
		t.Fatalf("write config (rewrite): %v", err)
	}

	if r.negativeCacheHit("missing-item", 30*time.Second, now) {
		t.Errorf("config write should have cleared the replica's negative cache")
	}
}
