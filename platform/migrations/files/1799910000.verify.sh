#!/bin/sh
# 002 verify — postcondition: import-console already validates that every
# console agent with a pubkey is present; a re-run is idempotent + convergent.
set -euo pipefail
exec "$FREEHOLD_AGENT_TOOLS" registry import-console \
  --registry "$REGISTRY" --console-state "$CONSOLE_STATE"
