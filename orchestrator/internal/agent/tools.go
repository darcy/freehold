package agent

import (
	"fmt"

	"freehold/orchestrator/internal/console"
)

// CreateAgentFn deploys a named agent's sprig pod + mints its identity and
// returns the new agent's pubkey. Supplied by the caller (the harness runtime
// wired to the runner); the tool invokes it and records the registry row.
type CreateAgentFn func(name, purpose string) (pubkey string, err error)

// Tools is the CPA's dedicated agent-management toolset (A4): create-agent,
// grant-agent, manage-agent. These are what the CPA's reasoning calls (via its
// MCP layer → console client) — the "dedicated create/grant/manage-agent
// toolset in place of a service-specific one" the roadmap A4 calls for. No
// skill-execution tools yet (Chunk 5/6 concerns).
//
// The toolset is deliberately thin: create deploys a NEW buzz-acp identity +
// pod, grant binds an agent to a runner's whitelist, manage lists/drops agent
// registry rows. All side effects flow through the console — the same audited
// surface the operators and delegate-peers already use.

// Tools wraps the console client with the create/grant/manage operations.
type Tools struct {
	Console *console.Client
	// Create deploys a new agent pod (mint + apply). nil = create unsupported.
	Create CreateAgentFn
}

// CreateAgent stands up a new named agent: deploys its sprig pod via Create,
// then registers the registry row with the minted pubkey. Returns the pubkey.
func (t *Tools) CreateAgent(name, purpose string) (string, error) {
	if t.Console == nil {
		return "", fmt.Errorf("create-agent: no console client bound")
	}
	if t.Create == nil {
		return "", fmt.Errorf("create-agent: no deploy path bound")
	}
	pubkey, err := t.Create(name, purpose)
	if err != nil {
		return "", fmt.Errorf("create-agent deploy %s: %w", name, err)
	}
	// The presence channel is the agent's own name (kind-9 mention channel).
	if _, err := t.Console.RegisterAgent(name, pubkey, name); err != nil {
		return "", fmt.Errorf("create-agent register: %w", err)
	}
	return pubkey, nil
}

// GrantAgent binds agent pubkeys to a runner's whitelist (agent ↔ runner grant,
// the locked coarse-grant model).
func (t *Tools) GrantAgent(runner string, agentPubkeys []string) error {
	if t.Console == nil {
		return fmt.Errorf("grant-agent: no console client bound")
	}
	for _, pk := range agentPubkeys {
		if _, err := t.Console.Grant(runner, pk); err != nil {
			return fmt.Errorf("grant-agent %s → %s: %w", pk, runner, err)
		}
	}
	return nil
}

// ManageAgent lists registered agents, or (with remove) drops one's registry
// row. Returns the current agents.
func (t *Tools) ManageAgent(remove string) ([]console.AgentInfo, error) {
	if t.Console == nil {
		return nil, fmt.Errorf("manage-agent: no console client bound")
	}
	if remove != "" {
		if _, err := t.Console.UnregisterAgent(remove); err != nil {
			return nil, fmt.Errorf("manage-agent remove %s: %w", remove, err)
		}
	}
	return t.Console.Agents()
}
