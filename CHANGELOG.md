# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Any change affecting secret-data handling or the replica's consistency
model (what gets cached, for how long, and under what staleness
guarantees) is always called out explicitly in this changelog, even
when it is otherwise a minor or patch change.

## [0.1.0] - 2026-08-19

### Added

- Initial public release of the `op` OpenBao secrets engine.
- `op/config`: configure the 1Password service-account token, vault
  allowlist, refresh cadence, and passthrough/rate-limit behavior.
- `op/vaults`, `op/vaults/<vault>/items`, `op/item/<vault>/<item>`,
  `op/field/<vault>/<item>/<field>`: list and read 1Password vaults,
  items, and fields through the in-memory, delta-refreshed replica.
- `op/refresh` and `op/refresh/<vault>`: force an immediate delta
  cycle.
- `op/invalidate` and `op/invalidate/<vault>`: zero-spend expiry of a
  vault's freshness window.
- `op/status`: per-vault replica age, item counts, refresh and
  failure state, and rate-governor state, with no secret material.
- A rate governor enforcing configured hourly and daily 1Password API
  usage ceilings, with exponential backoff on rate-limited,
  auth-failed, and connectivity failures.
- No durable copy of 1Password item data: the replica lives in process
  memory only, rebuilt from 1Password on every cold start.
