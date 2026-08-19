# openbao-plugin-secrets-onepassword

A custom OpenBao secrets engine that serves 1Password items through the
1Password service-account Go SDK. It reads 1Password vaults live (with an
in-memory, delta-refreshed replica for freshness and rate-limit economy),
giving OpenBao ACL and audit coverage over 1Password-origin secrets without
copying item data into OpenBao's own storage.

Status: pre-alpha, spec-driven.

May work with HashiCorp Vault — untested, unsupported.

## License

MPL-2.0. See `LICENSE`.

## Bench gate record (fallback-scoped)

Task 8 bench gate run 2026-08-05. **Fallback-scoped, NOT a full spec-§7
PASS**: per operator decision 2026-08-05 (plan "Operator inputs" item 2),
this run used the **estate SA + Infra vault** (existing production
service account, read-only against the engine), not a dedicated
bench SA. All engine reads were against real Infra items (read-only,
183 pre-existing items untouched); the only 1P writes were a
disposable test item (see below), which was created, out-of-band
edited, and deleted within this run — never any existing item.

Local: scratch `ghcr.io/openbao/openbao:2.5.5` container (`bao-bench`),
server mode (file storage, `bench/config/bao.hcl`), plugin
cross-compiled `linux/arm64` (`bench/build.sh`), registered under
catalog name `op` (sha256
`fb95c7829cb635cefbc6ee44a02ff7cf4197436892a8873f2274eab764a135e9`),
mounted at `op/`. Scripts: `bench/build.sh`, `bench/setup.sh`,
`bench/cleanup.sh`. Disposable item:
`bench.openbao-plugin-secrets-onepassword__disposable` (vault Infra,
id `imzwip74amad2b6zpdnxorvnk4`) — created, out-of-band edited, and
deleted (confirmed via `op item get` failing afterward) within this
run.

### Overall verdict: **PASS** (as of the 2026-08-05 fix + step-7 re-run)

Original run (2026-08-05, commit `d7a5955`): **FAIL** — one
reproducible engine bug found in step 7 (below); steps 1–6 and step
7's first half all PASS. Bench matrix stopped per Task 8 instructions
on confirming a genuine engine bug (not a bench-script issue) — not
patched ad hoc.

