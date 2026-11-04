# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Any change affecting secret-data handling or the replica's consistency
model (what gets cached, for how long, and under what staleness
guarantees) is always called out explicitly in this changelog, even
when it is otherwise a minor or patch change.

## [Unreleased]

## [0.2.0-rc.2] - 2026-11-04

### Fixed

- A write's `<section>/<label>` field key splits at the last `/`, so a
  section whose title holds a `/` can be written.

## [0.2.0-rc.1] - 2026-11-04

### Added

- Item writes. `op/item/<vault>/<item>` accepts create/update (upsert by
  address; fields merged over the existing item) and delete;
  `op/field/<vault>/<item>/<field>` accepts update. New config field
  `write_vaults` (subset of `vaults`) gates them; empty keeps the engine
  read-only. Writes go through the rate governor and share its budget;
  a deferred write fails loudly instead of queueing.

### Changed

- Replica consistency: after a successful write the replica is updated
  from the SDK's returned item (no re-fetch), deletes are tombstoned,
  and a delta cycle no longer replaces a held item with an older
  `version`. Write responses and logs never contain field values.

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
