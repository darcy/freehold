# Grants on the fly — agent-initiated capability (0.7.4 build plan)

Status: current build plan. 0.7.3 shipped the department capability runners
(`stageDepartmentRunners`) with operator/console-issued grants; this chunk
unlocks the freehold agent (the CPA) to provision capability on the fly — the
operator asks in conversation, the department interviews, the CPA stands the
runner up and grants the requester onto it.

The scenario: "I have an RTX 3090 machine on the network and I want the AI
agent to manage it for me" (an ssh door). "I have a UniFi network and I want
the Network agent to manage it" (an api door; the agent works out with the
operator whether there's an API and what account it needs).

## The model

- **The unit of grant is still the runner; new capability = NEW runner.**
  `provision_runner` stages a new capability runner (keypair + sealed
  credential + private relay channel = the audit stream + a systemd unit on
  the CP guest) and grants the named agents onto its roster live. It never
  widens an existing runner; grants onto the build-time capability runners
  (`pve-ssh-root`, the kube doors, …) stay operator-scoped (`grant_agent` /
  the console).
- **Dynamic capability records.** An agent-provisioned runner is recorded in
  the CP state (`Capabilities`): kind, address, fixed port, rosters. The
  record makes the door rebuild-safe — `stageDepartmentRunners` re-stages it
  every build, adopting the existing package (the credential came from the
  operator; there is no build-time source to re-seal from, and a record whose
  package vanished fails loudly instead of silently re-keying).
- **The confirmation discipline.** `agent_grants` on the CP state: `confirm`
  (default) — the CPA grants when the operator's ask is in a thread with it;
  otherwise it DMs the operator and waits for a yes; `auto` — grants land
  without confirmation; `off` — the server denies the flow outright (the kill
  switch; `freehold-console grants-mode`). The server cannot see Buzz threads,
  so the in-thread/DM discipline is the granting skill's to enforce — the same
  trust class as `create_agent`.
- **Credentials:** ssh doors mint their OWN keypair — the operator installs
  the returned public key on the target once (the operator's password or
  private key never enters any context); api-class doors (unifi) seal the
  operator-supplied credential on arrival. Plaintext never persists.
- **Pod pickup.** After granting, the grantees' pods are re-applied with the
  new `FREEHOLD_RUNNER_*` coords resolved from state (static + per-zone DNS +
  dynamic) — the exec surface carries the new target without a full build.

## Deliverables

- `provision_runner` on the CP toolset (agent-tools server + the stdio
  bridge's advertised tools), bound to the staging flow
  (`cpbuild.BuildProvisionRunner`); a `console` `/api/provision` parity path
  (rosters as agent NAMES, resolved through the agent registry, + a port) so
  operator-provisioned runners are rebuild-safe too.
- The `unifi` kind: an api-class door whose exec runs locally with the
  credential + base URL injected as env; the runner's self-check probe posts
  the JSON login body to the controller's `/api/auth/login`.
- The prompts: the granting skill carries the grant-giving flow (confirm
  discipline, interview checklist, only-own-runners); the AI and Network
  department prompts carry the request-a-capability protocol.
- `freehold-console grants-mode` to read/flip the knob.

## Acceptance

*   [x] An agent-provisioned capability runner is recorded, survives a store
    reopen, and re-stages from its record (adopt-only) on every build.

*   [x] A granted caller execs through the new door (the real runner binary);
    an ungranted caller fails closed.

*   [x] The CPA can call `provision_runner` (confirm mode); `agent_grants:
    off` denies it server-side; grants onto build-time capability runners
    stay operator-only (`grant_agent` refuses registry agents).

*   [x] The unifi probe arm is unit-tested (posts the login body; unknown
    kinds report red, never silently green).

*   [ ] Live-world leg (on the release's test run): the CPA provisions an ssh
    door for AI onto a real box on the network and the AI agent execs through
    it; the same for Network against a UniFi controller.
