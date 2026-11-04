# openbao-plugin-secrets-onepassword

[![CI](https://github.com/NavistAu/openbao-plugin-secrets-onepassword/actions/workflows/ci.yml/badge.svg)](https://github.com/NavistAu/openbao-plugin-secrets-onepassword/actions/workflows/ci.yml)
[![License: MPL-2.0](https://img.shields.io/badge/License-MPL--2.0-blue.svg)](https://opensource.org/licenses/MPL-2.0)

`op` is an [OpenBao](https://openbao.org/) secrets engine that serves
[1Password](https://1password.com/) items live, through 1Password's
service-account Go SDK. It holds no durable copy of item data: values
are cached in an in-memory replica, refreshed on a delta cycle, and
refetched from 1Password whenever a plugin process restarts.

## Why this exists

1Password already secures the secret data. What it does not give you
is OpenBao's audit log, ACL model, and dynamic-secret ecosystem sitting
in front of that data. This engine puts a live 1Password vault behind
an OpenBao mount: every read is ACL-gated and audit-logged by OpenBao,
without OpenBao ever taking custody of the underlying secret. Nothing
written to `bao`'s storage backend needs to be trusted with 1Password
item contents, because nothing is written there — the replica lives in
process memory only and is rebuilt from 1Password on every cold start.

That second-order benefit is the point: teams that already manage
access through OpenBao policy get the same governance over
1Password-origin secrets, without copying the data out of 1Password
and without granting 1Password credentials directly to every
consumer.

## Terms

| Term | Meaning here |
| --- | --- |
| item | A 1Password item: a credential, note, or other record with fields, optional sections, and tags. |
| vault (1Password) | A 1Password container for items. Distinct from an OpenBao mount. |
| mount (OpenBao) | The OpenBao path where this engine is enabled (for example `op/`). Distinct from a 1Password vault. |
| replica | This engine's in-memory copy of one allowlisted vault's items. Rebuilt from 1Password on every cold start; never persisted to OpenBao storage. |
| field | A single named value inside an item, such as a password or username. |

## How it works

```mermaid
flowchart LR
    client[OpenBao client] -->|ACL-checked request| openbao[OpenBao core]
    openbao -->|dispatch| engine[op secrets engine]
    engine -->|read| replica[In-memory replica]
    engine -->|delta cycle| sdk[1Password service-account SDK]
    sdk -->|list / get items| onepassword[1Password]
    sdk -.->|refreshes| replica
```

Every read (`item/…`, `field/…`, `vaults/…`) is served from the
replica. If the replica's data for that vault is outside its freshness
window, the engine first runs a delta cycle: it lists changed items
since the last cycle and fetches only those, then serves the read from
the updated replica. A background timer also runs this delta cycle on
a fixed interval per vault, independent of reads, so the replica stays
warm even without traffic.

## When to use this

Use this engine when consumers already authenticate to OpenBao and you
want OpenBao ACLs and audit logging over secrets that live in
1Password, without copying those secrets into OpenBao's storage
backend or handing out 1Password credentials directly.

Do not use this engine as a general 1Password client, as a way to
migrate secrets permanently into OpenBao (it never stores item data
durably), or where sub-second freshness on every single read is
required regardless of request budget — the replica model trades some
staleness for a bounded, predictable 1Password API request rate.

## Requirements

- An OpenBao server. Built and tested against `openbao/sdk/v2` v2.6.2
  and `openbao/api/v2` v2.6.0.
- A 1Password service-account token with read access to the vaults
  you allowlist.
- Go 1.25 to build from source.

## Installation

### From a release tarball

1. Download the tarball and its checksum file for your platform from
   the [Releases](https://github.com/NavistAu/openbao-plugin-secrets-onepassword/releases)
   page. Releases publish `linux_amd64` and `linux_arm64` builds only.
2. Verify the checksum before use:
   ```sh
   sha256sum -c openbao-plugin-secrets-onepassword_<version>_linux_amd64.tar.gz.sha256
   ```
3. Extract the tarball and copy the `openbao-plugin-secrets-onepassword`
   binary into your OpenBao plugin directory.

A release tarball needs no Go toolchain on the target host, but it
only covers linux/amd64 and linux/arm64. Other platforms must build
from source.

### From source

```sh
go build -o openbao-plugin-secrets-onepassword ./cmd/openbao-plugin-secrets-onepassword
```

Building from source works on any platform Go supports, but requires
a Go 1.25 toolchain on the build host and does not benefit from a
release's pre-computed checksum — compute your own with `sha256sum`.

### Register and mount

```sh
SHA256=$(sha256sum openbao-plugin-secrets-onepassword | awk '{print $1}')
bao plugin register -sha256="$SHA256" \
  -command=openbao-plugin-secrets-onepassword secret op
bao secrets enable -path=op op
```

Write the configuration with the service-account token piped through
stdin, never through an argument, so the token never appears in a
process listing:

```sh
printf '%s' "$OP_SERVICE_ACCOUNT_TOKEN" | bao write op/config \
  service_account_token=- vaults=Infra refresh_interval=15m
```

## Configuration reference

All fields are written to `op/config`. `service_account_token` is
required on every write; every other field defaults if omitted. A
write always replaces the whole configuration.

| Field | Type | Default | Effect |
| --- | --- | --- | --- |
| `service_account_token` | string | none, required | Concealed on read. Rewriting it rotates the engine's 1Password client. |
| `vaults` | comma-separated strings | none | Allowlisted 1Password vault names or IDs this engine serves. |
| `write_vaults` | comma-separated strings | none | Subset of `vaults` the engine may create, update, and delete items in. Empty means the engine is read-only. |
| `refresh_interval` | duration | `15m` | Interval between delta cycles per vault. |
| `daily_request_limit` | integer | `1000` | Sets the daily budget the usage-ceiling calculation uses. |
| `hourly_read_limit` | integer | `1000` | Sets the hourly budget the usage-ceiling calculation uses. |
| `passthrough` | boolean | `true` | Serve reads within the freshness window without waiting for the periodic cycle. |
| `passthrough_ceiling_pct` | integer | `25` | Usage ceiling, as a percent of configured limits, above which passthrough fresh-fetches stop. |
| `passthrough_ttl` | duration | `1m` | Per-vault freshness window. `0` means every read triggers a delta cycle. |
| `serve_stale` | boolean | `true` | Serve stale replica data during a 1Password outage instead of failing the read. |
| `negative_cache_ttl` | duration | `30s` | How long the engine caches an unknown miss before retrying. |
| `path_split` | string | empty | Optional delimiter or regex that splits item titles into path segments. Empty means flat titles. |
| `always_fresh` | comma-separated strings | none | `vault/title` entries that bypass the freshness window on every read. |
| `ratelimit_probe_cmd` | string | empty | Optional absolute path to a pinned `op` binary for usage probing. |

A write is rejected if any of the following hold:

- `service_account_token` is empty.
- A `write_vaults` entry is not listed (string-equal) in `vaults`.
- `refresh_interval` is not positive.
- `daily_request_limit` is not positive.
- `hourly_read_limit` is not positive.
- `passthrough_ceiling_pct` is outside 0–100.
- `passthrough_ttl` is negative.
- `negative_cache_ttl` is negative.
- `ratelimit_probe_cmd` is set but not an absolute path.
- `path_split` is set but does not compile as a regex.
- An `always_fresh` entry is not shaped `vault/title`.
- The steady-state list cost (vault count × 86400 ÷ refresh_interval
  seconds) exceeds 25% of `daily_request_limit` (`checkGuardrail` in
  `backend/config.go`).

## Errors and log messages

The engine itself keeps no logger; every failure surfaces as an API
error or as a field on `op/status`. The plugin's process wrapper
(`cmd/openbao-plugin-secrets-onepassword`) logs two startup/shutdown
conditions:

| Log message | When it appears |
| --- | --- |
| `failed to parse flags`. | The process received flags OpenBao's plugin launcher did not set. |
| `plugin shutting down`. | The plugin's gRPC server returned, ending the process. |

The engine's own failures surface as API-response error text, not as
log lines:

| Error message | Meaning |
| --- | --- |
| `op: not found`. | Nothing in the replica matches this vault, item, or field. |
| `op: ambiguous title`. | Two or more items share this title. Use the item ID or `path_split` instead. |
| `op: replica empty, cold start incomplete`. | The plugin restarted and has not finished its first delta cycle yet. |
| `op: replica data exceeds staleness bound and serve_stale is disabled`. | The replica is too old. `serve_stale` is off, so the read failed. |
| `op: 1password client unavailable, retrying`. | Config is loaded, but the 1Password client is not built yet. The engine is retrying. |
| `op: engine not configured`. | No configuration has been written to `op/config` yet. |
| `op: request gate denied cycle`. | The rate governor deferred this cycle to protect the usage ceiling. |
| `op: vault "…" is not writable: it is not in write_vaults`. | A write targeted a vault outside `write_vaults` (the default, empty list, is read-only). |
| `rate limit quota exceeded` (HTTP 429): `op: write deferred by the rate governor …` or `1Password rate limit exceeded`. | The governor would defer the write, or 1Password returned 429. Writes are never queued; retry later. |
| `op: write refused: 1Password auth failed …`. | The governor is in `auth_failed`; rewrite `op/config` with a valid token. |
| `op: not found: item ID "…" does not exist (an ID address never creates an item)`. | A write addressed an ID-shaped item (26 lowercase alphanumerics) that is not in the vault. |
| `op: cannot create an item with a multi-segment address; use a single-segment title`. | A create used an address containing `/`. |
| `op: 1Password write failed: …`. | The SDK call failed; the SDK's message follows. This engine never adds field values to it. |
| `service_account_token is required`. | A config write omitted the required token field. |
| `config rejected: steady-state list cost … exceeds … of daily_request_limit …`. | Vault count × 86400 ÷ refresh_interval seconds exceeds 25% of `daily_request_limit` (`checkGuardrail`). Raise `daily_request_limit` or `refresh_interval` instead. |

## Writing items

Writes are off until `write_vaults` names a vault. With `write_vaults`
empty the engine is read-only, exactly as before. The engine is then
the only holder of the vault's read-write 1Password token, and callers
get OpenBao policies scoped to one item:

```hcl
path "op/item/Infra/rollothermal" { capabilities = ["create", "update", "read"] }
path "op/field/Infra/rollothermal/*" { capabilities = ["update"] }
```

`op/item/<vault>/<item>` takes create and update (an upsert by address:
an ID, exact title, or `path_split` address; `create` vs `update`
follows whether the replica knows the item) and delete (`Items().Delete`,
not archive; deleting an absent item succeeds).

| Body field | Meaning |
| --- | --- |
| `category` | Create only; default `login`. On update a differing value is ignored with a response warning. |
| `fields` | Map of `label` (or `<section>/<label>`) to string value. Merged over the item; fields and sections not named are kept. A missing section is created. |
| `field_types` | Map of label to `concealed`, `text`, `url`, `email`, `phone`, `date`, `monthyear`, `menu`, `reference`. Default: `concealed` for `password`/`credential`, otherwise `text`; an existing field keeps its type. |
| `notes`, `tags`, `url` | Optional; `tags` replaces, `url` sets the primary website. |

The response is `id`, `title`, `version`, `updated_at`, never field
values. Create is by title only: an address shaped like an item ID that
does not exist is an error, and a create address must be a single
segment.

```sh
# create (JSON body, since fields is a map)
bao write op/item/Infra/rollothermal - <<'JSON'
{"category": "login", "fields": {"username": "rollo", "password": "s3cret", "api/token": "t0k"}, "tags": "iac"}
JSON
# update: only the named fields change
bao write op/item/Infra/rollothermal - <<'JSON'
{"fields": {"password": "n3w"}}
JSON
# rotate one field
bao write op/field/Infra/rollothermal/password value=n3w2
# delete
bao delete op/item/Infra/rollothermal
```

`op/field/<vault>/<item>/<field>` (and `<section>/<field>`) takes
`value` and optional `type`: the field is created or updated, but the
item must exist.

An update spends two 1Password requests (a fresh `GetAll`, then
`Put`); create and delete spend one. All come out of the same budget as
reads, through the governor. A write is never deferred silently: if the
governor would defer it, it fails with the reason (HTTP 429 for rate
limits). After a successful write the replica takes the SDK's returned
item with no extra fetch, a deleted item is tombstoned, and a delta
cycle never regresses an item to an older `version`.

## Troubleshooting

- Read `op/status`. It reports per-vault item counts, last refresh
  time, consecutive failure counts, rate-governor state, and probe
  health — with no secret material.
- If a read fails with `op: replica empty, cold start incomplete`,
  wait for the next delta cycle or check `op/status` for
  `client_init_failures` and `client_init_last_err`.
- If a read fails with `op: replica data exceeds staleness bound and
  serve_stale is disabled`, either set `serve_stale=true` or resolve
  the underlying 1Password connectivity or auth problem so the next
  cycle can succeed.
- If `op/status` shows a nonzero `governor.client_init_failures` or a
  `rate_limited`/`auth_failed` state, check `client_init_last_err`
  for the 1Password SDK's own error text.
- If configured `always_fresh` entries appear in
  `always_fresh_unmatched`, the pattern matched no item in any
  allowlisted vault; check the vault name and item title.

## Operations

- To force a refetch without waiting for the next cycle, write to
  `op/refresh` (all vaults) or `op/refresh/<vault>`.
- To clear cached state without spending a 1Password request, write to
  `op/invalidate` (all vaults) or `op/invalidate/<vault>`. See Design
  constraints below for what invalidation does and does not cost.

## Design constraints

- No durable copy of 1Password item data. The replica lives in process
  memory only; a plugin restart means a full cold refetch, never a
  read from OpenBao storage.
- Delta cycles, not full syncs. After the first cold fetch, each cycle
  lists changed items since the last cycle and fetches only those, to
  keep 1Password API usage bounded and predictable.
- A rate governor sits in front of every 1Password-spending operation.
  It tracks rolling usage against the configured hourly and daily
  limits, classifies 1Password errors as rate-limited, auth-failed, or
  other, and applies exponential backoff independently for delta
  cycles and client construction.
- Passthrough reads and the periodic background cycle share the same
  budget and the same governor; passthrough and periodic work is the
  first to be deferred when usage approaches the configured ceiling,
  while a miss-triggered lookup or a manual `op/refresh` is not.
- Writes need `write_vaults`; the engine's token is read-write for the
  whole vault, so access is scoped by OpenBao policy on
  `op/item/<vault>/<item>` paths. Field values are never logged or
  returned from a write.
- Zero-spend invalidation (`op/invalidate`) exists specifically to let
  an operator expire a freshness window without touching the
  1Password request budget; the actual refetch cost lands on whatever
  read or scheduled cycle touches that vault next.

## Tests

`go test ./...` in `backend/` is the authoritative suite: it runs
against fakes for both the 1Password SDK and the OpenBao storage
backend, so it is deterministic and needs no live 1Password account or
running OpenBao server. `bench/` holds separate, manual scripts for
exercising the engine against a real `bao` server and a real
1Password service account; it is not run in CI and is not a substitute
for the `backend/` suite.

A private-forge Woodpecker CI also runs `go build ./...` and
`go test ./...` on every push, alongside the GitHub Actions workflow
above.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the build and test commands,
the branch model, and the bar for adding new configuration options. This
project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Security

Report vulnerabilities through GitHub private security advisories on
this repository. See [SECURITY.md](SECURITY.md) for the full policy.

## License

MPL-2.0. See [LICENSE](LICENSE).