Fix landed the same day (commit `ddce1f6`, "decouple config load from
SDK client construction") and step 7 was re-run live against the same
estate SA + Infra vault to prove the fix, rather than trust it from
unit tests alone. See "Fix + step-7 re-run (2026-08-05): PASS" below
for the fix summary and live transcript. Steps 1–6 and 8 were not
re-run (unaffected by the bug or the fix; their original PASS results
still hold) — this re-run is scoped to step 7 only.

### Results table

| # | Step | Verdict | Notes |
|---|---|---|---|
| 1 | Register + mount + config write + known-item field read | PASS | sha256-verified registration; token concealed on read; field read of disposable item returned correct value |
| 2 | GetAll accounting re-confirmation (cold start) | PASS* | Full 183/184-item Infra vault materialized via PeriodicFunc within ≤10s of config write. *See "ratelimit reporting lag" below — per-step deltas from `op service-account ratelimit` were not reliable; cumulative session totals were used instead and are consistent with spec §11's order of magnitude. |
| 3 | PeriodicFunc delta refresh (out-of-band edit) | PASS | Disposable item's `marker` field edited out-of-band (v1→v2-outofband) via `op item edit`; window-fresh reads kept serving the old value; the next periodic cycle (refresh_interval=1m) picked up the change within ~2 cycles and the engine served the new value, `updated_at` matching the edit timestamp |
| 4a | Passthrough: window-fresh read | PASS | 0 requests, served instantly (`replica_age_seconds` ≈ 0) |
| 4b | Passthrough: expired-window read | PASS | Isolated from periodic (`refresh_interval=6m`, `passthrough_ttl=5s`); read past the window triggered exactly one delta cycle |
| 4c | Passthrough: `passthrough_ttl=0` (always-fresh) | PASS | Two consecutive reads triggered two distinct delta cycles (`last_refresh` advanced both times) |
| 4d | `invalidate` (zero-spend) | PASS | `op/invalidate/Infra`: zero requests, item data preserved, `stale_suspect`+`invalidated_at` set, `last_refresh` zeroed; next read triggered the recovery cycle and cleared `stale_suspect` |
| 5 | Serve-stale outage (black-holed 1password.com) | PASS | Known-item read served stale (`stale: true`, correct cached value/`updated_at`, accurate `replica_age_seconds`); unknown-item read failed fast (~0.15s, `op: not found`, no hang); `status` showed `consecutive_failures` incrementing, governor state correctly stayed `normal` (not `rate_limited`/`auth_failed` — a plain connectivity failure) |
| 6 | `kill -9` plugin process, network healthy | PASS | bao detected the exit; respawn was lazy (on next request), that request blocked ~7.8s for process spawn + full cold refetch; `item_count` restored to 184, `consecutive_failures` reset to 0 |
| 7a | Cold-start-during-outage: explicit error | PASS | Killing the plugin while black-holed produced `op: replica empty, cold start incomplete` on every read, fast (~150ms–2s), no hang; `status` showed an empty `vaults` map |
| 7b | Cold-start-during-outage: recovery after black-hole removed | **FAIL (original run) → PASS (2026-08-05 re-run)** | **Original**: recovery did not happen automatically — stuck for 7+ minutes of continuous polling with connectivity fully restored; only a manual `op/config` rewrite (identical values) forced recovery (~25s later, one periodic-tick cadence). See "Bug found" below. **Re-run (fix commit `ddce1f6`)**: black-hole removed at `08:43:24Z` with no config rewrite and no restart; client + vault directory rebuilt automatically by `08:44:18Z` (~54s, one periodic tick), full 183-item replica materialized by `08:45:02Z` (~98s, two ticks); a known item read immediately after confirmed a correct, non-stale serve. See "Fix + step-7 re-run" below. |
| 8 | RSS + cold-start timing | PASS (data captured) | Plugin RSS at idle, full 184-item replica: **74,020 KB VmRSS** (~72 MiB); container total 111.5 MiB (bao + plugin). Cold-start wall time (config-write → first populated status): ≤10s (bounded by 5s poll granularity) via PeriodicFunc. Process-restart-triggered cold refetch (step 6, network healthy): **7.8s** (process spawn + full 184-item materialization + first served read). |

### Bug found: cold-start-during-outage never retries automatically

**Root cause** (traced in `backend/backend.go`): `initialize()` calls
`loadPersistedConfig()`, which calls `b.clientFactory(...)` — SDK
client construction, which performs a live network auth handshake
(`POST https://my.1password.com/api/v3/auth/start` — see "SDK
endpoint" below). If that call fails (exactly what happens when the
plugin process restarts during a 1P outage), `loadPersistedConfig`
returns an error and `initialize()` silently swallows it (`return
nil`) **without ever setting `b.config`**. `periodic()` immediately
bails (`if cfg := b.currentConfig(); cfg == nil { return nil }`)
before reaching the `coldStart()` call that's supposed to drive
retries, and nothing else ever calls `loadPersistedConfig` again — the
backend is stuck **permanently, even long after the outage clears**,
until an operator manually rewrites `op/config`.

This contradicts spec §4 Restart ("the engine retries initial load
with the same backoff") and `coldStart`'s own doc comment claiming
periodic's unconditional call handles this retry — that claim is true
for `refreshVaultDirectory`/`runVaultCycle` failures (config already
loaded) but false for a `loadPersistedConfig` failure (config never
loaded in the first place).

**Reproduction**: kill the plugin process while 1password.com is
black-holed for the container, then remove the black-hole. Reads keep
failing with `op: replica empty, cold start incomplete` indefinitely;
`status` keeps showing an empty `vaults` map. A manual `op/config`
rewrite (even with unchanged values) unblocks it within one periodic
tick.

**Evidence**: `bench/scratch/coldstart-outage-known-read.log`,
`bench/scratch/status-coldstart-outage.json`,
`bench/scratch/recovery-read-after-blackhole-removed.json`,
`bench/scratch/config-write-3-full-under-blackhole.log` (client-init
failure showing the exact endpoint/error).

Not fixed as part of this bench run per Task 8 instructions (engine
bugs are reported, not patched mid-bench).

### Fix + step-7 re-run (2026-08-05): PASS

**Fix** (commit `ddce1f6`): `loadPersistedConfig` (`backend/backend.go`)
no longer calls the 1P SDK client factory — it's now a pure storage
read that sets `b.config` unconditionally, so `initialize()` can never
swallow a client-construction failure into a permanently-stalled
backend. Client construction moved to a new lazy/retryable
`ensureClient`, called from `runVaultCycle` and `refreshVaultDirectory`
(the two call sites that actually need a client) — so `periodic()`'s
existing `coldStart()` retry loop now drives client construction too,
not just per-vault delta cycles. A construction failure is classified
by a new `governor.recordClientInitResult` the same way a cycle
failure already is (rate_limited/auth_failed reuse the existing
engine-wide state; a plain connectivity failure — this bug's exact
scenario — paces retries via a new engine-wide `clientInitState`
backoff, mirroring the existing per-vault `vaultBackoff`) and surfaced
on `op/status` as `governor.client_init_failures` /
`governor.client_init_last_err`. Proven first on fakes (`go test
./... -race`, new tests in `backend/coldstart_test.go`:
`TestBackend_Initialize_ClientFactoryFails_ConfigStillLoadsAndStatusShowsIt`,
`TestBackend_ClientFactoryRecovers_NoConfigRewrite`), then live below.

**Live re-run scope**: step 7 only (steps 1–6 and 8 are unaffected by
this bug/fix and were not re-run; their original results above still
hold). Same estate SA + Infra vault, same read-only posture as the
original run — no 1Password item was created, edited, or deleted this
time (183 pre-existing items, read-only). Plugin rebuilt at sha256
`e3897d0cdad53ee62de266b8779e219e6b05e12903bd2a9e6decf00f43f04e24`
(`bench/build.sh`), fresh scratch `bao-bench` container
(`bench/setup.sh`), `op/config` written once against the healthy
network (baseline, ~5s — normal client construction, unaffected by
this fix).

**Procedure** (adapted from the original step 6/7 methodology — same
container-local `/etc/hosts` black-hole for `my.1password.com`,
`events.1password.com`, `b5.1password.com`, since no in-flight cycle
had ever completed yet there was nothing to warm before killing):

1. `08:42:48Z` — black-holed the three hostnames to `127.0.0.1` and
   `kill -9`'d the plugin process (pid 105) in the same breath, so the
   next request's lazy respawn (`Initialize`) would hit the outage
   before ever constructing a client.
2. `08:42:52Z` — `op/status` read (triggers the respawn): confirmed
   config loaded (this is the fix — previously this behavior didn't
   exist to observe) with `governor.client_init_failures: 1` and
   `client_init_last_err` showing the exact refused-connection auth
   handshake failure; `vaults: {}` (cold start incomplete).
3. `08:43:03Z` — `bao list op/vaults/Infra/items`: failed fast (~0.3s)
   with `op: replica empty, cold start incomplete` — the same explicit
   error step 7a already validated, unchanged by this fix.
4. `08:43:24Z` — black-hole removed (`/etc/hosts` restored verbatim).
   **No config rewrite, no restart** from here on.
5. Polled `op/status` every 10s: by `08:44:18Z` (~54s, one periodic
   tick) the client had reconstructed and the vault directory rebuilt
   (`client_init_failures: 0`, vault title populated); by `08:45:02Z`
   (~98s, two ticks) the replica was fully materialized
   (`item_count: 183`).
6. Read a known item (`ups-01.example.com__admin`) immediately
   after: served correctly (`stale: false`, `replica_age_seconds`
   ≈9.4s, zero additional 1P requests — window-fresh passthrough).
7. `bench/cleanup.sh` — container removed (no disposable item existed
   to clean up this run).

**Verdict**: step 7 PASS in full (7a unchanged-PASS, 7b now PASS).
Recovery is fully automatic, matching spec §4 Restart ("the engine
retries initial load with the same backoff") — no operator action
required, closing the gap the original run found.

Evidence: `bench/scratch/rerun-status-under-outage.json`,
`bench/scratch/rerun-coldstart-outage-read.log`,
`bench/scratch/rerun-poll-4.json` (directory-rebuilt state),
`bench/scratch/rerun-poll-5.json` (fully materialized),
`bench/scratch/rerun-recovered-item-read.json`.

### Other findings

- **SDK endpoint, empirically confirmed**: the SDK's session/auth
  bootstrap hits `POST https://my.1password.com/api/v3/auth/start`
  (observed via a live TCP connection capture during a fetch, and
  confirmed definitively by the client-init failure message under
  black-hole). The resolved IPs for `my.1password.com`,
  `events.1password.com`, and `b5.1password.com` overlap (shared
  CDN/LB pool) — a robust black-hole needs to cover all three via
  container-local `/etc/hosts`, not DNS alone.
- **SDK client holds a long-lived connection**: poisoning DNS while
  the client is already warm does not break it immediately — a
  request only fails once the existing connection's idle timeout
  expires (observed ~123s) and a fresh connection is attempted, or the
  client is reconstructed (e.g. via a config rewrite). Relevant to
  anyone re-running or extending this bench.
- **`op service-account ratelimit` reporting lag for SDK-path
  requests**: CLI-driven ops (`op item create`/`edit`) showed up in
  the ratelimit counters within seconds. SDK-driven requests (through
  the engine) sometimes took minutes to appear, and one delta
  (client-init, +1 read) appeared immediately while a subsequent
  cold-start's ~5 reads took several minutes to surface. Per-step
  before/after checkpoints were therefore unreliable for this run;
  only the session-start-vs-session-end cumulative delta is trusted
  (see below). This differs from spec §11's spike, which reported
  apparently-immediate deltas — possibly a standalone-Go-process vs.
  containerized-plugin-subprocess difference, or simply timing luck.
  Worth a note for anyone relying on tight-loop ratelimit monitoring
  in production.
- **Plugin respawn after `kill -9` is lazy**: bao does not proactively
  restart a dead plugin process; the next request to the mount blocks
  for the full respawn + (if config is loaded) cold refetch.

### Live 1Password request budget

Checkpointed via `op service-account ratelimit` before/after (files
`bench/scratch/ratelimit-*.json`, 16 checkpoints total this run).
Session-start vs. session-end cumulative deltas (the only reliable
numbers — see reporting-lag finding above):

| Counter | Start | End | Delta | Configured limit |
|---|---|---|---|---|
| account `read_write` (daily, shared) | 72 | 83 | **+11** | 1,000/day |
| token `read` (hourly) | 2 | 10 | +8 | 1,000/hr |
| token `write` (hourly) | 0 | 3 | +3 | 100/hr |

Well within the ≤80-request bench budget (11 of 80 used, 14%) and
negligible against the account's real daily pool (1.1%). No 2×
overrun of any per-phase expectation was observed; the bench was not
stopped for budget reasons — it was stopped for the engine bug (step
7b).

**2026-08-05 fix re-run (step 7 only)**, budget ≤20 requests
(`bench/scratch/ratelimit-{00,01,02}-rerun-*.txt`, checkpointed
before/immediately-after):

| Counter | Start | End | Delta | Configured limit |
|---|---|---|---|---|
| account `read_write` (daily, shared) | 83 | 84 | +1 | 1,000/day |
| token `read` (hourly) | 0 | 1 | +1 | 1,000/hr |
| token `write` (hourly) | 3 | 3 | +0 | 100/hr |

Same reporting-lag caveat as the original run applies (SDK-path
requests can take minutes to surface): the actual request count this
re-run issued is higher than +2 — one healthy client-construction
handshake at the baseline config write, one recovery handshake, one
`ListVaults`, one `ListItems`, and ~4 chunked `GetItems` calls for the
183-item vault (the outage-time construction attempt itself never
reached 1Password — the container's black-holed `/etc/hosts` refused
the connection locally, so it cost nothing against the account's
quota). Even the un-lagged estimate (~8) is comfortably within the
≤20 budget; no further checkpoints were taken to chase the lag.

### Disposable item lifecycle

1. Created: `op item create` — id `imzwip74amad2b6zpdnxorvnk4`,
   vault Infra, category Secure Note, fields `notesPlain` + `marker`
   (`marker=v1`).
2. Out-of-band edited: `op item edit ... marker[text]=v2-outofband`
   (step 3).
3. Deleted: `bench/cleanup.sh` → `op item delete`, then independently
   verified via `op item get imzwip74amad2b6zpdnxorvnk4 --vault
   Infra`, which returned `isn't an item in the "Infra" vault` —
   confirmed gone (1Password's standard 30-day Recently Deleted
   retention applies; not a permanent-delete API).

No other 1Password item was written, edited, or deleted in this run.

The 2026-08-05 fix re-run (step 7 only, above) created no disposable
item of its own — it needed only a read-only known-item confirmation,
served from the same 183 pre-existing Infra items.

## CI

Woodpecker (the private forge's Woodpecker CI): build + test on push; tagged
releases build cross-platform artifacts and publish to the forge with
checksums. Activated 2026-08-05; the `forgejo_token` secret is
tag-scoped for the release step.
