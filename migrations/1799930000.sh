echo "Fold any console-only agent into the authoritative registry"

# The registry reconcile that used to ride 1799910000.sh. That script's marker was
# written on worlds that ran it, but an agent added to the console's state.json
# afterwards still needs folding in, so this re-runs the same additive import at a
# fresh epoch. ImportConsoleAgents is additive (an existing row keeps its pubkey)
# and postchecks by name, so this is safe on any world, including a fresh install.
"$FREEHOLD_AGENT_TOOLS" registry import-console \
  --registry "$REGISTRY" --console-state "$CONSOLE_STATE"
