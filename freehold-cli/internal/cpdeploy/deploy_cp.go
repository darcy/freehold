package cpdeploy

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/version"
	"freehold/contract/wire"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/deploy"
	"freehold/providers/proxmox"
)

// DeployCpSpec mirrors the CP deploy spec.
type DeployCpSpec struct {
	StateDir         string
	BinDir           string
	BindAddr         string
	BinaryPath       string
	RelayURL         string
	RelayPubkey      *string
	RelayHostIP      *string
	AdminPubkeys     []string
	LXc              *uint32
	PublicOrigin     *string
	RunnerBinary     *string
	RunnerPackage    *string
	AgentToolsURL    *string
	AgentToolsPubkey *string
	WorldConfig      *string // cpbuild.Coords JSON (bounds the console as the CP build executor)
	AgentToolsBinary *string // local freehold-agent-tools binary, shipped so the console's world_build can deploy it
	// Pin is the version identity to stamp after a successful deploy (install
	// only). nil on a rebuild/redeploy: build/teardown never promote.
	Pin *version.Pin
}

// DeployCpResult is the CP deploy outcome.
type DeployCpResult struct {
	StateDir string
	BindAddr string
	Pubkey   string
	Detail   string
}

// Transport reaches the Proxmox host: run a host shell command and upload a
// local file. Two backings satisfy it — the runner MCP client (the classic
// path) and a transient root-SSH connection (PR4), so deploy-cp is transport-
// agnostic.
type Transport interface {
	Exec(cmd string, timeoutS uint64) (*client.ExecOutcome, error)
	Upload(localPath, remotePath string, timeoutS uint64) (uint64, error)
}

// ClientTransport adapts a runner MCP client + target to the host transport.
type ClientTransport struct {
	C      *client.McpClient
	Target string
}

func (t ClientTransport) Exec(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return bootstrap.Exec(t.C, t.Target, cmd, timeoutS)
}

func (t ClientTransport) Upload(localPath, remotePath string, timeoutS uint64) (uint64, error) {
	return t.C.Upload(t.Target, localPath, remotePath, timeoutS)
}

// execToOK runs a host command and asserts it succeeded.
func execToOK(t Transport, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	out, err := t.Exec(cmd, timeoutS)
	if err != nil {
		return nil, err
	}
	if err := bootstrap.ExpectOK(out, step); err != nil {
		return nil, err
	}
	return out, nil
}

// shipFile streams a LOCAL file to the target via the runner's sftp upload,
// then moves it into place (pct push inside an LXC, mv on a bare host).
func shipFile(t Transport, spec *DeployCpSpec, localPath, remoteFinal, step string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	localSize := info.Size()
	hostTmp := fmt.Sprintf("/tmp/freehold-ship-%d", os.Getpid())
	_, _ = execToOK(t, "rm -f "+hostTmp, "reset host tmp "+step, 30)
	remoteSize, err := t.Upload(localPath, hostTmp, 300)
	if err != nil {
		return &bootstrap.StepError{Step: "sftp " + step, Output: err.Error()}
	}
	if remoteSize != uint64(localSize) {
		return fmt.Errorf("shipped %s size mismatch: remote %d vs local %d", step, remoteSize, localSize)
	}
	var place string
	if spec.LXc != nil {
		place = fmt.Sprintf("pct push %d %s %s", *spec.LXc, hostTmp, remoteFinal)
	} else {
		place = "mv " + hostTmp + " " + remoteFinal
	}
	if _, err := execToOK(t, place, "place "+step, 120); err != nil {
		return err
	}
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, "chmod 755 "+remoteFinal), "chmod "+step, 30); err != nil {
		return err
	}
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, "wc -c < "+remoteFinal), "verify "+step, 30)
	if err != nil {
		return err
	}
	guestSizeStr := strings.TrimSpace(out.Stdout)
	var guestSize uint64
	if _, err := fmt.Sscanf(guestSizeStr, "%d", &guestSize); err != nil || guestSize != uint64(localSize) {
		return fmt.Errorf("shipped %s guest size mismatch: %s vs local %d", step, guestSizeStr, localSize)
	}
	_, _ = execToOK(t, "rm -f "+hostTmp, "clean "+step, 30)
	return nil
}

