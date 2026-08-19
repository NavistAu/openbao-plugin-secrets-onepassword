#!/usr/bin/env bash
# Bench gate cleanup (Task 8). Removes the disposable 1Password item and
# the scratch bao-bench container. Safe to run multiple times.
set -uo pipefail

DISPOSABLE_ITEM_ID="${DISPOSABLE_ITEM_ID:-imzwip74amad2b6zpdnxorvnk4}"

echo "== deleting disposable 1P item ${DISPOSABLE_ITEM_ID} (vault Infra) =="
op item delete "${DISPOSABLE_ITEM_ID}" --vault Infra 2>&1
op item get "${DISPOSABLE_ITEM_ID}" --vault Infra >/dev/null 2>&1
if [ $? -ne 0 ]; then
  echo "confirmed: disposable item no longer readable (deleted)"
else
  echo "WARNING: disposable item still readable after delete attempt" >&2
fi

echo "== removing bao-bench container =="
docker rm -f bao-bench >/dev/null 2>&1 || true

echo "cleanup done"
