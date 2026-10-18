package cpbuild

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"freehold/contract/relay"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/cpstate"
	provisioner "freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// fill_door + door_package — the no-secret credential path and the
// package fetch, the two legs that take the operator out of a config-only
// door's chain without ever letting an agent judge "is this one secret?":
//
//   - A door DECLARED no_secret at provision time (the declaration is the
//     judgment — made once, while constructing the door) is sealed by the
//     provisioning agent via fill_door, riding the SAME provisioner seal
//     path the console's fill uses. A door without the declaration is
//     agent-unfillable forever; its credential is sealed only by the
//     operator's console fill.
//   - A self-hosted door's ready-to-ship package is built from the CP's own
//     record and fetched by the door's GRANTEE (the live relay roster is the
//     gate, fail-closed) — the operator never copy-pastes the package JSON
//     through chat. The package is ciphertext sealed to the runner's own
//     key: the agent carrying it learns nothing it did not already stage.

// BuildFillDoor binds the fill_door flow to a Spec. The dispatch gates the
// CALLER (the CPA's grant surface, the same agent_grants kill switch); this
// flow gates the DOOR: declared no_secret, agent-provisioned, and a fillable
// kind — ssh carries its own keypair and a kube slot's token is CP-sourced,
// so neither is agent-sealable under any declaration.
func BuildFillDoor(spec *Spec) agent.FillDoorFn {
	return func(name, value string) (string, error) {
		name = strings.TrimSpace(name)
		if !runnerNameRe.MatchString(name) {
			return "", fmt.Errorf("fill_door: name must be kebab-case [a-z0-9-] (got %q)", name)
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("fill_door %s: value is required (the credential-as-config the door was declared to take)", name)
		}
		cpState := spec.consoleStateDir()
		store, err := state.Open(cpState)
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		rec, ok := store.GetCapability(name)
		if !ok {
			return "", fmt.Errorf("fill_door %s: no capability record — only doors provision_runner gave (declared no_secret) are agent-sealable", name)
		}
		if !rec.AgentProvisioned() {
			return "", fmt.Errorf("fill_door %s: this door was provisioned by the operator — its credential is sealed only by the console's fill", name)
		}
		if rec.Kind == "ssh" {
			return "", fmt.Errorf("fill_door %s: ssh doors carry their own minted keypair — there is nothing to fill", name)
		}
		if rec.Kind == "kubernetes" {
			return "", fmt.Errorf("fill_door %s: a kube slot's credential is the SA token the CP sourced itself — never agent-sealable", name)
		}
		if !rec.NoSecret {
			return "", fmt.Errorf("fill_door %s: this door was not declared no_secret — its credential is operator-sealed (the console's fill is the only path); if the value is genuinely not a secret, provision a NEW door with no_secret=true", name)
		}
		if rec.Hosted == state.HostedSelf {
			// RotateSecretSelfHosted carries the operator-confirm barrier itself
			// (EnrollConfirmedAt) — the fill binds to the key the guest actually
			// holds, whoever does the filling.
			if _, _, err := provisioner.RotateSecretSelfHosted(store, name, []byte(value)); err != nil {
				return "", fmt.Errorf("fill_door %s: %w", name, err)
			}
			host := rec.Host
			if host == "" {
				host = "the target"
			}
			return fmt.Sprintf("fill_door: %s sealed (declared no-secret). The GRANTEE fetches the package with door_package(%q) and places it on %s — secrets.json beside the runner's identity.json, then restart the unit there. Verify by exec-probe before claiming the door is live.", name, name, host), nil
		}
		if _, err := provisioner.RotateSecret(store, name, []byte(value)); err != nil {
			return "", fmt.Errorf("fill_door %s: %w", name, err)
		}
		// A capability door picks a sealed credential up ONLY on restart (the
		// runner holds its package in memory from boot). The seal stands even
		// when the restart fails — reported soft, never blocking (the
		// console's fill carries the same semantics).
		restartNote := "the unit was restarted to pick it up"
		if err := restartDoorUnit(name, rec.Port); err != nil {
			restartNote = "SEALED, but the restart did not verify: " + err.Error() + " — restart freehold-runner-" + name + " by hand before exec"
		}
		return fmt.Sprintf("fill_door: %s sealed (declared no-secret), %s. Verify by exec-probe before claiming the door is live.", name, restartNote), nil
	}
}

