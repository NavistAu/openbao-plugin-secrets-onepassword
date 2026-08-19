#!/usr/bin/env bash
# Task 8 bench gate: stand up a scratch OpenBao 2.5.5 server-mode
# container, init/unseal it, register the op plugin (hash-verified),
# and mount it at op/. Does NOT write op/config — that step needs
# OP_SERVICE_ACCOUNT_TOKEN piped via stdin (never argv), run it
# separately, e.g.:
#
#   printf '%s' "$OP_SERVICE_ACCOUNT_TOKEN" | docker exec -i \
#     -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN="$(cat bench/scratch/root-token.txt)" \
#     bao-bench bao write op/config service_account_token=- vaults=Infra \
#       refresh_interval=15m daily_request_limit=1000 hourly_read_limit=1000 \
#       passthrough=true passthrough_ceiling_pct=25 passthrough_ttl=1m \
#       serve_stale=true negative_cache_ttl=30s
#
# Scratch state (root token, unseal key, init output) is never
# committed — it lives under bench/scratch/, which is gitignored.
set -euo pipefail
cd "$(dirname "$0")/.."

SCRATCH_DIR="${BENCH_SCRATCH_DIR:-bench/scratch}"
mkdir -p "$SCRATCH_DIR"

docker rm -f bao-bench >/dev/null 2>&1 || true

docker run -d --name bao-bench \
  --cap-add=IPC_LOCK \
  -p 18200:8200 \
  -v "$(pwd)/bench/config/bao.hcl:/bao/config/bao.hcl:ro" \
  -v "$(pwd)/bench/dist:/bao/plugins:ro" \
  --entrypoint bao \
  ghcr.io/openbao/openbao:2.5.5 \
  server -config=/bao/config/bao.hcl

sleep 3

docker exec -e BAO_ADDR=http://127.0.0.1:8200 bao-bench \
  bao operator init -key-shares=1 -key-threshold=1 -format=json \
  > "$SCRATCH_DIR/init-output.json"

UNSEAL_KEY=$(python3 -c "import json; print(json.load(open('$SCRATCH_DIR/init-output.json'))['unseal_keys_b64'][0])")
ROOT_TOKEN=$(python3 -c "import json; print(json.load(open('$SCRATCH_DIR/init-output.json'))['root_token'])")
echo "$ROOT_TOKEN" > "$SCRATCH_DIR/root-token.txt"
chmod 600 "$SCRATCH_DIR/root-token.txt"

docker exec -e BAO_ADDR=http://127.0.0.1:8200 bao-bench bao operator unseal "$UNSEAL_KEY"

SHA256=$(shasum -a 256 bench/dist/openbao-plugin-secrets-onepassword | awk '{print $1}')
echo "plugin sha256: $SHA256"

# Catalog name is "op" (spec D10/§3); the binary filename
# (openbao-plugin-secrets-onepassword) is passed via -command since
# it differs from the catalog name.
docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN="$ROOT_TOKEN" bao-bench \
  bao plugin register -sha256="$SHA256" -command=openbao-plugin-secrets-onepassword secret op

docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN="$ROOT_TOKEN" bao-bench \
  bao secrets enable -path=op op

echo "bao-bench ready: mounted at op/, unsealed, plugin registered."
echo "root token: $SCRATCH_DIR/root-token.txt"
echo "next: write op/config (see script header for the stdin-token form)."
