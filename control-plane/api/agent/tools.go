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
// channel created with private set gets visibility=private. `model` is the
// litellm alias the agent's harness reasons on (one of CustomLiteLLMModels;
// empty = the General default) — the deploy path pins the core alias for the
// CPA + departments regardless. Supplied by the caller (the harness runtime
// wired to the runner); the tool invokes it and records the registry row.
type CreateAgentFn func(name, purpose string, channels []string, private bool, model string) (pubkey string, err error)

// ProvisionArgs is one provision_runner call: a NEW capability runner's spec
// plus the agents to grant it to. The tool is CREDENTIAL-BLIND by construction
// — it accepts no secret at all: ssh doors mint their own keypair (the public
// line comes back for a one-time install on the target box), api-class doors
// ship EMPTY (a "pending" placeholder) and the operator fills the credential
// through the console web UI. No credential can transit an agent's context.
type ProvisionArgs struct {
	// Name is the runner name, <target>-<protocol>-<identity> (e.g.
	// rtx3090-ssh-root, unifi-api-admin) — capability-named, never
	// consumer-named.
	Name string `json:"name"`
	// Kind is the connector kind: "ssh" (user@host[:port] address), or an
	// api-class kind ("unifi", ...) whose exec runs locally with the
	// credential + address injected as env.
	Kind string `json:"kind"`
	// Address is the target endpoint: user@host[:port] (ssh) or the base URL
	// (api-class).
	Address string `json:"address"`
	// GrantTo are the agent NAMES to grant onto the runner's roster (a
	// department that owns the capability class, or a custom agent that owns
	// the service). Each must exist in the agent registry.
	GrantTo []string `json:"grant_to"`
	// Hosted selects where the runner process lives: "" (default) stages it
	// on the CP guest; "self" enrolls a runner RESIDENT on the target —
	// installed there by the agent (the runner-client), identity minted
	// on-guest. The CP holds no private material either way.
	Hosted string `json:"hosted,omitempty"`
	// Self-hosted only: the target's dial address — the box's pinned name
	// (bare host, no port; the CP allocates the port). Pods dial
	// http://<host>:<port>.
	Host string `json:"host,omitempty"`
	// Self-hosted only: the runner's presented Nostr + X25519 pubkeys
	// (64-hex each, from `runner enroll` on the target).
	Pubkey    string `json:"pubkey,omitempty"`
	EncPubkey string `json:"enc_pubkey,omitempty"`
	// Probe is the door's verify arm — "<METHOD> <path> [auth] [want]" (e.g.
	// "GET /user/tokens/verify bearer"; auth one of bearer (default), basic,
	// json-body (the credential IS the POST body — unifi), none; want a
	// 3-digit status, default 200). Required for api-class kinds at first
	// provision: it is what turns the runner's self-check green, and the
	// requesting agent knows the API. Optional on re-provision ("" keeps the
	// door's existing probe). CP-validated; never a free-form shell string.
	Probe string `json:"probe,omitempty"`
	// ProbeBody is the probe's optional literal request body (JSON — e.g.
	// kubernetes' SelfSubjectReview) alongside the credential.
	ProbeBody string `json:"probe_body,omitempty"`
}

// ProvisionRunnerFn stages a NEW capability runner on the fly (the CPA's
// grant-giving flow): provision the runner package, sync its relay channel,
// start it on the CP guest, record the dynamic capability (rebuild-safe), grant
// the named agents onto its roster live, and re-apply the grantees' pods so
// their exec surface picks the new coords up. Built by cpbuild.BuildProvisionRunner;
// nil = unsupported.
type ProvisionRunnerFn func(args ProvisionArgs) (report string, err error)

// RetireArgs is one revoke_runner call. The tool takes no secret (retirement
// only removes; there is nothing to seal).
type RetireArgs struct {
	// Name is the runner to take capability away from — always capability-named
	// <target>-<protocol>-<identity>, never the consumer's name.
	Name string `json:"name"`
	// RevokeFrom narrows the call to a from-the-roster removal: the named agent
	// NAMES lose their grant on this door while the door keeps serving whoever
	// remains on its roster. Empty = retire the WHOLE door (its unit is stopped,
	// its credential erased, its record dropped under a re-enablable guard note).
	// A named agent that is not on the roster is a reported no-op, never a
	// fall-through to the whole-door path.
	RevokeFrom []string `json:"revoke_from,omitempty"`
}

// RevokeRunnerFn is the CPA's take-away-half twin of ProvisionRunnerFn: the
// whole-door retirement flow — clear the roster (the live revocation), cut the
// coords feed, and retire the record. CP-hosted doors get their unit stopped
// here; a resident door's box-side close-out is the OPERATOR's (on the box the
// build does not drive — Compute for a substrate credential), with the steps in
// the report: the agent that just lost the door is never handed a teardown job
// for it. Built by cpbuild.BuildRevokeRunner; nil = unsupported.
type RevokeRunnerFn func(args RetireArgs) (report string, err error)

