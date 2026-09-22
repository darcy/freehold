package agenttools

import (
	"path/filepath"

	"freehold/contract/version"
	"freehold/control-plane/api/cpstate"
)

// WorldStatus assembles the CP's single-inventory status (agents + runners +
// dns + facts) — the ONE implementation backed by both the console's /api/world
// route and the /mcp world_status tool, so the two surfaces can never diverge.
// agents come from the authoritative durable registry (registry.json), runners +
// dns from the console's state.json, facts from facts.json. The caller adds its
// own cp_pubkey (the console serves its pubkey; agent-tools serves its audience).
func WorldStatus(reg *Registry, facts *FactsStore, consoleStateDir string) (map[string]interface{}, error) {
	agents, err := reg.Agents()
	if err != nil {
		return nil, err
	}
	cs, err := cpstate.Read(consoleStateDir)
	if err != nil {
		return nil, err
	}
	runners := []map[string]interface{}{}
	for _, r := range cs.Runners {
		runners = append(runners, map[string]interface{}{
			"name": r.Name, "nostr_pubkey": r.NostrPubkey, "mcp_addr": r.McpAddr,
		})
	}
	dns := []map[string]interface{}{}
	for _, d := range cs.DNS {
		dns = append(dns, map[string]interface{}{"name": d.Name, "ip": d.IP})
	}
	pin, _ := version.Read(filepath.Join(consoleStateDir, version.FileName))
	out := map[string]interface{}{
		"agents":  agents,
		"runners": runners,
		"dns":     dns,
		"version": pin,
	}
	if facts != nil {
		out["facts"] = facts.Facts()
	}
	return out, nil
}
