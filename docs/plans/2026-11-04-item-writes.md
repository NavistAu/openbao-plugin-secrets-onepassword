# Item writes through the engine

Status: approved by the operator 2026-11-04. Implementer: follow this plan; ask before changing its decisions.

## Why

1Password scopes a service account to a whole vault, so any write-capable token is the whole vault.
The engine becomes the only holder of the vault's read-write token; callers get **bao policies on
`op/item/<vault>/<item>` paths**, which scope to one item. A kit's tofu or runbook writes the one item
it owns, with a short-lived bao token, and never sees a 1Password token.

## What changes

### 1. `item/<vault>/<item>`: create, update, delete

- **`CreateOperation` / `UpdateOperation`** (OpenBao routes `bao write` to create when the path does
  not exist per `ExistenceCheck`, else update; implement `ExistenceCheck` against the replica).
  Both are an **upsert by address**: the item segment is an ID, an exact title, or a path_split
  address, resolved as reads resolve it.
  - Existing item: fetch it fresh (one GetAll), apply the request's fields over it, `Items().Put`.
    Fields and sections not named in the request are kept (a write never drops data it was not
    told about).
  - No item: `Items().Create` with the request's title (the address's last form, a title; an ID
    address that does not exist is an error, not a create).
  - Request body: `category` (create only; default `login`; ignored on update with a warning if it
    differs), `fields` as a map `label -> value` (a `<section>/<label>` key targets a section field,
    creating the section on create), optional `field_types` map `label -> concealed|text|url|...`
    (default: `concealed` for `password`/`credential`/any label already concealed, else `text`),
    `notes`, `tags`, `url`. Values arrive as strings; never log them.
  - Response: the item's id, title, version, updated_at. **No field values in the response.**
- **`DeleteOperation`**: `Items().Delete` (not archive). Deleting an absent item is success (idempotent).
- **Allowlist:** a new config field `write_vaults` (names or IDs, default empty). A write to a vault
  outside it is refused with a clear error. `vaults` (reads) is unchanged; `write_vaults` must be a
  subset of `vaults`. Empty `write_vaults` = the engine is read-only, exactly as today.
- **Governor:** every write spends from the same budget and goes through the governor (classify
  rate-limit/auth errors the same way, surface 429 as a retryable error). A write is never deferred
  silently: if the governor would defer, the write fails with the reason.
- **Replica coherence:** after a successful write or delete, update that item in the replica from
  the SDK's returned item (no extra fetch), so an immediate read returns the new value; delete
  removes it. A concurrent delta cycle must not resurrect a deleted item or regress a newer version
  (compare `version`).

### 2. `field/<vault>/<item>/<field>`: update

`UpdateOperation` with body `value` (and optional `type`): updates that one field of an existing
item (upsert semantics on the field, not on the item: an absent item is an error). Same allowlist,
governor, coherence. This is what runbooks use for a rotated credential.

### 3. Client interface

Extend `OPClient` with `CreateItem`, `PutItem`, `DeleteItem` (thin over `Items().Create/Put/Delete`),
and the fake client with the same, recording calls so tests can assert spend and arguments.

### 4. Docs

README: the write paths, `write_vaults`, the policy shape callers use (one item per path), error
table entries. CHANGELOG entry; bump VERSION minor.

## Tests (backend/, against the fakes)

Create on absent title; update keeps unnamed fields; section-field create/update; field update;
delete idempotent; write outside `write_vaults` refused; `write_vaults` not subset of `vaults`
refused at config; ID address absent → error; read after write returns the new value with no extra
fetch; delta cycle after a write does not regress it; governor-deferred write fails loudly; no value
ever in a response or log line (assert on captured logs).

## Out of scope

Documents/files, item moves between vaults, archive, OTP fields, anything in 29c (separate work).

## Delivery

Branch off `develop` in a worktree. `go test ./...` and `go vet ./...` in `backend/` pass.
Do not push to `origin` (GitHub, public); pushing to `forge` (git.29c.sh) is fine and runs its CI.
