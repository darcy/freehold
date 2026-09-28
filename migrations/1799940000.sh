echo "Archive the retired department channels"

# Departments now hold their conversations in #freehold (or wherever a
# conversation already includes them); the per-department channels are retired.
# Each is archived by its OWNER (the department identity) via the native
# kind-9002 archived tag: members keep the read-only history, discovery drops
# it, and every further mutation is refused except an unarchive
# (--archived false). Idempotent — an already-archived channel is reported and
# skipped; a missing identity or channel is skipped (no-op).
for dept in ai compute network data; do
  "$FREEHOLD_AGENT_TOOLS" channel edit \
    --state-dir "$STATE_DIR" \
    --relay-url "$FREEHOLD_RELAY_URL" \
    --relay-auth-url "$FREEHOLD_RELAY_AUTH_URL" \
    --as "$dept" \
    --channel "#freehold-$dept" \
    --archived true
done
