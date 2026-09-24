package cpbuild

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"freehold/contract/console"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// provision_runner — the CPA's on-the-fly capability grant flow (0.7.4): the
// operator asks in conversation ("I have an RTX3090 box at ..., manage it for
// me"), the department interviews, the CPA calls this to stand the runner up
// and grant the requester onto it. The guardrails are the granting skill's;
// the code enforces the mechanical ones: new capability = NEW runner (never a
// widening — grants attach only to runners recorded as agent-provisioned),
// the credential is sealed on arrival (never persisted plaintext, never for
// ssh — the runner mints its own keypair), and grantees must be existing
// registry agents.
//
// The confirmation discipline (grant immediately when the operator's ask is in
// the CPA's own thread, else DM the operator first) lives in the granting
// skill — the server cannot see Buzz threads; `agent_grants: off` in the CP
// state is the server-side kill switch.

// runnerNameRe constrains runner names to systemd-unit/dir-safe kebab case
// (the name flows into the unit name, the package dir, and the channel name).
var runnerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// agentProvisionKinds are the kinds an on-the-fly runner may use: ssh (a box
// on the network) and the api-class kinds whose exec runs locally with the
// credential + address injected as env (unifi today; a new kind needs a
// runner-side verify arm to report green).
var agentProvisionKinds = map[string]bool{
	"ssh": true, "unifi": true,
}

func supportedProvisionKinds() []string {
	out := make([]string, 0, len(agentProvisionKinds))
	for k := range agentProvisionKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildProvisionRunner binds the provision_runner flow to a Spec + the
// agent-tools registry (grants + grantee lookup). The re-apply leg reuses the
// create deploy (idempotent kubectl apply) after re-pointing THIS process's
// DepartmentRunners at the full coord set resolved from state, so the
// re-applied pod carries every runner — static, per-zone DNS, and dynamic —
// not just the new one.
func BuildProvisionRunner(spec *Spec, reg *agenttools.Registry) agent.ProvisionRunnerFn {
	create := BuildCreateAgentFn(spec)
	return func(args agent.ProvisionArgs) (string, error) {
		name := strings.TrimSpace(args.Name)
		if !runnerNameRe.MatchString(name) {
			return "", fmt.Errorf("provision_runner: name must be kebab-case [a-z0-9-] (got %q) — <target>-<protocol>-<identity>", args.Name)
		}
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if !agentProvisionKinds[kind] {
			return "", fmt.Errorf("provision_runner %s: unsupported kind %q (supported: %s)", name, args.Kind, strings.Join(supportedProvisionKinds(), ", "))
		}
		if strings.TrimSpace(args.Address) == "" {
			return "", fmt.Errorf("provision_runner %s: address is required (user@host[:port] for ssh, the base URL for %s)", name, kind)
		}
		if kind == "ssh" && strings.TrimSpace(args.Secret) != "" {
			return "", fmt.Errorf("provision_runner %s: ssh runners mint their own keypair — leave secret empty and install the returned public key on the target", name)
		}
		if kind != "ssh" && strings.TrimSpace(args.Secret) == "" {
			return "", fmt.Errorf("provision_runner %s: a %s runner needs the operator-supplied credential as secret", name, kind)
		}
		grantTo := dedupNonBlank(args.GrantTo)
		if len(grantTo) == 0 {
			return "", fmt.Errorf("provision_runner %s: grant_to is required (which agent does this capability belong to?)", name)
		}
		if spec.CpaName != "" {
			for _, g := range grantTo {
				if g == spec.CpaName {
					return "", fmt.Errorf("provision_runner %s: the CPA holds no exec — grant to the department or agent that does the work", name)
				}
			}
		}

		cpState := spec.consoleStateDir()
		store, err := state.Open(cpState)
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		if rec, ok := store.GetRunner(name); ok && rec.Status == state.RunnerRevoked {
			return "", fmt.Errorf("provision_runner %s: runner is revoked — pick a new name", name)
		}

		port := nextCapabilityPort(store)
		if prev, ok := store.GetCapability(name); ok {
			port = prev.Port // coords stay stable across re-provisions
		}
		pkgDir := runnerPackageDir(cpState, name)

		// Create (first call) or adopt (re-provision): the package is the
		// identity — an existing one is never re-keyed, so the ssh public line
		// is stable and re-returned. A re-provision with a moved endpoint
		// (the box changed IP) updates the package's target address.
		var pubLine string
		if _, exists := store.GetRunner(name); !exists {
			if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
				Name: name, Kind: kind, Address: strings.TrimSpace(args.Address),
				Secret: []byte(args.Secret), RunnerDir: pkgDir,
			}); err != nil {
				return "", fmt.Errorf("provision %s: %w", name, err)
			}
			for _, extra := range sortedExtraList(args.Extras) {
				if _, err := provisioner.AddSecret(store, name, extra.name, []byte(extra.value)); err != nil {
					return "", fmt.Errorf("seal extra %s: %w", extra.name, err)
				}
			}
		} else if addr := strings.TrimSpace(args.Address); addr != pkgTargetAddr(pkgDir, name) {
			if err := setTargetAddress(store, name, addr); err != nil {
				return "", fmt.Errorf("move %s target address: %w", name, err)
			}
		}
		if kind == "ssh" {
			if pubLine, err = departmentRunnerPubLine(pkgDir, name); err != nil {
				return "", fmt.Errorf("read %s public key: %w", name, err)
			}
		}

		// Record the dynamic capability BEFORE staging so a failure after this
		// point heals on the next build/reconcile (the record re-stages it).
		if err := store.InsertCapability(name, state.CapabilityRecord{
			Kind: kind, Address: strings.TrimSpace(args.Address), Port: port,
			Rosters:   append([]string(nil), grantTo...),
			CreatedAt: uint64(time.Now().Unix()),
		}); err != nil {
			return "", fmt.Errorf("record capability: %w", err)
		}

		// Stage: relay channel (the audit stream) + community membership + the
		// systemd unit on the CP guest. Idempotent; the same tail the build's
		// ensureCapabilityRunner runs on a rebuild.
		if err := provisioner.SyncRunnerChannel(store, spec.relayDialURL(), spec.relaySignURL(), name, cpState); err != nil {
			return "", fmt.Errorf("sync relay channel: %w", err)
		}
		rec, _ := store.GetRunner(name)
		if err := spec.addRelayCommunityMember(rec.NostrPubkey); err != nil {
			return "", fmt.Errorf("relay community membership: %w", err)
		}
		r := capabilityRunner{name: name, kind: kind, port: port,
			rosters: append([]string(nil), grantTo...), addr: strings.TrimSpace(args.Address), dynamic: true}
		if err := spec.startCapabilityRunner(r, pkgDir); err != nil {
			return "", fmt.Errorf("start runner: %w", err)
		}

		// Grant each grantee onto the runner's roster (live — the runner
		// re-reads the signed 39002 per call), then re-apply the grantee's pod
		// so its exec surface carries the new coords.
		granted := []string{}
		for _, grantee := range grantTo {
			row, err := registryRow(reg, grantee)
			if err != nil {
				return "", err
			}
			if _, err := reg.Grant(name, row.Pubkey); err != nil {
				return "", fmt.Errorf("grant %s → %s: %w", grantee, name, err)
			}
			granted = append(granted, grantee)
			// Make THIS process's coord map authoritative for the grantee so
			// the re-apply carries every runner (static + DNS + dynamic), not
			// just the new one.
			if spec.DepartmentRunners == nil {
				spec.DepartmentRunners = map[string][]agent.RunnerCoords{}
			}
			spec.DepartmentRunners[grantee] = spec.agentRunnerCoords(store, grantee)
			channels, private := reconciledChannels(row)
			if _, err := create(grantee, row.Purpose, channels, private); err != nil {
				return "", fmt.Errorf("re-apply %s pod: %w", grantee, err)
			}
		}

		report := fmt.Sprintf("runner %s (%s → %s) listening on %s:%d, granted to [%s].",
			name, kind, strings.TrimSpace(args.Address), spec.CpIP, port, strings.Join(granted, ", "))
		if pubLine != "" {
			report += "\nSSH public key (install in the target's authorized_keys — the runner's self-check goes green once it is):\n" + pubLine
		}
		report += "\nVerify with the runner's self-check before claiming the capability is live."
		return report, nil
	}
}

