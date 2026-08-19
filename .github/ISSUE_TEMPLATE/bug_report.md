---
name: Bug report
about: Report a problem with the plugin
title: ""
labels: bug
---

## Plugin version

<!-- Output of `bao secrets list -detailed` or the release tag/commit you built
from. If the failure happens during `bao plugin register` or `bao secrets
enable` (before the mount exists to query), report the release tag or
commit you built from instead. -->

## OpenBao version

<!-- Output of `bao version`. -->

## Mount configuration

<!-- The relevant `op/config` payload for the mount. Redact secrets: service
account tokens, item contents, and vault IDs. Field names and structure are
fine to include as-is. -->

## op/status output

<!-- Output of reading `op/status` on the mount: replica staleness, cold
start / refresh state, and rate-governor state. This is the primary
diagnostic surface for replica and governor issues (see the README's
Troubleshooting section). Redact vault names here if they are sensitive;
`op/status` does not include secret material but does report per-vault
state keyed by vault name. -->

## Expected behavior

<!-- What you expected to happen. -->

## Actual behavior

<!-- What actually happened. -->

## Relevant log lines

<!-- OpenBao server log lines around the failure. Redact secrets. -->

## Additional context

<!-- Anything else that might help: steps to reproduce, whether this is new
or a regression, related configuration. -->
