package backend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
)

// configStorageKey is the single storage entry backing `op/config`.
const configStorageKey = "config"

// Defaults per spec §3 config row. Duration defaults are strings so
// they can be reused verbatim as framework.FieldSchema.Default values
// (framework parses them the same way it parses request input).
const (
	defaultRefreshInterval       = "15m"
	defaultPassthroughTTL        = "1m"
	defaultNegativeCacheTTL      = "30s"
	defaultDailyRequestLimit     = 1000
	defaultHourlyReadLimit       = 1000
	defaultPassthroughCeilingPct = 25
	defaultPassthrough           = true
	defaultServeStale            = true
)

// guardrailFraction is the spec §4 budget guardrail: config is
// rejected if the steady-state list cost alone would exceed this
// fraction of daily_request_limit.
const guardrailFraction = 0.25

// Config is the persisted `op/config` entry (spec §3). A write always
// replaces it wholesale — service_account_token is required on every
// write, so there is no partial-update/merge-with-previous case to
// handle (token rotation is simply "write the whole config again").
type Config struct {
	ServiceAccountToken   string        `json:"service_account_token"`
	Vaults                []string      `json:"vaults"`
	RefreshInterval       time.Duration `json:"refresh_interval"`
	DailyRequestLimit     int           `json:"daily_request_limit"`
	HourlyReadLimit       int           `json:"hourly_read_limit"`
	Passthrough           bool          `json:"passthrough"`
	PassthroughCeilingPct int           `json:"passthrough_ceiling_pct"`
	PassthroughTTL        time.Duration `json:"passthrough_ttl"`
	ServeStale            bool          `json:"serve_stale"`
	NegativeCacheTTL      time.Duration `json:"negative_cache_ttl"`
	// PathSplit is a delimiter or regex (D13): treating a plain
	// delimiter like "__" as a (literal-character) regex means one
	// compile+split code path covers both cases described in the
	// spec ("optional delimiter/regex") without a separate mode
	// flag. Empty means flat titles.
	PathSplit string `json:"path_split"`
	// AlwaysFresh holds "vault/title" entries (D14). The spec calls
	// these "patterns" but describes only exact raw-title/ID matching
	// (§4), so validation here checks shape (one non-empty "/" split)
	// rather than treating them as globs.
	AlwaysFresh       []string `json:"always_fresh"`
	RatelimitProbeCmd string   `json:"ratelimit_probe_cmd"`
}

// validate applies every spec §3 rejection rule, guardrail included.
// Order matters: cheap field-shape checks run before the guardrail
// arithmetic, which assumes RefreshInterval and DailyRequestLimit are
// already known-positive.
func (c *Config) validate() error {
	if c.ServiceAccountToken == "" {
		return errors.New("service_account_token is required")
	}
	if c.RefreshInterval <= 0 {
		return errors.New("refresh_interval must be positive")
	}
	if c.DailyRequestLimit <= 0 {
		return errors.New("daily_request_limit must be positive")
	}
	if c.HourlyReadLimit <= 0 {
		return errors.New("hourly_read_limit must be positive")
	}
	if c.PassthroughCeilingPct < 0 || c.PassthroughCeilingPct > 100 {
		return errors.New("passthrough_ceiling_pct must be between 0 and 100")
	}
	if c.PassthroughTTL < 0 {
		return errors.New("passthrough_ttl must not be negative (0 means always-fresh)")
	}
	if c.NegativeCacheTTL < 0 {
		return errors.New("negative_cache_ttl must not be negative")
	}
	if c.RatelimitProbeCmd != "" && !filepath.IsAbs(c.RatelimitProbeCmd) {
		return fmt.Errorf("ratelimit_probe_cmd must be an absolute path, got %q", c.RatelimitProbeCmd)
	}
	if c.PathSplit != "" {
		if _, err := regexp.Compile(c.PathSplit); err != nil {
			return fmt.Errorf("path_split: invalid delimiter/regex: %w", err)
		}
	}
	for _, pat := range c.AlwaysFresh {
		vault, title, found := strings.Cut(pat, "/")
		if !found || vault == "" || title == "" {
			return fmt.Errorf("always_fresh: invalid pattern %q, want \"vault/title\"", pat)
		}
	}
	return c.checkGuardrail()
}