// restartDoorUnit restarts a CP-guest door's runner unit on THIS guest (the
// agent-tools serve runs there, as the console does) and waits for the listen.
func restartDoorUnit(name string, port int) error {
	unit := "freehold-runner-" + name
	cmd := exec.Command("systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not listen on %d after restart", unit, port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// BuildDoorPackage binds the door_package fetch to a Spec. The gate is the
// door's LIVE relay roster — the same signed 39002 the runner itself reads
// per call — so only the door's grantees may fetch, and a relay outage fails
// closed exactly as the runner's own grant check does. Agent-provisioned
// doors only: a console-provisioned door's package hand-off stays on its
// door page (the console renders it there).
func BuildDoorPackage(spec *Spec) agent.DoorPackageFn {
	return func(name, callerPubkey string) (string, error) {
		name = strings.TrimSpace(name)
		if !runnerNameRe.MatchString(name) {
			return "", fmt.Errorf("door_package: name must be kebab-case [a-z0-9-] (got %q)", name)
		}
		cpState := spec.consoleStateDir()
		store, err := state.Open(cpState)
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		rec, ok := store.GetCapability(name)
		if !ok {
			return "", fmt.Errorf("door_package %s: no capability record — only doors provision_runner gave are fetchable (a console-provisioned door's package rides its door page)", name)
		}
		if !rec.AgentProvisioned() {
			return "", fmt.Errorf("door_package %s: this door was provisioned by the operator — its package hand-off lives on the door page", name)
		}
		if rec.Hosted != state.HostedSelf {
			return "", fmt.Errorf("door_package %s: not a self-hosted door — the CP ships a CP-guest door's package itself; there is nothing to fetch", name)
		}
		secret, ok := store.GetSecret(name)
		if !ok {
			return "", fmt.Errorf("door_package %s: no credential record — the door's staging is incomplete", name)
		}
		// The pending placeholder is not a package anyone should ship — a
		// no_secret door's value is filled via fill_door first, which is what
		// stamps RotatedAt on every door kind.
		if secret.RotatedAt == nil {
			return "", fmt.Errorf("door_package %s: the credential is not sealed yet — fill first (fill_door for a declared no-secret door, the console's fill otherwise), then fetch", name)
		}
		runnerPK := rpkOrEmpty(store, cpState, name)
		if runnerPK == "" {
			return "", fmt.Errorf("door_package %s: the door's relay identity is unresolvable — its roster cannot be checked (fail-closed)", name)
		}
		consoleSecret, cerr := cpstate.ConsoleSecret(cpState)
		if cerr != nil {
			return "", fmt.Errorf("door_package %s: the channel-owner credential is unavailable (%v) — the roster could not be read (fail-closed)", name, cerr)
		}
		if spec.RelayPK == "" {
			return "", fmt.Errorf("door_package %s: no relay pubkey recorded in this build — the roster could not be read (fail-closed)", name)
		}
		have, qerr := relay.QueryChannelRosterAuth(spec.relayDialURL(), spec.relaySignURL(), spec.RelayPK, runnerPK, consoleSecret)
		if qerr != nil {
			return "", fmt.Errorf("door_package %s: the live roster could not be read (%v) — failing closed, the same posture the runner's own grant check takes", name, qerr)
		}
		admitted := false
		for _, pk := range have {
			if pk == callerPubkey {
				admitted = true
				break
			}
		}
		if !admitted {
			return "", fmt.Errorf("door_package %s: the caller is not on the door's roster — only its grantees fetch its package", name)
		}
		pkgJSON, err := provisioner.PackageJSON(secret)
		if err != nil {
			return "", fmt.Errorf("door_package %s: %w", name, err)
		}
		host := rec.Host
		if host == "" {
			host = "the target"
		}
		return fmt.Sprintf("package for %s (resident on %s) — write it on the guest as secrets.json beside the runner's identity.json (the dir `runner enroll` minted), then restart the unit there and verify by exec-probe:\n\n%s",
			name, host, pkgJSON), nil
	}
}
