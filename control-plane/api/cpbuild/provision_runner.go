package cpbuild

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
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

// reservedProvisionKinds are NOT agent-provisionable: kind=local is the
// self-hosted enroll flow (hosted=self only). kind=kubernetes WAS reserved
// when its token had no build-time source to re-seal from — kube slots lifted
// that: a dynamic kube door's record carries the slot (ns + quota), the build
// re-creates the slot and re-seals the token every rebuild exactly as
// doors.tf does for the static ones (kube_slot.go). The slot's CARVE stays
// outside this flow — it is Compute's audited kube-api-root leg.
var reservedProvisionKinds = map[string]bool{
	"local": true,
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
		if kind == "" {
			return "", fmt.Errorf("provision_runner %s: kind is required (ssh, or an api-class kind with a probe)", name)
		}
		hosted := strings.ToLower(strings.TrimSpace(args.Hosted))
		if hosted != "" && hosted != state.HostedSelf {
			return "", fmt.Errorf("provision_runner %s: hosted must be \"\" (CP guest) or %q (resident on the target)", name, state.HostedSelf)
		}
		selfHosted := hosted == state.HostedSelf
		host := strings.TrimSpace(args.Host)
		if selfHosted {
			if kind != "local" {
				return "", fmt.Errorf("provision_runner %s: hosted=self runs the connector on the target itself — kind must be \"local\" (got %q)", name, kind)
			}
			// A BARE host (the pinned name or LAN IP) — the port is
			// CP-allocated, so a host:port here would record a door whose
			// coords can never work.
			if host == "" || strings.ContainsAny(host, ":/ \t") {
				return "", fmt.Errorf("provision_runner %s: host is required for hosted=self — a BARE host (the pinned name or LAN IP, no port — the CP allocates it), got %q", name, args.Host)
			}
			if !provisioner.IsPubkey(args.Pubkey) || !provisioner.IsPubkey(args.EncPubkey) {
				return "", fmt.Errorf("provision_runner %s: hosted=self enrolls the target's own identity — pubkey and enc_pubkey (64-hex each, from `runner enroll` on the guest) are required", name)
			}
		} else {
			if reservedProvisionKinds[kind] {
				return "", fmt.Errorf("provision_runner %s: kind %q is reserved (local = the hosted=self enroll flow)", name, kind)
			}
			// host is a self-hosted-only field: accepted on a CP-guest door it
			// would silently repoint the granted pods' exec surface elsewhere
			// while the unit still starts on the CP guest.
			if host != "" {
				return "", fmt.Errorf("provision_runner %s: host is a hosted=self field — a CP-guest door always dials the CP itself", name)
			}
		}
		// Kube slots: kind=kubernetes provisions a namespace-scoped door —
		// full access within one namespace, never cluster scope. The slot's
		// CARVE is Compute's audited leg (kube-api-root, in conversation);
		// this flow verifies the slot, reads its SA token and seals it, so
		// the credential-blind property holds: the CP sources the token from
		// the cluster, never from any agent's context, and the door is live
		// with no console fill.
		ns, quota := strings.TrimSpace(args.NS), strings.TrimSpace(args.Quota)
		kube := kind == "kubernetes"
		if kube {
			if !strings.HasPrefix(name, kubeDoorNamePrefix) {
				return "", fmt.Errorf("provision_runner %s: kube doors are named %s<slot> (e.g. kube-api-homelab) — what it reaches, how, at what level", name, kubeDoorNamePrefix)
			}
			if spec.K3sVmid == 0 {
				return "", fmt.Errorf("provision_runner %s: no k3s guest is recorded — the world has no kube yet (a build brings it up)", name)
			}
			if ns != "" {
				if err := validateKubeNS(ns); err != nil {
					return "", fmt.Errorf("provision_runner %s: %w", name, err)
				}
			}
			if quota != "" {
				if _, err := parseKubeQuota(quota); err != nil {
					return "", fmt.Errorf("provision_runner %s: %w", name, err)
				}
			}
			// The standard verify arm — a SelfSubjectReview proves BOTH
			// reachability and that the sealed token survived a CA rotation.
			// Never agent-supplied for kube: the CP knows the API.
			args.Probe = kubernetesProbe
			args.ProbeBody = kubernetesProbeBody
			if strings.TrimSpace(args.Address) == "" {
				// The same derivation the static kube doors get (the gateway's
				// route to the k3s API).
				args.Address = "https://" + config.StripCIDR(spec.ProxyIP) + ":6443"
			}
		}
		if strings.TrimSpace(args.Address) == "" {
			return "", fmt.Errorf("provision_runner %s: address is required (user@host[:port] for ssh, the base URL for %s)", name, kind)
		}
		// (The tool is credential-blind BY CONSTRUCTION: ProvisionArgs carries
		// no secret/extras at all — an unknown "secret" in the call JSON is
		// dropped by the unmarshal and never reaches this code. ssh mints its
		// own keypair; api doors ship EMPTY for the console fill.)
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

		// Name guards (pure string checks, before any I/O): a build-time
		// capability runner is operator-owned — refusing its name keeps the CPA
		// from shadowing it with an agent-keyed door or adopt-granting onto it.
		// The per-zone DNS doors share a reserved prefix (their names derive
		// from the stored zones — today's AND any future one).
		for _, static := range capabilityRunners() {
			if static.name == name {
				return "", fmt.Errorf("provision_runner %s: a build-time capability runner is operator-owned — provision a NEW runner instead (never widen one)", name)
			}
		}
		if strings.HasPrefix(name, "cloudflare-api-") {
			return "", fmt.Errorf("provision_runner %s: the cloudflare-api- prefix is reserved (per-zone DNS doors are operator-owned)", name)
		}

		cpState := spec.consoleStateDir()
		store, err := state.Open(cpState)
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		// A retired door's name is refused to the agent surface, read BEFORE the
		// runner-row guards: after a retire the row survives as REVOKED, so the
		// checks below would answer "runner is revoked — pick a new name" or "was
		// not provisioned by an agent" to what is really "revoke_runner took this
		// away". Take-away and re-grant must not be the same caller's two hands, or
		// a revocation the CPA could instantly undo would be a gesture rather than a
		// control. The name is NOT reserved forever — the operator re-enables it by
		// re-provisioning the door from the console, whose InsertCapability drops
		// the guard.
		if prior, retired := store.GetRetired(name); retired {
			return "", fmt.Errorf("provision_runner %s: this door was retired by revoke_runner (%s) — an agent may not re-mint a capability it was just told to take away; the operator re-enables the name by provisioning it from the console", name, orUnknown(prior.RetiredBy))
		}
		// State guards: an existing runner with NO capability record was
		// provisioned by the operator/console — grants onto it stay
		// operator-scoped. A record with operator provenance likewise.
		prevRecord, hasPrevRecord := store.GetCapability(name)
		_, runnerExists := store.GetRunner(name)
		if rec, ok := store.GetRunner(name); ok {
			if rec.Status == state.RunnerRevoked {
				return "", fmt.Errorf("provision_runner %s: runner is revoked — pick a new name", name)
			}
			if _, isCap := store.GetCapability(name); !isCap {
				return "", fmt.Errorf("provision_runner %s: runner already exists and was not provisioned by an agent — grants onto it stay operator-scoped (the console)", name)
			}
		}
		if hasPrevRecord && !prevRecord.AgentProvisioned() {
			return "", fmt.Errorf("provision_runner %s: this door was provisioned by the operator — grants onto it stay operator-scoped (the console)", name)
		}
		if hasPrevRecord && prevRecord.Kind != "" && prevRecord.Kind != kind {
			return "", fmt.Errorf("provision_runner %s: kind %q is fixed — the door was provisioned as %q (a different kind is a NEW capability, new name)", name, kind, prevRecord.Kind)
		}
		if kube {
			// The ns is FIXED at first provision; a re-provision may omit it
			// (the record keeps it) but never change it — a different ns is a
			// NEW slot and a new door. A restated quota updates the record (the
			// spec the build re-creates the slot from after a k3s rebuild); the
			// in-cluster apply of a resized quota is Compute's leg, same as the
			// carve.
			if ns == "" {
				if !hasPrevRecord {
					return "", fmt.Errorf("provision_runner %s: ns is required — the slot Compute carves (kube-api-root) that this door seals", name)
				}
				ns = prevRecord.NS
			}
			if hasPrevRecord && prevRecord.NS != "" && ns != prevRecord.NS {
				return "", fmt.Errorf("provision_runner %s: ns %s is fixed (the slot was carved as %s) — a different ns is a NEW slot (new door, new name)", name, ns, prevRecord.NS)
			}
			if quota == "" && hasPrevRecord {
				quota = prevRecord.Quota
			}
			for other, rec := range store.Capabilities() {
				if other != name && rec.Kind == "kubernetes" && rec.NS == ns {
					return "", fmt.Errorf("provision_runner %s: ns %s is already %s's slot — one slot per namespace; share the door (grant_to) instead of re-slicing it", name, ns, other)
				}
			}
		}
		if !selfHosted && kind != "ssh" && !runnerExists && strings.TrimSpace(args.Probe) == "" {
			// The verify arm is data, not code: the requesting agent knows the
			// API and names the probe that proves the credential works (the
			// runner's self-check needs it to report honest status). Re-provisions
			// may omit it ("" keeps the door's existing arm).
			return "", fmt.Errorf("provision_runner %s: api-class doors require a probe — \"<METHOD> <path> [auth] [want]\" (e.g. \"GET /user/tokens/verify bearer\"; auth: bearer (default) | basic | json-body | none)", name)
		}

		// Resolve EVERY grantee up front (a re-provision may shrink the roster
		// or rename it — the whole list must be resolvable BEFORE any side
		// effect, or a typo'd name would leave a live runner + record behind).
		granteeRows := map[string]console.AgentInfo{}
		for _, g := range grantTo {
			row, err := registryRow(reg, g)
			if err != nil {
				return "", fmt.Errorf("provision_runner %s: %w", name, err)
			}
			granteeRows[g] = row
		}

		port := spec.NextCapabilityPort(store)
		if hasPrevRecord {
			port = prevRecord.Port // coords stay stable across re-provisions
		}
		pkgDir := runnerPackageDir(cpState, name)

		// Create (first call) or adopt (re-provision): the package is the
		// identity — an existing one is never re-keyed, so the ssh public line
		// is stable and re-returned. A re-provision updates the endpoint (the
		// box changed IP); the credential is ONLY ever sealed by the console
		// (the operator's fill) — a re-provision restarts the door to load it.
		// A SELF-HOSTED runner has no CP-side package at all: its identity was
		// minted on the guest and is presented here — a re-provision must
		// present the SAME pubkeys (a mismatch is a different process claiming
		// the name — refuse loudly).
		adopted := false // self-hosted: the record carries a CONFIRMED enrollment only on the adopt path
		var pubLine string
		if selfHosted {
			if !runnerExists {
				if _, err := provisioner.EnrollRunner(store, name, kind,
					strings.TrimSpace(args.Address), args.Pubkey, args.EncPubkey,
					net.JoinHostPort(host, strconv.Itoa(port)),
				); err != nil {
					return "", fmt.Errorf("enroll %s: %w", name, err)
				}
			} else {
				rec, _ := store.GetRunner(name)
				if rec.Status == state.RunnerRevoked {
					return "", fmt.Errorf("provision_runner %s: runner is revoked — pick a new name", name)
				}
				if rec.NostrPubkey != args.Pubkey || rec.EncPubkey != args.EncPubkey {
					return "", fmt.Errorf("provision_runner %s: the presented pubkeys do not match the enrolled runner — the guest's identity IS the runner (re-enroll under a new name, or fix the guest's state dir)", name)
				}
				adopted = true
			}
			if before, ok := store.GetSecret(name); ok && before.Address != strings.TrimSpace(args.Address) {
				if err := setTargetAddress(store, name, strings.TrimSpace(args.Address)); err != nil {
					return "", fmt.Errorf("move %s target address: %w", name, err)
				}
			}
			// The runner's unit lives on the guest; a moved host updates the
			// console's readiness probe target.
			mcpAddr := net.JoinHostPort(host, strconv.Itoa(port))
			if err := store.SetRunnerMcpAddr(name, &mcpAddr); err != nil {
				return "", fmt.Errorf("update %s mcp addr: %w", name, err)
			}
		} else if !runnerExists {
			// The sealed secret per kind: ssh mints its OWN keypair (the
			// private half seals; the public line returns for the operator's
			// one-time install); api doors seal the empty-shell "pending"
			// placeholder for the console fill; kube slots seal the SA token
			// read from the carved slot (Compute's audited leg put it there).
			secret := []byte("pending")
			if kind == "ssh" {
				priv, _, err := crypto.GenerateSSHKeypair(name)
				if err != nil {
					return "", fmt.Errorf("generate %s ssh keypair: %w", name, err)
				}
				secret = priv
			}
			if kube {
				if err := spec.kubeSlotReady(ns); err != nil {
					return "", kubeCarveMissing(ns, quota, err)
				}
				token, err := spec.kubeSlotToken(ns)
				if err != nil {
					return "", err
				}
				secret = token
			}
			if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
				Name: name, Kind: kind, Address: strings.TrimSpace(args.Address),
				Secret: secret, RunnerDir: pkgDir,
				Probe: args.Probe, ProbeBody: args.ProbeBody,
			}); err != nil {
				return "", fmt.Errorf("provision %s: %w", name, err)
			}
		} else if addr := strings.TrimSpace(args.Address); addr != pkgTargetAddr(pkgDir, name) {
			if err := setTargetAddress(store, name, addr); err != nil {
				return "", fmt.Errorf("move %s target address: %w", name, err)
			}
		}
		// A re-provision may restate the verify arm ("" keeps the door's
		// existing probe): same update-and-ship path as an address move, and
		// the restart below loads it.
		if kind != "ssh" && !selfHosted && runnerExists {
			if strings.TrimSpace(args.Probe) == "" && args.ProbeBody != "" {
				return "", fmt.Errorf("provision_runner %s: probe_body requires probe (a blank probe keeps the door's existing arm — send both to restate it)", name)
			}
			probe, probeBody := args.Probe, args.ProbeBody
			if strings.TrimSpace(probe) != "" {
				var err error
				if probe, err = provisioner.ValidateProbe(probe); err != nil {
					return "", fmt.Errorf("provision_runner %s: %w", name, err)
				}
				if probeBody != "" {
					if err := provisioner.ValidateProbeBody(probeBody); err != nil {
						return "", fmt.Errorf("provision_runner %s: %w", name, err)
					}
				}
			}
			if before, ok := store.GetSecret(name); ok {
				if strings.TrimSpace(probe) == "" {
					probe, probeBody = before.Probe, before.ProbeBody
				}
				if probe != before.Probe || probeBody != before.ProbeBody {
					if err := setTargetProbe(store, name, probe, probeBody); err != nil {
						return "", fmt.Errorf("restate %s verify arm: %w", name, err)
					}
				}
			}
		}
		if kind == "ssh" && !selfHosted {
			if pubLine, err = departmentRunnerPubLine(pkgDir, name); err != nil {
				return "", fmt.Errorf("read %s public key: %w", name, err)
			}
		}

		// Record the dynamic capability BEFORE staging so a failure after this
		// point heals on the next build/reconcile (the record re-stages it).
		// A re-provision:
		//   - preserves the operator's enrollment confirmation ONLY on the
		//     adopt path (the runner row exists AND presented the same
		//     pubkeys) — a fresh enroll is a NEW identity, never pre-confirmed;
		//   - UNIONS the self-hosted Rosters with the previous record's: a
		//     resident door's re-provision is a MOVE/repair op and must never
		//     silently revoke a console-granted agent — the console's ungrant
		//     is the revoke path there. (A CP-guest re-provision keeps the
		//     shrink semantics: grant_to is the whole desired roster and the
		//     dropped agents are revoked below.)
		capRec := state.CapabilityRecord{
			Kind: kind, Address: strings.TrimSpace(args.Address), Port: port,
			Rosters: append([]string(nil), grantTo...), Origin: state.OriginAgent,
			Hosted: hosted, Host: host,
			NS: ns, Quota: quota,
			CreatedAt: uint64(time.Now().Unix()),
		}
		if hasPrevRecord {
			if adopted {
				capRec.EnrollConfirmedAt = prevRecord.EnrollConfirmedAt
			}
			if selfHosted {
				for _, r := range prevRecord.Rosters {
					if !containsRoster(capRec.Rosters, r) {
						capRec.Rosters = append(capRec.Rosters, r)
					}
				}
			}
		}
		if err := store.InsertCapability(name, capRec); err != nil {
			return "", fmt.Errorf("record capability: %w", err)
		}

		// Stage: relay channel (the audit stream) + community membership, then
		// the systemd unit on the CP guest — self-hosted runners run their own
		// unit on the target, so the CP stages everything EXCEPT the process.
		// Idempotent; the same tail the build's ensureCapabilityRunner runs on
		// a rebuild.
		if err := provisioner.SyncRunnerChannel(store, spec.relayDialURL(), spec.relaySignURL(), name, cpState); err != nil {
			return "", fmt.Errorf("sync relay channel: %w", err)
		}
		rec, _ := store.GetRunner(name)
		if err := spec.addRelayCommunityMember(rec.NostrPubkey); err != nil {
			return "", fmt.Errorf("relay community membership: %w", err)
		}
		r := capabilityRunner{name: name, kind: kind, port: port,
			rosters: append([]string(nil), capRec.Rosters...), addr: strings.TrimSpace(args.Address), dynamic: true,
			selfHosted: selfHosted, host: host}
		if !selfHosted {
			if err := spec.startCapabilityRunner(r, pkgDir); err != nil {
				return "", fmt.Errorf("start runner: %w", err)
			}
		}

		// Grant each grantee onto the runner's roster (live — the runner
		// re-reads the signed 39002 per call), then re-apply the grantee's pod
		// so its exec surface carries the new coords. A CP-guest re-provision
		// that SHRANK the roster revokes the dropped agents first — a grant
		// that could not be revoked must not be made. A self-hosted
		// re-provision revokes nobody: its record UNIONed the rosters above
		// (the move/repair op never silently revokes; the console's ungrant
		// is the revoke path there).
		for _, dropped := range droppedRosters(prevRecord.Rosters, capRec.Rosters) {
			row, err := registryRow(reg, dropped)
			if err != nil {
				return "", fmt.Errorf("re-provision %s: the dropped grantee %s is no longer in the registry — revoke it by hand (console) so its exec does not linger: %w", name, dropped, err)
			}
			if err := provisioner.RemoveUserMembership(store, spec.relayDialURL(), spec.relaySignURL(), name, row.Pubkey, cpState); err != nil {
				return "", fmt.Errorf("revoke dropped grantee %s from %s: %w", dropped, name, err)
			}
		}
		granted := []string{}
		for _, grantee := range grantTo {
			row := granteeRows[grantee]
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
			if _, err := create(grantee, row.Purpose, channels, private, row.Model); err != nil {
				return "", fmt.Errorf("re-apply %s pod: %w", grantee, err)
			}
		}

		report := ""
		if selfHosted {
			// The relay dial is the LAN IP:3000 — a sandbox guest has no
			// /etc/hosts pin for the relay hostname (the CP guest does), and
			// the public edge does not listen on 3000. The NIP-98 signature
			// stays canonical via FREEHOLD_RELAY_AUTH_URL (the dial-LAN /
			// sign-public split).
			report = fmt.Sprintf("runner %s (local on %s, resident) enrolled; roster [%s]; pods dial %s:%d. "+
				"The CP holds no identity and starts nothing — the unit runs ON %s as User=lxcadmin, and the OPERATOR must CONFIRM the enrollment on the door page (verify the presented pubkeys against %s's own `runner enroll` output / Compute's report) before the credential fill unlocks. "+
				"The unit: EnvironmentFile=/home/lxcadmin/.freehold/serve.env (write it root-side: FREEHOLD_STATE_DIR=/home/lxcadmin/.freehold — the dir `runner enroll` minted into, or serve mints a DIFFERENT identity under / — plus FREEHOLD_RUNNER_ADDR=0.0.0.0:%d, FREEHOLD_RELAY_URL=http://%s:3000, FREEHOLD_RELAY_PUBKEY=%s, FREEHOLD_RELAY_AUTH_URL=%s, FREEHOLD_RUNNER_ALLOW_REMOTE=1), ExecStart the runner binary `serve`.",
				name, host, strings.Join(granted, ", "), host, port, host, name, port,
				spec.RelayIP, spec.RelayPK, spec.relaySignURL())
		} else {
			report = fmt.Sprintf("runner %s (%s → %s) listening on %s:%d, granted to [%s].",
				name, kind, strings.TrimSpace(args.Address), spec.CpIP, port, strings.Join(granted, ", "))
		}
		if pubLine != "" {
			report += "\nSSH public key (install in the target's authorized_keys — the runner's self-check goes green once it is):\n" + pubLine
		}
		if kube {
			q := quota
			if q == "" {
				q = "none"
			}
			report += fmt.Sprintf("\nSlot ns %s (quota %s): carved by Compute through kube-api-root; the CP read its SA token and sealed it — the door is live, no console fill. Its record re-creates the slot and re-seals the token on every build (a k3s rebuild rotates the CA).", ns, q)
		} else {
			// Every door has its own console URL; an empty-shell door's link is
			// how the operator fills the credential (never via any agent's chat).
			link := doorLink(spec, name)
			report += "\nDoor page: " + link
			if kind != "ssh" && !selfHosted {
				report += "\nThe door ships EMPTY: DM the operator that link (log in first if asked — the page opens the door's fill form). The console seals the credential and restarts the door; verify by exec-probe before claiming the capability is live."
			}
		}
		if selfHosted {
			report += "\nThe door ships EMPTY: DM the operator that link (log in first if asked) — they verify the presented pubkeys and confirm the enrollment on the page, then fill the credential. The console seals it to the runner's own key and returns the package JSON; the GRANTEE writes it to the guest's state dir as secrets.json (through its own door exec) and restarts the unit there. Verify by exec-probe before claiming the capability is live."
		}
		report += "\nVerify with the runner's self-check before claiming the capability is live."
		return report, nil
	}
}