// UpdateArgs is one update_agent call. Name is the agent to touch; every other
// field is optional and absent-means-keep-current, resolved against the agent's
// registry row. Rename moves the agent to a new name (identity preserved).
type UpdateArgs struct {
	// Name is the agent's CURRENT registry name.
	Name string `json:"name"`
	// Rename is the agent's NEW name; empty = no rename. The durable identity
	// dir, workspace, pod, and registry row all move; the pubkey (and so chat
	// history, grants, and memory) stays.
	Rename string `json:"rename"`
	// Purpose replaces the one-liner the agent's system prompt is rendered
	// from (custom agents; core prompts are repo-embedded). Empty = keep.
	Purpose string `json:"purpose"`
	// Channel/Channels replace the channel list (the create_agent shapes).
	// Empty/absent = keep the row's current list.
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	// Private applies only when a channel list is given.
	Private bool `json:"private"`
	// Model switches the litellm alias (one of CustomLiteLLMModels). Empty = keep.
	Model string `json:"model"`
}

// UpdateAgentFn applies an update_agent call and returns a leg-by-leg report.
// Built by cpbuild.BuildUpdateAgentFn; nil = unsupported.
type UpdateAgentFn func(args UpdateArgs) (report string, err error)

// RemoveAgentFn retires an agent's pod + derived k8s objects — the infra half
// of manage_agent remove (the registry row drop stays Console-side). The
// durable workspace dir is deliberately kept: it is data, not an object. Built
// by cpbuild.BuildRemoveAgentFn; nil = remove drops the row only.
type RemoveAgentFn func(name string) (report string, err error)

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
	// Migrate runs the CP's pending one-time migration scripts (the versioned
	// config/prompt/repair path). nil = migrations unsupported.
	Migrate Migrator
	// World drives the CP's world-build/reconcile stages through its co-located
	// runner (the "box = login + trigger" entry point). nil = unsupported.
	World WorldApply
	// WorldTeardownFn runs the CP-owned world teardown (the physical inverse of
	// build; the CP + runner survive). nil = unsupported.
	WorldTeardownFn func() (string, error)
	// Provision stages a new capability runner on the fly (provision_runner —
	// the CPA's grant-giving flow). nil = unsupported.
	Provision ProvisionRunnerFn
	// Revoke retires a capability door on the fly (revoke_runner — the CPA's
	// take-away flow; the counterpart of Provision above). nil = unsupported.
	Revoke RevokeRunnerFn
	// Update applies update_agent (purpose/model/channels/rename). nil =
	// unsupported.
	Update UpdateAgentFn
	// Remove retires an agent's pod + objects before manage_agent drops the
	// row. nil = remove drops the row only (the pod lingers — the pre-retire
	// shape).
	Remove RemoveAgentFn
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

// Migrator applies the CP's pending migration scripts and returns their results.
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

// WorldMigrate runs the CP's pending migration scripts (idempotent; a failure
// stops the queue and stays pending for the next run).
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
func (t *Tools) CreateAgent(name, purpose string, channels []string, private bool, model string) (string, error) {
	if t.Console == nil {
		return "", fmt.Errorf("create-agent: no console client bound")
	}
	if t.Create == nil {
		return "", fmt.Errorf("create-agent: no deploy path bound")
	}
	pubkey, err := t.Create(name, purpose, channels, private, model)
	if err != nil {
		return "", fmt.Errorf("create-agent deploy %s: %w", name, err)
	}
	// RegisterAgent records only the primary channel (the ConsoleOps contract);
	// the full channel list + private flag are persisted separately as
	// Registry.SetChannels by the create_agent dispatch, so reconcile rejoins
	// every channel. The agent's own presence channel is its name (kind-9).
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

// ProvisionRunner stages a new capability runner on the fly and grants the
// named agents onto it (the CPA-scoped grant-giving flow; raw grants onto
// EXISTING runners stay operator-scoped via grant_agent).
func (t *Tools) ProvisionRunner(args ProvisionArgs) (string, error) {
	if t.Provision == nil {
		return "", fmt.Errorf("provision-runner: no staging path bound")
	}
	return t.Provision(args)
}

// RevokeRunner retires a capability door: roster cleared, coords cut, record
// marked retired (the CPA's take-away flow; raw grant/revoke onto EXISTING
// runners stay operator-scoped via grant_agent).
func (t *Tools) RevokeRunner(args RetireArgs) (string, error) {
	if t.Revoke == nil {
		return "", fmt.Errorf("revoke_runner: no retirement path bound")
	}
	return t.Revoke(args)
}

// UpdateAgent applies an update_agent call (purpose/model/channels/rename).
func (t *Tools) UpdateAgent(args UpdateArgs) (string, error) {
	if t.Update == nil {
		return "", fmt.Errorf("update_agent: no update path bound")
	}
	return t.Update(args)
}

// ManageAgent lists registered agents, or (with remove) retires one's pod and
// drops its registry row. Returns the current agents.
func (t *Tools) ManageAgent(remove string) ([]console.AgentInfo, error) {
	if t.Console == nil {
		return nil, fmt.Errorf("manage-agent: no console client bound")
	}
	if remove != "" {
		if t.Remove != nil {
			if _, err := t.Remove(remove); err != nil {
				return nil, fmt.Errorf("manage-agent remove %s: %w", remove, err)
			}
		}
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

// WorldTeardown runs the CP-owned world teardown — the physical inverse of
// build (relay/k3s + the CP-side agent-tools process; internal DNS cleared by
// the console). The CP + its co-located runner, and the durable agent registry,
// SURVIVE. Injected by the caller (the console's cpbuild apply).
func (t *Tools) WorldTeardown() (string, error) {
	if t.WorldTeardownFn == nil {
		return "", fmt.Errorf("world-teardown: no teardown driver bound")
	}
	return t.WorldTeardownFn()
}