// checkGuardrail rejects a config whose steady-state list cost alone
// (vault_count × 86400/interval_s) exceeds guardrailFraction of
// daily_request_limit (spec §4). An empty allowlist has zero list
// cost and always passes — the guardrail is a rate decision about the
// vaults actually configured, not a precondition for configuring any.
func (c *Config) checkGuardrail() error {
	if len(c.Vaults) == 0 {
		return nil
	}
	listCostPerDay := float64(len(c.Vaults)) * (86400.0 / c.RefreshInterval.Seconds())
	budget := guardrailFraction * float64(c.DailyRequestLimit)
	if listCostPerDay > budget {
		return fmt.Errorf(
			"config rejected: steady-state list cost %.0f requests/day (%d vaults × 86400s/%ds refresh_interval) exceeds %.0f%% of daily_request_limit %d (budget %.0f); raise daily_request_limit or refresh_interval (spec §4 guardrail)",
			listCostPerDay, len(c.Vaults), int(c.RefreshInterval.Seconds()), guardrailFraction*100, c.DailyRequestLimit, budget,
		)
	}
	return nil
}

// getConfigFromStorage reads `op/config` fresh from storage — the
// source of truth for reads, independent of whatever the backend has
// cached in memory. Takes a bare logical.Storage (rather than a
// *logical.Request) so it's callable from contexts that don't have a
// full Request, e.g. the Initialize hook (Task 7 cold start), which
// only gets a *logical.InitializationRequest.
func getConfigFromStorage(ctx context.Context, storage logical.Storage) (*Config, error) {
	entry, err := storage.Get(ctx, configStorageKey)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}
	var cfg Config
	if err := entry.DecodeJSON(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyConfig validates cfg, persists it, and reinitializes the
// runtime state that derives from it: the OPClient (token rotation =
// client swap, via the injectable clientFactory so tests can observe
// re-init) and the compiled path_split regex. It also clears every
// vault replica's negative cache (spec §4: "config writes clear it
// too"). Nothing is touched — storage included — if validation fails.
func (b *Backend) applyConfig(ctx context.Context, req *logical.Request, cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	var pathSplitRegex *regexp.Regexp
	if cfg.PathSplit != "" {
		re, err := regexp.Compile(cfg.PathSplit)
		if err != nil {
			// Unreachable given validate() above; guarded defensively
			// rather than silently swallowing a compile error.
			return fmt.Errorf("path_split: invalid delimiter/regex: %w", err)
		}
		pathSplitRegex = re
	}

	client, err := b.clientFactory(ctx, cfg.ServiceAccountToken)
	if err != nil {
		return fmt.Errorf("initializing 1password client: %w", err)
	}

	entry, err := logical.StorageEntryJSON(configStorageKey, cfg)
	if err != nil {
		return err
	}
	if err := req.Storage.Put(ctx, entry); err != nil {
		return err
	}

	b.mu.Lock()
	b.config = cfg
	b.client = client
	b.pathSplitRegex = pathSplitRegex
	for _, r := range b.replicas {
		r.clearNegativeCache()
	}
	b.mu.Unlock()

	// spec §4 Failure behaviour: auth_failed clears only on a
	// successful config rewrite; reconfigure also refreshes the
	// governor's limits/probe-cmd/token for the new config (governor.go).
	b.gate.reconfigure(cfg)

	return nil
}

// configResponseData builds the `op/config` read response.
// service_account_token is intentionally absent — concealed on read
// per spec §3.
func configResponseData(cfg *Config) map[string]interface{} {
	return map[string]interface{}{
		"vaults":                  cfg.Vaults,
		"refresh_interval":        cfg.RefreshInterval.String(),
		"daily_request_limit":     cfg.DailyRequestLimit,
		"hourly_read_limit":       cfg.HourlyReadLimit,
		"passthrough":             cfg.Passthrough,
		"passthrough_ceiling_pct": cfg.PassthroughCeilingPct,
		"passthrough_ttl":         cfg.PassthroughTTL.String(),
		"serve_stale":             cfg.ServeStale,
		"negative_cache_ttl":      cfg.NegativeCacheTTL.String(),
		"path_split":              cfg.PathSplit,
		"always_fresh":            cfg.AlwaysFresh,
		"ratelimit_probe_cmd":     cfg.RatelimitProbeCmd,
	}
}
