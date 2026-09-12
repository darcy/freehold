#!/bin/sh
# 001 — the agent-tools durable registry must be a valid, loadable store.
# The verify gate re-runs the same check: a registry that loads is converged.
set -euo pipefail
exec "$FREEHOLD_AGENT_TOOLS" registry verify --registry "$REGISTRY"
