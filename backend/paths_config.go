package backend

import (
	"context"
	"time"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// pathConfig defines `op/config`. It is a single write-only-as-a-whole
// resource (no Create/Update distinction, no ExistenceCheck): every
// write is a full replacement, which matches service_account_token
// being required on every write (spec §3).
func pathConfig(b *Backend) *framework.Path {
	return &framework.Path{
		Pattern: "config",

		Fields: map[string]*framework.FieldSchema{
			"service_account_token": {
				Type:        framework.TypeString,
				Required:    true,
				Description: "1Password service-account token. Concealed on read; rewriting it rotates the engine's client.",
			},
			"vaults": {
				Type:        framework.TypeCommaStringSlice,
				Description: "Allowlisted 1Password vaults (names or IDs) this engine serves.",
			},
			"refresh_interval": {
				Type:        framework.TypeDurationSecond,
				Default:     defaultRefreshInterval,
				Description: "Delta-cycle interval per vault (spec D5).",
			},
			"daily_request_limit": {
				Type:        framework.TypeInt,
				Default:     defaultDailyRequestLimit,
				Description: "Configured account-wide daily 1Password API request limit.",
			},
			"hourly_read_limit": {
				Type:        framework.TypeInt,
				Default:     defaultHourlyReadLimit,
				Description: "Configured per-token hourly read limit; the burst-brake reference.",
			},
			"passthrough": {
				Type:        framework.TypeBool,
				Default:     defaultPassthrough,
				Description: "Serve reads within a freshness window instead of only on the periodic cycle (spec §4).",
			},
			"passthrough_ceiling_pct": {
				Type:        framework.TypeInt,
				Default:     defaultPassthroughCeilingPct,
				Description: "Usage ceiling (percent of configured limits) above which passthrough fresh-fetches stop.",
			},
			"passthrough_ttl": {
				Type:        framework.TypeDurationSecond,
				Default:     defaultPassthroughTTL,
				Description: "Per-vault freshness window; 0 means always-fresh (every read triggers a delta cycle).",
			},
			"serve_stale": {
				Type:        framework.TypeBool,
				Default:     defaultServeStale,
				Description: "Serve stale replica data during 1Password outages instead of failing reads (spec D6).",
			},
			"negative_cache_ttl": {
				Type:        framework.TypeDurationSecond,
				Default:     defaultNegativeCacheTTL,
				Description: "How long an unknown item/field miss is cached before the next attempt.",
			},
			"path_split": {
				Type:        framework.TypeString,
				Description: "Optional delimiter or regex splitting item titles into path segments (spec D13). Empty means flat titles.",
			},
			"always_fresh": {
				Type:        framework.TypeCommaStringSlice,
				Description: "\"vault/title\" entries that bypass the freshness window on every read (spec D14).",
			},
			"ratelimit_probe_cmd": {
				Type:        framework.TypeString,
				Description: "Optional absolute path to a pinned `op` binary for account-wide usage probing (spec D12).",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathConfigRead,
				Summary:  "Read the op engine configuration (token concealed).",
			},
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathConfigWrite,
				Summary:  "Write the op engine configuration.",
			},
		},

		HelpSynopsis:    "Configure the op secrets engine.",
		HelpDescription: "Configure the 1Password service-account token, vault allowlist, refresh cadence, and rate/passthrough behavior for the op secrets engine (spec §3).",
	}
}

func (b *Backend) pathConfigRead(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	cfg, err := getConfigFromStorage(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return &logical.Response{Data: configResponseData(cfg)}, nil
}

func (b *Backend) pathConfigWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	token, _ := data.Get("service_account_token").(string)
	if token == "" {
		return logical.ErrorResponse("service_account_token is required"), nil
	}

	cfg := &Config{
		ServiceAccountToken:   token,
		Vaults:                data.Get("vaults").([]string),
		RefreshInterval:       time.Duration(data.Get("refresh_interval").(int)) * time.Second,
		DailyRequestLimit:     data.Get("daily_request_limit").(int),
		HourlyReadLimit:       data.Get("hourly_read_limit").(int),
		Passthrough:           data.Get("passthrough").(bool),
		PassthroughCeilingPct: data.Get("passthrough_ceiling_pct").(int),
		PassthroughTTL:        time.Duration(data.Get("passthrough_ttl").(int)) * time.Second,
		ServeStale:            data.Get("serve_stale").(bool),
		NegativeCacheTTL:      time.Duration(data.Get("negative_cache_ttl").(int)) * time.Second,
		PathSplit:             data.Get("path_split").(string),
		AlwaysFresh:           data.Get("always_fresh").([]string),
		RatelimitProbeCmd:     data.Get("ratelimit_probe_cmd").(string),
	}

	if err := b.applyConfig(ctx, req, cfg); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return nil, nil
}