// shipSmallFile ships a SMALL file in one exec (base64).
func shipSmallFile(t Transport, spec *DeployCpSpec, localPath, remoteFinal, step string) error {
	bytes, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	b64 := base64StdEncode(bytes)
	parent := "."
	if i := strings.LastIndex(remoteFinal, "/"); i >= 0 {
		parent = remoteFinal[:i]
	}
	cmd := fmt.Sprintf("mkdir -p %s && printf %%s \"%s\" | base64 -d > %s && chmod 600 %s",
		parent, b64, remoteFinal, remoteFinal)
	_, err = execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), step, 60)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StampPin writes the world's version identity to <StateDir>/version.json over
// the deploy transport (0600). It is the version PROMOTION: install calls it at
// the end of a deploy, update calls it last (after migrations succeed), and
// build/teardown never call it. A zero Version is a no-op.
func StampPin(t Transport, spec *DeployCpSpec, pin version.Pin) error {
	if pin.Version == "" {
		return nil
	}
	data, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "fh-version-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return shipSmallFile(t, spec, tmp.Name(), spec.StateDir+"/"+version.FileName, "version.json")
}

// stopPriorServe kills a previously started serve (if any) and clears its pid
// so the binary can be overwritten and a fresh instance started.
func stopPriorServe(t Transport, spec *DeployCpSpec, step string) error {
	cmd := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true",
		spec.StateDir, spec.StateDir)
	_, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), step, 30)
	return err
}

// shipConsoleBins ships the console binary and (when provided) the
// agent-tools binary into the CP's bin dir. A RUNNING agent-tools serve holds
// the binary, so it is stopped first or the overwrite fails/size-mismatches.
func shipConsoleBins(t Transport, spec *DeployCpSpec) error {
	if err := shipFile(t, spec, spec.BinaryPath,
		spec.BinDir+"/freehold-console", "console binary"); err != nil {
		return err
	}
	if spec.AgentToolsBinary == nil || *spec.AgentToolsBinary == "" {
		return nil
	}
	atState := filepath.Join(spec.StateDir, "..", "agent-tools")
	stop := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true", atState, atState)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, stop), "stop prior agent-tools", 30); err != nil {
		return err
	}
	return shipFile(t, spec, *spec.AgentToolsBinary,
		spec.BinDir+"/freehold-agent-tools", "agent-tools binary")
}

// startServe launches the console serve in the guest with the given flag
// string, waits for /healthz, then confirms the started pid is still alive.
// Shared by the full deploy and update's lighter redeploy.
func startServe(t Transport, spec *DeployCpSpec, flags string) (uint32, error) {
	start := fmt.Sprintf(
		"setsid nohup %s/freehold-console serve --state-dir %s --addr %s%s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid",
		spec.BinDir, spec.StateDir, spec.BindAddr, flags, spec.StateDir, spec.StateDir)
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, start), "start control plane", 30)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(out.Stdout)
	var pid uint32
	if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == 0 {
		return 0, fmt.Errorf("start did not yield a pid: %q", out.Stdout)
	}
	healthy := false
	var lastErr error
	for i := 0; i < 15; i++ {
		probe := fmt.Sprintf("curl -fsS -m 3 http://%s/healthz", spec.BindAddr)
		_, err := execToOK(t, proxmox.LxcCmd(spec.LXc, probe), "cp healthz", 20)
		if err == nil {
			healthy = true
			break
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if !healthy {
		return pid, fmt.Errorf("control plane did not answer /healthz on %s within the poll window (last probe error: %v)",
			spec.BindAddr, lastErr)
	}
	alive := fmt.Sprintf("kill -0 %d >/dev/null 2>&1", pid)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, alive), "control plane still alive", 10); err != nil {
		return pid, fmt.Errorf("control plane answered /healthz but the started process (pid %d) is gone — check %s/serve.log (e.g. address already in use)",
			pid, spec.StateDir)
	}
	return pid, nil
}

// relayDomain strips the scheme and trailing slash from a relay URL.
func relayDomain(relayURL string) string {
	d := strings.TrimPrefix(strings.TrimPrefix(relayURL, "https://"), "http://")
	return strings.TrimSuffix(d, "/")
}

