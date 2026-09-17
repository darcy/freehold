package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"freehold/contract/console"
	"freehold/platform/migrations"
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
// returns the new agent's pubkey. `channels` are the channel NAMES to add it to
// (empty = the default freehold channel); the CPA is added to each. An explicit
// channel created with private set gets visibility=private. Supplied by the
// caller (the harness runtime wired to the runner); the tool invokes it and
// records the registry row.
type CreateAgentFn func(name, purpose string, channels []string, private bool) (pubkey string, err error)

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
	// World drives the CP's world-build/reconcile stages through its co-located
	// runner (the "box = login + trigger" entry point). nil = unsupported.
	World WorldApply
	// Status builds the single inventory world_status returns (agents + the
	// console's runners/DNS read underneath). nil = agents only.
	Status WorldStatusFunc
	// Exec runs a command through the CP's co-located runner — the "drive
	// through the CP" exec surface a thin login box uses instead of a local
	// provisioning runner. nil = exec unsupported.
	Exec ExecFn
	// DoorAuthorize/Revoke authorize/revoke an operator box's door key on the
	// host (DOOR_SPEC). nil = door unsupported.
	DoorAuthorize DoorAuthorizeAppend
	DoorRevoke    DoorRevoke
}

// ExecFn runs a command through the CP's co-located runner and returns its
// stdout. target is the runner target the caller asked for — the CP validates
// it against its own bound runner target (error on mismatch) so a thin box
// never silently execs on a host it didn't name. secrets request extra
// runner-injected secret env by name (redacted).
type ExecFn func(target, cmd string, timeoutS uint64, secrets ...string) (string, error)

// WorldExec runs a command through the CP's co-located runner (drive-through-
// CP exec, so a thin box has the build box's full operational surface without
// hosting a runner). Operator-scoped (dispatch gates it).
func (t *Tools) WorldExec(target, cmd string, timeoutS uint64, secrets ...string) (string, error) {
	if t.Exec == nil {
		return "", fmt.Errorf("world-exec: no CP exec driver bound")
	}
	return t.Exec(target, cmd, timeoutS, secrets...)
}

// Migrator applies pending CP migrations and returns their verify-gated results.
type Migrator func() ([]migrations.Result, error)

// DoorAuthorizeAppend authorizes a box's public door key onto the host door
// through the CP's co-located runner. nil = door unsupported.
type DoorAuthorizeAppend func(pubkey string) error

// DoorRevoke removes a box's public door key from the host door.
type DoorRevoke func(pubkey string) error

// AuthorizeDoor appends an operator box's SSH public key to the host door
// (DOOR_SPEC): the CP runs the append through its co-located runner, scoped to
// the caller-presented pubkey. Operator-only (dispatch gates it).
func (t *Tools) AuthorizeDoor(pubkey string) error {
	if t.DoorAuthorize == nil {
		return fmt.Errorf("world-authorize-door: no door driver bound")
	}
	return t.DoorAuthorize(pubkey)
}

// RevokeDoor removes an operator box's SSH public key from the host door.
func (t *Tools) RevokeDoor(pubkey string) error {
	if t.DoorRevoke == nil {
		return fmt.Errorf("world-revoke-door: no door driver bound")
	}
	return t.DoorRevoke(pubkey)
}

// WorldMigrate runs pending CP migrations (idempotent, 🟢/🔴 verify-gated).
func (t *Tools) WorldMigrate() ([]migrations.Result, error) {
	if t.Migrate == nil {
		return nil, fmt.Errorf("world-migrate: no migrations runner bound")
	}
	return t.Migrate()
}

// WorldApply is a CP-driven world reconcile step: it runs one of the shared
// stage commands (internal/stages) through the CP's co-located runner, so the
// box can "login + trigger" the CP to (re)assert part of the world. Returns a
// human report.
type WorldApply func() (string, error)

// WorldBuild applies the CP owned bring-up/reconcile stages through the
// co-located runner (the "box = login + trigger" entry point). nil Apply =
// unsupported.
func (t *Tools) WorldBuild() (string, error) {
	if t.World == nil {
		return "", fmt.Errorf("world-build: no CP build driver bound")
	}
	return t.World()
}

// CreateAgent stands up a new named agent: deploys its sprig pod via Create,
// then registers the registry row with the minted pubkey. Returns the pubkey.
func (t *Tools) CreateAgent(name, purpose string, channels []string, private bool) (string, error) {
	if t.Console == nil {
		return "", fmt.Errorf("create-agent: no console client bound")
	}
	if t.Create == nil {
		return "", fmt.Errorf("create-agent: no deploy path bound")
	}
	pubkey, err := t.Create(name, purpose, channels, private)
	if err != nil {
		return "", fmt.Errorf("create-agent deploy %s: %w", name, err)
	}
	// The registry row carries only the primary channel. Reconcile re-derives a
	// reserved department's full channel list (agents.DepartmentChannels) but
	// rejoins only this one for any other agent, so a multi-channel CUSTOM agent
	// would lose its extra channels across a rebuild — departments are the only
	// multi-channel caller today; persisting a custom agent's full list is a
	// named follow-up. The agent's own presence channel is its name (kind-9).
	if _, err := t.Console.RegisterAgent(name, pubkey, primaryChannel(channels)); err != nil {
		return "", fmt.Errorf("create-agent register: %w", err)
	}
	return pubkey, nil
}

// primaryChannel returns the first non-empty channel name, or "" (the default
// freehold channel) when the list is empty.
func primaryChannel(channels []string) string {
	for _, c := range channels {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return ""
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

// WorldStatusFunc builds the single-inventory world_status payload (agents +
// the console's runners/DNS read underneath) — one read the TUI and the world
// CLI consume.
type WorldStatusFunc func() (map[string]interface{}, error)

// WorldStatus is the CP's world-action surface for the box's "login + trigger":
// the single inventory read — what the CP manages (its agent registry) plus the
// console's runners/DNS (the "console underneath"). The infra half (k3s /
// litellm / Caddy / relay provisioning) is the build/upgrade follow-up that
// needs a live world; status + teardown are the roster-gated first slice.
func (t *Tools) WorldStatus() (map[string]interface{}, error) {
	if t.Status != nil {
		return t.Status()
	}
	if t.Console == nil {
		return nil, fmt.Errorf("world-status: no console client bound")
	}
	agents, err := t.Console.Agents()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"agents": agents}, nil
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
