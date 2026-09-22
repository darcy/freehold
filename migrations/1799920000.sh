echo "Make #freehold private and prefix department channels with freehold-"

# #freehold becomes private: its owner (the CPA) flips visibility in place, so
# the operator, the CPA, the departments, and every custom agent keep their
# membership. Idempotent — a second run is a no-op on the relay.
"$FREEHOLD_AGENT_TOOLS" channel edit \
  --state-dir "$STATE_DIR" \
  --relay-url "$FREEHOLD_RELAY_URL" \
  --relay-auth-url "$FREEHOLD_RELAY_AUTH_URL" \
  --as "${FREEHOLD_CPA_NAME:-freehold}" \
  --channel '#freehold' \
  --visibility private

# Department channels gain the `freehold-` prefix (#ai -> #freehold-ai, …).
# Each is renamed in place by its OWNER (the department identity), preserving
# membership and history; a missing identity or channel is skipped (no-op).
for dept in ai compute network data; do
  "$FREEHOLD_AGENT_TOOLS" channel edit \
    --state-dir "$STATE_DIR" \
    --relay-url "$FREEHOLD_RELAY_URL" \
    --relay-auth-url "$FREEHOLD_RELAY_AUTH_URL" \
    --as "$dept" \
    --channel "#$dept" \
    --rename "#freehold-$dept"
done