// serveFlags builds the console `serve` flag string, shared by the full deploy
// and update's redeploy so the two can never drift. Returns the flags and the
// relay domain derived from the URL.
func serveFlags(spec *DeployCpSpec) (string, string) {
	domain := relayDomain(spec.RelayURL)
	var b strings.Builder
	if len(spec.AdminPubkeys) > 0 {
		b.WriteString(" --admin-pubkeys " + strings.Join(spec.AdminPubkeys, ","))
	}
	if spec.PublicOrigin != nil {
		b.WriteString(" --public-origin " + *spec.PublicOrigin)
	}
	if spec.RelayPubkey != nil && *spec.RelayPubkey != "" {
		scopeURL := spec.RelayURL
		if spec.RelayHostIP != nil {
			scopeURL = fmt.Sprintf("http://%s:3000", *spec.RelayHostIP)
		}
		b.WriteString(fmt.Sprintf(" --relay-url %s --relay-pubkey %s --relay-host %s", scopeURL, *spec.RelayPubkey, domain))
	}
	if spec.AgentToolsURL != nil && *spec.AgentToolsURL != "" {
		b.WriteString(" --agent-tools-url " + *spec.AgentToolsURL)
	}
	if spec.AgentToolsPubkey != nil && *spec.AgentToolsPubkey != "" {
		b.WriteString(" --agent-tools-pubkey " + *spec.AgentToolsPubkey)
	}
	if spec.WorldConfig != nil && *spec.WorldConfig != "" {
		b.WriteString(" --world-config " + base64.StdEncoding.EncodeToString([]byte(*spec.WorldConfig)))
	}
	return b.String(), domain
}

// readGuestFile returns a file's bytes from inside the LXC, or nil when absent
// (the path is trusted: built from a fixed runner dir, no shell metacharacters).
func readGuestFile(t Transport, spec *DeployCpSpec, path string) []byte {
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, "cat "+path+" 2>/dev/null || true"), "read "+path, 30)
	if err != nil {
		return nil
	}
	return []byte(out.Stdout)
}

// runnerShipPlan is the adopt-vs-mint ship decision: which box-package files
// go into the co-located runner dir, and whether the box's secrets.json merges.
// On ADOPT the plane already holds the runner identity AND secrets sealed to
// its own encryption key — skip identity.json and the merge (the box's
// ciphertext would be unopenable). On MINT the box is the identity source.
func runnerShipPlan(adopt bool) (files []string, mergeSecrets bool) {
	if adopt {
		return []string{"known_hosts.json"}, false
	}
	return []string{"identity.json", "known_hosts.json"}, true
}

// guestHasRunnerIdentity reports whether the plane already carries the
// co-located runner's identity (the re-adopt signal, checked through the box
// runner before anything is shipped). A present identity is the durable
// grant/roster anchor and must never be overwritten by the box's.
func guestHasRunnerIdentity(t Transport, spec *DeployCpSpec, runnerDir string) bool {
	out, err := execToOK(t,
		proxmox.LxcCmd(spec.LXc, "test -f "+runnerDir+"/identity.json && echo yes"), "probe runner identity", 30)
	return err == nil && strings.TrimSpace(out.Stdout) == "yes"
}

// shipMergedRunnerSecrets ships the box package's secrets.json into the CP
// runner package, preserving secret NAMES already present on the CP that the
// box package doesn't carry (the build-added litellm trio); the box wins on a
// name overlap.
func shipMergedRunnerSecrets(t Transport, spec *DeployCpSpec, localPath, remoteFinal string) error {
	boxRaw, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	merged, err := mergeRunnerSecrets(boxRaw, readGuestFile(t, spec, remoteFinal))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "fh-runner-secrets-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(merged); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return shipSmallFile(t, spec, tmp.Name(), remoteFinal, "runner secrets.json")
}

// mergeRunnerSecrets unions a runner SecretPackage's secrets/targets maps
// (box entries win on conflict) and keeps the CP's grants. Uses the wire type
// so the per-target `secret` link and serde-compatible JSON survive; a
// corrupt/missing remote file just loses its extras rather than failing.
func mergeRunnerSecrets(boxRaw, cpRaw []byte) ([]byte, error) {
	box := &wire.SecretPackage{}
	if err := json.Unmarshal(boxRaw, box); err != nil {
		return nil, fmt.Errorf("parse box runner secrets: %w", err)
	}
	cp := &wire.SecretPackage{}
	if len(cpRaw) > 0 {
		_ = json.Unmarshal(cpRaw, cp)
	}
	if box.Secrets == nil {
		box.Secrets = map[string]string{}
	}
	for k, v := range cp.Secrets {
		if _, ok := box.Secrets[k]; !ok {
			box.Secrets[k] = v
		}
	}
	if box.Targets == nil {
		box.Targets = map[string]wire.TargetMeta{}
	}
	for k, v := range cp.Targets {
		if _, ok := box.Targets[k]; !ok {
			box.Targets[k] = v
		}
	}
	// The CP runner's grants are the CONSOLE's (self-granted at deploy) plus
	// any added since; the box package's grants belong to the BOX runner.
	// Preserve the CP's set — overwriting would strip the console's own call.
	if len(cp.Grants) > 0 {
		box.Grants = cp.Grants
	}
	return json.Marshal(box)
}

