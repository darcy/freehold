echo "Fold console state agents into the authoritative registry"

"$FREEHOLD_AGENT_TOOLS" registry import-console \
  --registry "$REGISTRY" --console-state "$CONSOLE_STATE"
