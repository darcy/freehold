package agent

import (
	"encoding/json"
	"fmt"

	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/migrations"
)

// ConsoleOps is what a tool handler needs from the registry backing it. The
// runner-facing console.Client satisfies it; freehold-agent-tools binds it to
// the CP's local durable agent registry (direct, in-process — no admin login,
// no HTTP hop, no cross-process state.json writes). Signatures mirror the
// console client so either implementation satisfies it.
type ConsoleOps interface {
	RegisterAgent(name, pubkey, channel string) (json.RawMessage, error)
	UnregisterAgent(name string) (json.RawMessage, error)
	Agents() ([]console.AgentInfo, error)
	Grant(runner, pubkey string) (json.RawMessage, error)
}

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

// Tools wraps the agent-management operations: the registry backend (a
// ConsoleOps) plus the create deploy path (deploy on the runner).
type Tools struct {
	Console ConsoleOps
	// Create deploys a new agent pod (mint + apply). nil = create unsupported.
	Create CreateAgentFn
	// Migrate runs the CP's pending verify-gated migrations (the versioned
	// config/prompt/repair path). nil = migrations unsupported.
	Migrate Migrator
}

// Migrator applies pending CP migrations and returns their verify-gated results.
type Migrator func() ([]migrations.Result, error)

// WorldMigrate runs pending CP migrations (idempotent, 🟢/🔴 verify-gated).
func (t *Tools) WorldMigrate() ([]migrations.Result, error) {
	if t.Migrate == nil {
		return nil, fmt.Errorf("world-migrate: no migrations runner bound")
	}
	return t.Migrate()
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

// WorldStatus is the CP's world-action surface for the box's "login + trigger":
// what the CP currently manages (its agent registry). The infra half (k3s /
// litellm / Caddy / relay provisioning) is the build/upgrade follow-up that
// needs a live world; status + teardown are the roster-gated first slice.
func (t *Tools) WorldStatus() ([]console.AgentInfo, error) {
	if t.Console == nil {
		return nil, fmt.Errorf("world-status: no console client bound")
	}
	return t.Console.Agents()
}

// WorldTeardown clears the CP's managed agent registry (the world-action pair to
// the console's /api/teardown, roster-gated here). Returns how many were removed.
func (t *Tools) WorldTeardown() (int, error) {
	if t.Console == nil {
		return 0, fmt.Errorf("world-teardown: no console client bound")
	}
	agents, err := t.Console.Agents()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		if _, err := t.Console.UnregisterAgent(a.Name); err != nil {
			return n, fmt.Errorf("world-teardown remove %s: %w", a.Name, err)
		}
		n++
	}
	return n, nil
}