// droppedRosters returns the names in prev (a door's current roster) that the
// re-provision's roster drops — the grants to revoke.
func droppedRosters(prev, next []string) []string {
	dropped := []string{}
	for _, p := range prev {
		if !containsRoster(next, p) {
			dropped = append(dropped, p)
		}
	}
	return dropped
}

// runnerPackageDir is the package-dir convention (cpState/runner/<name>) — the
// same layout ensureCapabilityRunner uses.
func runnerPackageDir(cpState, name string) string {
	return cpState + "/runner/" + name
}

// doorLink is a door's console URL — the deep link the agents DM the operator
// for the credential fill (the page opens the door's form; login preserves
// the path).
func doorLink(spec *Spec, name string) string {
	if spec.CpHost != "" {
		return "https://" + spec.CpHost + "/runner/" + name
	}
	return fmt.Sprintf("http://%s:8080/runner/%s", spec.CpIP, name)
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
		// A self-hosted runner's pods dial the TARGET's LAN address, not the
		// CP IP (the record carries the host) — gated on the mode, never on
		// the field being merely non-empty.
		dialHost := s.CpIP
		if r.selfHosted && r.host != "" {
			dialHost = r.host
		}
		out = append(out, agent.RunnerCoords{
			URL:    fmt.Sprintf("http://%s:%d", dialHost, r.port),
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
