#!/bin/sh
# 002 — fold the console's own state.json agents into the authoritative
# registry.json ADDITIVELY: a name already in the registry keeps its current row
# (the registry is authoritative; a stale console pubkey must never clobber it).
set -euo pipefail
exec "$FREEHOLD_AGENT_TOOLS" registry import-console \
  --registry "$REGISTRY" --console-state "$CONSOLE_STATE"
