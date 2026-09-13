#!/bin/sh
# 001 verify — postcondition: the registry loads (an unparsable store fails).
set -euo pipefail
exec "$FREEHOLD_AGENT_TOOLS" registry verify --registry "$REGISTRY"
