# Contributing

Thank you for your interest in improving openbao-plugin-secrets-onepassword.
This document covers the build and test workflow, the branch model, the
documentation style rule, and the bar for adding new configuration options.

In one sentence: fork the repo, clone it, branch off `develop`, make your
change, run `gofmt`, `go vet`, and `go test`, and open a pull request
against `develop`.

## Toolchain

This project uses [mise](https://mise.jdx.dev/) to pin the Go toolchain
version (`mise.toml` specifies Go 1.25). Install mise, then run commands
through it so you build and test with the pinned version:

```sh
mise exec -- go build ./...
mise exec -- go test ./...
```

Format code with `gofmt` before submitting:

```sh
gofmt -l .
```

`gofmt -l` should print no files. Run `gofmt -w .` to fix formatting in
place.

## Branch model

- `develop` is the default branch. Feature branches are cut from `develop`
  and target `develop` in their pull request.
- `main` only ever receives merges from `develop`. That merge is the only
  path to `main`.
- Every push to `main` triggers a release. Do not open pull requests
  directly against `main`.

## Documentation style

The README deliberately mixes two registers of language:

- **Controlled language** (STE100-style: short sentences, one instruction
  per sentence, restricted vocabulary) is used in the Terms, Requirements,
  Installation, and Configuration sections, in the error/log tables, and
  in Troubleshooting. These sections are read under time pressure while
  something is broken or being configured, so they favor precision and
  scanability over style.
- **Ordinary prose** is used everywhere else (introduction, design
  rationale, and similar explanatory sections).

This split is intentional. When editing the README, keep new or changed
text in a controlled-language section terse and single-instruction-per-
sentence; keep prose elsewhere readable and connected. Do not "smooth out"
the controlled sections into flowing prose, and do not tighten the prose
sections into clipped instructions.

## Adding configuration options

This plugin keeps a deliberately small configuration surface. Before
proposing a new configuration option, show a concrete need that the
existing options cannot express — not a hypothetical or a convenience.
Explain in the pull request what you are trying to do, why the current
options (see the README's Configuration reference) cannot do it, and why
a new option is the right fix rather than a change to existing behavior.

Pull requests that add configuration surface without that case will be
declined.

## Changelog

If your change affects secret-data handling or the replica's consistency
model, add an entry to `CHANGELOG.md` — see its header for the exact
rule.

## Tests

Run the full test suite before opening a pull request:

```sh
mise exec -- go vet ./...
mise exec -- go test ./...
```

All packages should report `ok`. CI runs the test suite with `-race`
(`go test -race ./...`); if you can reproduce a race locally, run it that
way too.