// DeployCp reproduces the CP deploy driver (OPERATE mode).
func DeployCp(t Transport, spec *DeployCpSpec) (*DeployCpResult, error) {
	if err := bootstrap.PlainPath(spec.StateDir); err != nil {
		return nil, err
	}
	if err := deploy.SafeDeployDir(spec.StateDir); err != nil {
		return nil, err
	}
	if err := bootstrap.PlainPath(spec.BinDir); err != nil {
		return nil, err
	}
	if err := deploy.SafeDeployDir(spec.BinDir); err != nil {
		return nil, err
	}
	// Fail the deploy early instead of shipping a config the box will refuse.
	if len(spec.AdminPubkeys) == 0 {
		if err := ValidateLoopbackBind(spec.BindAddr); err != nil {
			return nil, err
		}
	}

	mkdir := fmt.Sprintf("mkdir -p %s/console && mkdir -p %s && rm -f %s/freehold-console.b64",
		spec.StateDir, spec.BinDir, spec.BinDir)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, mkdir), "mkdir deploy dirs", 30); err != nil {
		return nil, err
	}

	// Stop any PRIOR serve instance before writing over the binary.
	if err := stopPriorServe(t, spec, "stop prior control plane"); err != nil {
		return nil, err
	}
	// Ship the console + agent-tools binaries through the shared path.
	if err := shipConsoleBins(t, spec); err != nil {
		return nil, err
	}

	flags, domain := serveFlags(spec)
	// Pin the relay host into the guest's /etc/hosts. grep -Fq (fixed string):
	// a regex grep would let '.' match '-' and falsely match the guest's own
	// dashed hostname (relay-librem-...-relay), skipping the pin forever.
	if spec.RelayHostIP != nil {
		host := strings.Split(strings.TrimSuffix(domain, "/"), ":")[0]
		hostsCmd := fmt.Sprintf("grep -Fq \"%s\" /etc/hosts 2>/dev/null || echo \"%s %s\" >> /etc/hosts", host, *spec.RelayHostIP, host)
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, hostsCmd), "pin relay host", 30); err != nil {
			return nil, err
		}
	}

	if _, err := startServe(t, spec, flags); err != nil {
		return nil, err
	}

	// Read back the box's console identity pubkey.
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc,
		spec.BinDir+"/freehold-console identity --state-dir "+spec.StateDir), "console identity pubkey", 30)
	if err != nil {
		return nil, err
	}
	pubkey := strings.TrimSpace(out.Stdout)
	if !isHexPubkey(pubkey) {
		return nil, fmt.Errorf("console identity pubkey readback is not 64-hex: %q", pubkey)
	}

	// CO-LOCATED RUNNER (optional).
	if spec.RunnerBinary != nil && spec.RunnerPackage != nil {
		runnerName := filepathBase(*spec.RunnerPackage)
		runnerDir := fmt.Sprintf("%s/runner/%s", spec.StateDir, runnerName)
		// Stop a RUNNING co-located runner BEFORE re-shipping its binary: pct
		// push cannot atomically overwrite the live-executable inode (the old
		// process keeps the old inode, so the shipped-binary size check reads
		// stale bytes and deploy-cp fails on the surviving binary). deploy-cp
		// restarts it below. Tolerated when it isn't running.
		if _, err := execToOK(t,
			proxmox.LxcCmd(spec.LXc, "systemctl stop freehold-runner 2>/dev/null; true"),
			"stop co-located runner", 30); err != nil {
			return nil, err
		}
		if err := shipFile(t, spec, *spec.RunnerBinary,
			spec.BinDir+"/freehold-runner", "runner binary"); err != nil {
			return nil, err
		}
		// ADOPT, never overwrite: when the plane already carries the runner
		// package, THAT identity is the durable one (grants + relay roster bind
		// to it). Ship only the binary + trusted hosts; never the box's
		// identity.json, and never merge the box's secrets.json — its
		// ciphertext is sealed to the BOX runner's encryption key and would be
		// unopenable by the plane runner. The plane's own sealed substrate
		// credential (still on the host door) keeps serving.
		adoptedFromPlane := guestHasRunnerIdentity(t, spec, runnerDir)
		if adoptedFromPlane {
			fmt.Fprintf(os.Stderr, "  co-located runner identity adopted from the plane (%s/identity.json) — box identity + secrets NOT shipped\n", runnerDir)
		}
		shipFiles, mergeSecrets := runnerShipPlan(adoptedFromPlane)
		for _, f := range shipFiles {
			lp := *spec.RunnerPackage + "/" + f
			if _, err := os.Stat(lp); err == nil {
				if err := shipSmallFile(t, spec, lp, runnerDir+"/"+f, "runner "+f); err != nil {
					return nil, err
				}
			}
		}
		// secrets.json MERGES instead of overwriting: the box package carries
		// only its OWN target credential (proxmox-box), but the CP's co-located
		// runner also holds the litellm/postgres-pw/provider-key secrets the
		// build added (freehold-console add-secret). Overwriting wipes them, and
		// the box cannot re-derive them (the CP is the durable owner), so a
		// rebuild would never get them back — the world-build's litellm stage
		// then dies "requested secret \"litellm\" is not in this runner's
		// package". Keep CP-only names; the box wins on overlap (a refreshed SSH
		// credential).
		if mergeSecrets {
			if lp := *spec.RunnerPackage + "/secrets.json"; fileExists(lp) {
				if err := shipMergedRunnerSecrets(t, spec, lp, runnerDir+"/secrets.json"); err != nil {
					return nil, err
				}
			}
		}
		// RE-ADOPT: rotate the co-located runner's substrate SSH key. The plane
		// keeps its runner identity (Nostr/enc + grants); its host credential is
		// regenerated and re-sealed here (identity-preserving), the new line is
		// authorized on the host, and the OLD line is dropped only AFTER the
		// runner restarts successfully — so a failure leaves the host usable.
		// Same-host only.
		var oldSubstratePub, newSubstratePub string
		if adoptedFromPlane {
			var rerr error
			oldSubstratePub, newSubstratePub, rerr = rotateAdoptedSubstrate(t, spec, runnerDir)
			if rerr != nil {
				return nil, rerr
			}
			if newSubstratePub != "" {
				if err := hostAuthorizeKey(t, newSubstratePub); err != nil {
					return nil, err
				}
			}
		}
		startRunner := fmt.Sprintf(
			"systemctl reset-failed freehold-runner 2>/dev/null; systemd-run --unit=freehold-runner --collect %s/freehold-runner serve --state-dir %s >/dev/null 2>&1; sleep 2; systemctl is-active freehold-runner",
			spec.BinDir, runnerDir)
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, startRunner), "start co-located runner", 60); err != nil {
			return nil, err
		}
		// The runner now holds the new key; only now drop the old host line.
		if oldSubstratePub != "" && newSubstratePub != "" {
			if err := hostDeauthorizeKey(t, oldSubstratePub); err != nil {
				return nil, err
			}
		}
		kind, address := "ssh", ""
		if raw, err := os.ReadFile(*spec.RunnerPackage + "/secrets.json"); err == nil {
			k, a := firstTargetKA(string(raw))
			kind, address = k, a
		}
		adopt := fmt.Sprintf("%s/freehold-console adopt %s --kind %s --address %s --package-dir %s --state-dir %s --mcp-addr %s",
			spec.BinDir, runnerName, kind, address, runnerDir, spec.StateDir, config.CoLocatedRunnerMCPAddr)
		if _, err := execTolerantAlreadyExists(t, proxmox.LxcCmd(spec.LXc, adopt), "adopt co-located runner", 60); err != nil {
			return nil, err
		}
		grant := fmt.Sprintf("%s/freehold-console grant %s --state-dir %s --pubkey %s",
			spec.BinDir, runnerName, spec.StateDir, pubkey)
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, grant), "self-grant console to co-located runner", 60); err != nil {
			return nil, err
		}
	}

	// Install stamps the version pin last. Build/teardown/redeploy never do:
	// they operate within the stamp already on the CP.
	if spec.Pin != nil {
		if err := StampPin(t, spec, *spec.Pin); err != nil {
			return nil, err
		}
	}

	isLoopback := ValidateLoopbackBind(spec.BindAddr) == nil
	authnOn := len(spec.AdminPubkeys) > 0
	bindHint := fmt.Sprintf("console loopback %s (reach it via `ssh -L 8080:127.0.0.1:8080 root@<box>`)", spec.BindAddr)
	if !isLoopback {
		authNote := "on"
		if !authnOn {
			authNote = "NOT "
		}
		bindHint = fmt.Sprintf("console on %s (LAN — the operator's proxy/path can reach it; NIP-98 auth %son)", spec.BindAddr, authNote)
	}
	detail := fmt.Sprintf("control plane deployed in OPERATE mode: state %s, %s; relay scope %s (C4: relay authoritative post-port, local state = offline cache mirror); the box's console identity (%s) GENERATED ON THE BOX — add it as a relay member with `freehold add-relay-member --pubkey %s`",
		spec.StateDir, bindHint, spec.RelayURL, pubkey, pubkey)
	return &DeployCpResult{StateDir: spec.StateDir, BindAddr: spec.BindAddr, Pubkey: pubkey, Detail: detail}, nil
}

