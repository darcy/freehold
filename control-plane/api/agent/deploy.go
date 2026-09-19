// Package agent: Chunk 4 Phase A infrastructure.
package agent

import (
	"fmt"
	"time"

	"freehold/control-plane/state"
)

// RegisterAgent records the CPA in the control-plane agent registry (A5) so it
// shows up in the console/TUI the way any named agent does. The readiness dot
// (●/○) is NOT stored here: it is live relay presence (kind:20001), read fresh
// per view — the registry row carries the identity, presence carries liveness.
func RegisterAgent(store *state.StateStore, name, pubkey string) error {
	if store == nil {
		return fmt.Errorf("nil state store")
	}
	rec := state.AgentRecord{
		Pubkey:    pubkey,
		CreatedAt: uint64(time.Now().Unix()),
	}
	store.InsertAgent(name, rec)
	return nil
}
