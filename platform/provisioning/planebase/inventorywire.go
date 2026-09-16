package planebase

import (
	"encoding/json"
	"strings"
)

// The storage inventory crosses a process boundary: a `storage resolve`
// subprocess (which holds the runner connection) detects it, and the rebuild
// engine (which drives the prompts) consumes it. The wire is one JSON line so
// the encoding stays dumb and the structs stay in one place.

// InventoryLinePrefix marks the single JSON inventory line.
const InventoryLinePrefix = "STORAGE-INVENTORY: "

// EncodeInventory renders an inventory as the contract line.
func EncodeInventory(inv Inventory) string {
	b, err := json.Marshal(inv)
	if err != nil {
		return ""
	}
	return InventoryLinePrefix + string(b)
}

// DecodeInventory scans command output for the inventory line. The bool is
// false when the line is absent (an older/blank resolve), so the caller can
// fall back rather than treat an empty host as a successful read.
func DecodeInventory(out string) (Inventory, bool) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, InventoryLinePrefix)
		if !ok {
			continue
		}
		var inv Inventory
		if err := json.Unmarshal([]byte(rest), &inv); err != nil {
			return Inventory{}, false
		}
		return inv, true
	}
	return Inventory{}, false
}
