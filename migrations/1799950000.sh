echo "Archive the retired department channels (retry: 1799940000 ran hollow)"

# 1799940000.sh marked itself done on worlds where it archived NOTHING: its
# find-by-name queried kind-39000 with a single 1000-event page, and a busy
# relay re-emits discovery events on every build, so quiet channels fell
# outside the page window and every channel edit skipped "not found" with
# exit 0. The client now pages through the full discovery history, so this
# re-runs the same idempotent archive at a fresh epoch — the marker on the
# hollow run must not be the end of it.
#
# Each channel is archived by its OWNER (the department identity) via the
# native kind-9002 archived tag: members keep the read-only history, discovery
# drops it, and every further mutation is refused except an unarchive
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