// execTolerantAlreadyExists runs cmd through the runner and returns nil when
// it succeeds OR the runner adoption reports "already exists" (the CP state
// survived the rebuild — the package + serve were already re-shipped, so
// re-adopting is a no-op). Any other non-zero exit is an error.
func execTolerantAlreadyExists(t Transport, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	out, err := t.Exec(cmd, timeoutS)
	if err != nil {
		return nil, err
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		if !strings.Contains(out.Stdout+out.Stderr, "already exists") {
			return out, fmt.Errorf("%s: exit %d: %s %s", step, *out.ExitCode, out.Stdout, out.Stderr)
		}
	}
	return out, nil
}

// base64StdEncode is std base64 encode.
func base64StdEncode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// filepathBase returns the last path element.
func filepathBase(p string) string {
	segs := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(segs) == 0 || segs[len(segs)-1] == "" {
		return "runner"
	}
	return segs[len(segs)-1]
}

// firstTargetKA extracts the first target's kind + address from a secrets.json
// (for co-located runner adoption).
func firstTargetKA(raw string) (string, string) {
	// Parse secrets.json properly (DEFER-firstTargetKA): read the first
	// target's kind + address via the wire SecretPackage shape instead of
	// hand-scanning the JSON text (fragile to field order).
	var pkg struct {
		Targets map[string]struct {
			Kind    string `json:"kind"`
			Address string `json:"address"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(raw), &pkg); err != nil {
		return "ssh", ""
	}
	for _, t := range pkg.Targets {
		kind := t.Kind
		if kind == "" {
			kind = "ssh"
		}
		return kind, t.Address
	}
	return "ssh", ""
}

// rotateAdoptedSubstrate regenerates the co-located runner's substrate SSH key
// and re-seals it into the plane's package: read the plane runner's identity
// (enc secret), generate a new keypair, seal it (round-trip verified), and ship
// secrets.json back. It does NOT touch the host: the caller authorizes the new
// key, restarts the runner, then drops the old line (so a failure keeps the
// host usable). Returns (oldPub, newPub) — "" newPub means no ssh target.
func rotateAdoptedSubstrate(t Transport, spec *DeployCpSpec, runnerDir string) (string, string, error) {
	idRaw := readGuestFile(t, spec, runnerDir+"/identity.json")
	if idRaw == nil {
		return "", "", fmt.Errorf("rotate substrate: no identity.json at %s", runnerDir)
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return "", "", fmt.Errorf("rotate substrate: parse identity: %w", err)
	}
	encSecret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return "", "", fmt.Errorf("rotate substrate: enc secret: %w", err)
	}
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		return "", "", fmt.Errorf("rotate substrate: enc pubkey: %w", err)
	}

	pkgRaw := readGuestFile(t, spec, runnerDir+"/secrets.json")
	if pkgRaw == nil {
		return "", "", fmt.Errorf("rotate substrate: no secrets.json at %s", runnerDir)
	}
	pkg := &wire.SecretPackage{}
	if err := json.Unmarshal(pkgRaw, pkg); err != nil {
		return "", "", fmt.Errorf("rotate substrate: parse package: %w", err)
	}
	targetName, secretName := "", ""
	for name, meta := range pkg.Targets {
		if meta.Kind == "ssh" {
			targetName, secretName = name, meta.Secret
			break
		}
	}
	if targetName == "" {
		return "", "", fmt.Errorf("rotate substrate: no ssh target in %s/secrets.json", runnerDir)
	}

	// Recover the OLD public line (to remove later) — best-effort.
	oldPub := ""
	if ct, ok := pkg.Secrets[secretName]; ok {
		if sealedOld, derr := hex.DecodeString(ct); derr == nil {
			if pem, oerr := crypto.Open(encSecret, []byte(secretName), sealedOld); oerr == nil {
				if line, lerr := crypto.ExtractED25519PublicKeyLine(pem); lerr == nil {
					oldPub = line
				}
			}
		}
	}

	newPEM, newPub, err := crypto.GenerateSSHKeypair(targetName)
	if err != nil {
		return "", "", fmt.Errorf("rotate substrate: keypair: %w", err)
	}
	sealed, err := crypto.Seal(encPub, []byte(secretName), newPEM)
	if err != nil {
		return "", "", fmt.Errorf("rotate substrate: seal: %w", err)
	}
	// Round-trip self-check: a wrong enc pub / bad seal must fail LOUDLY here,
	// not silently ship a key the runner cannot open.
	if back, oerr := crypto.Open(encSecret, []byte(secretName), sealed); oerr != nil || string(back) != string(newPEM) {
		return "", "", fmt.Errorf("rotate substrate: seal self-check failed — refusing to ship an unopenable credential")
	}
	if pkg.Secrets == nil {
		pkg.Secrets = map[string]string{}
	}
	pkg.Secrets[secretName] = hex.EncodeToString(sealed)
	out, err := pkg.Bytes()
	if err != nil {
		return "", "", fmt.Errorf("rotate substrate: encode package: %w", err)
	}
	tmp, err := os.CreateTemp("", "fh-rotate-*.json")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := shipSmallFile(t, spec, tmp.Name(), runnerDir+"/secrets.json", "runner secrets.json (rotated)"); err != nil {
		return "", "", err
	}
	fmt.Fprintln(os.Stderr, "  rotated the co-located runner substrate key (re-adopt; same-host)")
	return oldPub, newPub, nil
}

// hostAuthorizeKey appends a public line to the HOST's root authorized_keys,
// idempotently (matching on the base64 body).
func hostAuthorizeKey(t Transport, pubLine string) error {
	body := keyBodyOf(pubLine)
	if body == "" {
		return fmt.Errorf("rotate substrate: malformed public line")
	}
	cmd := fmt.Sprintf("mkdir -p /root/.ssh && chmod 700 /root/.ssh && touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && (grep -qF '%s' /root/.ssh/authorized_keys || echo '%s' >> /root/.ssh/authorized_keys)", body, pubLine)
	if _, err := execToOK(t, cmd, "authorize rotated substrate key", 60); err != nil {
		return err
	}
	return nil
}

// hostDeauthorizeKey removes a public line (matched by base64 body) from the
// HOST's root authorized_keys. A mktemp file avoids the predictable-path
// symlink/TOCTOU hazard of a fixed /tmp name.
func hostDeauthorizeKey(t Transport, pubLine string) error {
	body := keyBodyOf(pubLine)
	if body == "" {
		return nil
	}
	cmd := fmt.Sprintf("if [ -f /root/.ssh/authorized_keys ]; then ak=$(mktemp) && grep -vF '%s' /root/.ssh/authorized_keys > $ak && cat $ak > /root/.ssh/authorized_keys && rm -f $ak; fi", body)
	if _, err := execToOK(t, cmd, "deauthorize old substrate key", 60); err != nil {
		return err
	}
	return nil
}

// keyBodyOf returns the base64 body of an authorized_keys line, or "".
func keyBodyOf(line string) string {
	f := strings.Fields(line)
	if len(f) >= 2 && strings.HasPrefix(f[0], "ssh-") {
		return f[1]
	}
	return ""
}
