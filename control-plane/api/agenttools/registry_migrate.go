package agenttools

import (
	"fmt"

	"freehold/control-plane/api/cpstate"
)

// CheckRegistry verifies the registry at path is a valid, loadable store — the
// check the 001 migration script runs (malformed JSON or an unreadable file
// errors).
func CheckRegistry(path string) error {
	if _, err := OpenRegistry(path); err != nil {
		return fmt.Errorf("registry not loadable: %w", err)
	}
	return nil
}

// ImportConsoleAgents folds the console's own state.json agents map into the
// authoritative registry ADDITIVELY (the 002 migration): a name already in the
// registry keeps its current row — the registry is authoritative and a stale
// console pubkey must never clobber it. Idempotent. Returns an error unless the
// check holds: every console agent with a pubkey is present in the registry.
func ImportConsoleAgents(reg *Registry, consoleStateDir string) error {
	st, err := cpstate.Read(consoleStateDir)
	if err != nil {
		return err
	}
	rows, err := reg.Agents()
	if err != nil {
		return err
	}
	byName := map[string]bool{}
	for _, r := range rows {
		byName[r.Name] = true
	}
	for _, a := range st.Agents {
		if a.Pubkey == "" || byName[a.Name] {
			continue
		}
		ch := a.Name
		if a.Channel != nil && *a.Channel != "" {
			ch = *a.Channel
		}
		if _, err := reg.RegisterAgent(a.Name, a.Pubkey, ch); err != nil {
			return err
		}
	}
	// Postcondition: every console agent NAME with a pubkey is present. Checked
	// BY NAME, not by pubkey — the additive guarantee is that an existing
	// registry row KEEPS its current pubkey, which is deliberately different
	// from a stale console value, so a by-pubkey gate would never converge.
	rows, err = reg.Agents()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Name] = true
	}
	for _, a := range st.Agents {
		if a.Pubkey != "" && !have[a.Name] {
			return fmt.Errorf("console agent %q (%s) missing from the registry", a.Name, a.Pubkey)
		}
	}
	return nil
}
