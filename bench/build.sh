#!/usr/bin/env bash
# Task 8 bench gate: cross-compile the plugin for the bench container
# (linux/arm64 — matches the arm64 Mac host's native docker platform;
# swap GOARCH=amd64 on an amd64 host).
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bench/dist
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -o bench/dist/openbao-plugin-secrets-onepassword \
  ./cmd/openbao-plugin-secrets-onepassword

echo "built: bench/dist/openbao-plugin-secrets-onepassword"
shasum -a 256 bench/dist/openbao-plugin-secrets-onepassword