// runnerPackageDir is the package-dir convention (cpState/runner/<name>) — the
// same layout ensureCapabilityRunner uses.
func runnerPackageDir(cpState, name string) string {
	return cpState + "/runner/" + name
}

// agentRunnerCoords resolves the FULL capability-runner coord list a pod for
// `name` carries: every runner whose rosters include the agent — the static
// table, the per-zone DNS doors, and the agent-provisioned records — with
// pubkeys read from the console state. Runners not yet staged (no record) are
// skipped: the next build lights them.
func (s *Spec) agentRunnerCoords(store *state.StateStore, name string) []agent.RunnerCoords {
	runners := capabilityRunners()
	runners = append(runners, s.cloudflareRunners(store)...)
	runners = append(runners, dynamicRunners(store)...)
	out := []agent.RunnerCoords{}
	for _, r := range runners {
		if !containsRoster(r.rosters, name) {
			continue
		}
		rec, ok := store.GetRunner(r.name)
		if !ok || rec.NostrPubkey == "" {
			continue
		}
		out = append(out, agent.RunnerCoords{
			URL:    fmt.Sprintf("http://%s:%d", s.CpIP, r.port),
			Pubkey: rec.NostrPubkey,
			Target: r.name,
			Secret: r.name,
		})
	}
	return out
}

func containsRoster(rosters []string, name string) bool {
	for _, r := range rosters {
		if r == name {
			return true
		}
	}
	return false
}

// registryRow resolves an agent NAME to its registry row (pubkey + the
// purpose/channels its pod re-apply needs).
func registryRow(reg *agenttools.Registry, name string) (console.AgentInfo, error) {
	rows, err := reg.Agents()
	if err != nil {
		return console.AgentInfo{}, fmt.Errorf("read agent registry: %w", err)
	}
	for _, a := range rows {
		if a.Name == name {
			return a, nil
		}
	}
	return console.AgentInfo{}, fmt.Errorf("unknown agent %q — create it first (create_agent), then grant", name)
}

// dedupNonBlank trims, drops blanks, and dedupes a name list (stable order).
func dedupNonBlank(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

type namedValue struct{ name, value string }

// sortedExtraList keeps the extra-seal order deterministic (tests + logs).
func sortedExtraList(extras map[string]string) []namedValue {
	names := make([]string, 0, len(extras))
	for n := range extras {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]namedValue, 0, len(names))
	for _, n := range names {
		out = append(out, namedValue{n, extras[n]})
	}
	return out
}
