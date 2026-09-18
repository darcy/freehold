package cpdeploy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/wire"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/deploy"
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
}

// DeployCpResult is the CP deploy outcome.
type DeployCpResult struct {
	StateDir string
	BindAddr string
	Pubkey   string
	Detail   string
}

// shipFile streams a LOCAL file to the target via the runner's sftp upload,
// then moves it into place (pct push inside an LXC, mv on a bare host).
func shipFile(clientConn *client.McpClient, target string, spec *DeployCpSpec, localPath, remoteFinal, step string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	localSize := info.Size()
	hostTmp := fmt.Sprintf("/tmp/freehold-ship-%d", os.Getpid())
	_, _ = bootstrap.ExecToOK(clientConn, target, "rm -f "+hostTmp, "reset host tmp "+step, 30)
	remoteSize, err := clientConn.Upload(target, localPath, hostTmp, 300)
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
	if _, err := bootstrap.ExecToOK(clientConn, target, place, "place "+step, 120); err != nil {
		return err
	}
	if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, "chmod 755 "+remoteFinal), "chmod "+step, 30); err != nil {
		return err
	}
	out, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, "wc -c < "+remoteFinal), "verify "+step, 30)
	if err != nil {
		return err
	}
	guestSizeStr := strings.TrimSpace(out.Stdout)
	var guestSize uint64
	if _, err := fmt.Sscanf(guestSizeStr, "%d", &guestSize); err != nil || guestSize != uint64(localSize) {
		return fmt.Errorf("shipped %s guest size mismatch: %s vs local %d", step, guestSizeStr, localSize)
	}
	_, _ = bootstrap.ExecToOK(clientConn, target, "rm -f "+hostTmp, "clean "+step, 30)
	return nil
}

// shipSmallFile ships a SMALL file in one exec (base64).
func shipSmallFile(clientConn *client.McpClient, target string, spec *DeployCpSpec, localPath, remoteFinal, step string) error {
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
	_, err = bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, cmd), step, 60)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readGuestFile returns a file's bytes from inside the LXC, or nil when absent
// (the path is trusted: built from a fixed runner dir, no shell metacharacters).
func readGuestFile(clientConn *client.McpClient, target string, spec *DeployCpSpec, path string) []byte {
	out, err := bootstrap.ExecToOK(clientConn, target,
		deploy.LxcCmd(spec.LXc, "cat "+path+" 2>/dev/null || true"), "read "+path, 30)
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
func guestHasRunnerIdentity(clientConn *client.McpClient, target string, spec *DeployCpSpec, runnerDir string) bool {
	out, err := bootstrap.ExecToOK(clientConn, target,
		deploy.LxcCmd(spec.LXc, "test -f "+runnerDir+"/identity.json && echo yes"), "probe runner identity", 30)
	return err == nil && strings.TrimSpace(out.Stdout) == "yes"
}

// shipMergedRunnerSecrets ships the box package's secrets.json into the CP
// runner package, preserving secret NAMES already present on the CP that the
// box package doesn't carry (the build-added litellm trio); the box wins on a
// name overlap.
func shipMergedRunnerSecrets(clientConn *client.McpClient, target string, spec *DeployCpSpec, localPath, remoteFinal string) error {
	boxRaw, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	merged, err := mergeRunnerSecrets(boxRaw, readGuestFile(clientConn, target, spec, remoteFinal))
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
	return shipSmallFile(clientConn, target, spec, tmp.Name(), remoteFinal, "runner secrets.json")
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
func DeployCp(clientConn *client.McpClient, target string, spec *DeployCpSpec) (*DeployCpResult, error) {
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
	if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, mkdir), "mkdir deploy dirs", 30); err != nil {
		return nil, err
	}

	// Stop any PRIOR serve instance before writing over the binary.
	stop := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true",
		spec.StateDir, spec.StateDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, stop), "stop prior control plane", 30); err != nil {
		return nil, err
	}

	// Ship the Go console binary (the CP CLI + web server).
	if err := shipFile(clientConn, target, spec, spec.BinaryPath,
		spec.BinDir+"/freehold-console", "console binary"); err != nil {
		return nil, err
	}
	// Ship the agent-tools binary too, so the console's world_build (cpbuild
	// deployAgentTools) can bring up the operator toolset itself in the guest.
	if spec.AgentToolsBinary != nil && *spec.AgentToolsBinary != "" {
		// A RUNNING agent-tools serve (the world brought it up) holds the
		// binary — stop it first or the overwrite fails/size-mismatches.
		atState := filepath.Join(spec.StateDir, "..", "agent-tools")
		stop := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true", atState, atState)
		if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, stop), "stop prior agent-tools", 30); err != nil {
			return nil, err
		}
		if err := shipFile(clientConn, target, spec, *spec.AgentToolsBinary,
			spec.BinDir+"/freehold-agent-tools", "agent-tools binary"); err != nil {
			return nil, err
		}
	}

	adminFlag := ""
	if len(spec.AdminPubkeys) > 0 {
		adminFlag = " --admin-pubkeys " + strings.Join(spec.AdminPubkeys, ",")
	}
	originFlag := ""
	if spec.PublicOrigin != nil {
		originFlag = " --public-origin " + *spec.PublicOrigin
	}
	domain := strings.TrimPrefix(strings.TrimPrefix(spec.RelayURL, "https://"), "http://")
	domain = strings.TrimSuffix(domain, "/")
	relayPK := spec.RelayPubkey
	scopeURL := spec.RelayURL
	if spec.RelayHostIP != nil {
		scopeURL = fmt.Sprintf("http://%s:3000", *spec.RelayHostIP)
	}
	relayFlag := ""
	if relayPK != nil && *relayPK != "" {
		relayFlag = fmt.Sprintf(" --relay-url %s --relay-pubkey %s --relay-host %s", scopeURL, *relayPK, domain)
	}
	atFlag := ""
	if spec.AgentToolsURL != nil && *spec.AgentToolsURL != "" {
		atFlag = fmt.Sprintf(" --agent-tools-url %s", *spec.AgentToolsURL)
	}
	if spec.AgentToolsPubkey != nil && *spec.AgentToolsPubkey != "" {
		atFlag += fmt.Sprintf(" --agent-tools-pubkey %s", *spec.AgentToolsPubkey)
	}
	// The world config bounds the console as the CP build executor. Base64 so
	// the JSON (quotes/spaces) survives single-arg embedding; the console serve
	// decodes it.
	worldFlag := ""
	if spec.WorldConfig != nil && *spec.WorldConfig != "" {
		worldFlag = " --world-config " + base64.StdEncoding.EncodeToString([]byte(*spec.WorldConfig))
	}
	// Pin the relay host into the guest's /etc/hosts. grep -Fq (fixed string):
	// a regex grep would let '.' match '-' and falsely match the guest's own
	// dashed hostname (relay-librem-...-relay), skipping the pin forever.
	if spec.RelayHostIP != nil {
		host := strings.TrimSuffix(domain, "/")
		host = strings.Split(host, ":")[0]
		hostsCmd := fmt.Sprintf("grep -Fq '%s' /etc/hosts 2>/dev/null || echo '%s %s' >> /etc/hosts", host, *spec.RelayHostIP, host)
		if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, hostsCmd), "pin relay host", 30); err != nil {
			return nil, err
		}
	}

	start := fmt.Sprintf(
		"setsid nohup %s/freehold-console serve --state-dir %s --addr %s%s%s%s%s%s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid",
		spec.BinDir, spec.StateDir, spec.BindAddr, adminFlag, originFlag, relayFlag, atFlag, worldFlag, spec.StateDir, spec.StateDir)
	out, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, start), "start control plane", 30)
	if err != nil {
		return nil, err
	}
	pidStr := strings.TrimSpace(out.Stdout)
	var pid uint32
	if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == 0 {
		return nil, fmt.Errorf("start did not yield a pid: %q", out.Stdout)
	}

	// Poll /healthz, then kill -0 the pid.
	healthy := false
	var lastErr error
	for i := 0; i < 15; i++ {
		probe := fmt.Sprintf("curl -fsS -m 3 http://%s/healthz", spec.BindAddr)
		_, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, probe), "cp healthz", 20)
		if err == nil {
			healthy = true
			break
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if !healthy {
		return nil, fmt.Errorf("control plane did not answer /healthz on %s within the poll window (last probe error: %v)",
			spec.BindAddr, lastErr)
	}
	alive := fmt.Sprintf("kill -0 %d >/dev/null 2>&1", pid)
	if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, alive), "control plane still alive", 10); err != nil {
		return nil, fmt.Errorf("control plane answered /healthz but the started process (pid %d) is gone — check %s/serve.log (e.g. address already in use)",
			pid, spec.StateDir)
	}

	// Read back the box's console identity pubkey.
	out, err = bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc,
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
		if _, err := bootstrap.ExecToOK(clientConn, target,
			deploy.LxcCmd(spec.LXc, "systemctl stop freehold-runner 2>/dev/null; true"),
			"stop co-located runner", 30); err != nil {
			return nil, err
		}
		if err := shipFile(clientConn, target, spec, *spec.RunnerBinary,
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
		adoptedFromPlane := guestHasRunnerIdentity(clientConn, target, spec, runnerDir)
		if adoptedFromPlane {
			fmt.Fprintf(os.Stderr, "  co-located runner identity adopted from the plane (%s/identity.json) — box identity + secrets NOT shipped\n", runnerDir)
		}
		shipFiles, mergeSecrets := runnerShipPlan(adoptedFromPlane)
		for _, f := range shipFiles {
			lp := *spec.RunnerPackage + "/" + f
			if _, err := os.Stat(lp); err == nil {
				if err := shipSmallFile(clientConn, target, spec, lp, runnerDir+"/"+f, "runner "+f); err != nil {
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
				if err := shipMergedRunnerSecrets(clientConn, target, spec, lp, runnerDir+"/secrets.json"); err != nil {
					return nil, err
				}
			}
		}
		startRunner := fmt.Sprintf(
			"systemctl reset-failed freehold-runner 2>/dev/null; systemd-run --unit=freehold-runner --collect %s/freehold-runner serve --state-dir %s >/dev/null 2>&1; sleep 2; systemctl is-active freehold-runner",
			spec.BinDir, runnerDir)
		if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, startRunner), "start co-located runner", 60); err != nil {
			return nil, err
		}
		kind, address := "ssh", ""
		if raw, err := os.ReadFile(*spec.RunnerPackage + "/secrets.json"); err == nil {
			k, a := firstTargetKA(string(raw))
			kind, address = k, a
		}
		adopt := fmt.Sprintf("%s/freehold-console adopt %s --kind %s --address %s --package-dir %s --state-dir %s --mcp-addr %s",
			spec.BinDir, runnerName, kind, address, runnerDir, spec.StateDir, config.CoLocatedRunnerMCPAddr)
		if _, err := execTolerantAlreadyExists(clientConn, target, deploy.LxcCmd(spec.LXc, adopt), "adopt co-located runner", 60); err != nil {
			return nil, err
		}
		grant := fmt.Sprintf("%s/freehold-console grant %s --state-dir %s --pubkey %s",
			spec.BinDir, runnerName, spec.StateDir, pubkey)
		if _, err := bootstrap.ExecToOK(clientConn, target, deploy.LxcCmd(spec.LXc, grant), "self-grant console to co-located runner", 60); err != nil {
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
	detail := fmt.Sprintf("control plane deployed in OPERATE mode: state %s, %s; relay scope %s (C4: relay authoritative post-port, local state = offline cache mirror); the box's console identity (%s) GENERATED ON THE BOX — add it as a relay member with `freehold relay-member --pubkey %s`",
		spec.StateDir, bindHint, spec.RelayURL, pubkey, pubkey)
	return &DeployCpResult{StateDir: spec.StateDir, BindAddr: spec.BindAddr, Pubkey: pubkey, Detail: detail}, nil
}

// execTolerantAlreadyExists runs cmd through the runner and returns nil when
// it succeeds OR the runner adoption reports "already exists" (the CP state
// survived the rebuild — the package + serve were already re-shipped, so
// re-adopting is a no-op). Any other non-zero exit is an error.
func execTolerantAlreadyExists(clientConn *client.McpClient, target, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	out, err := bootstrap.Exec(clientConn, target, cmd, timeoutS)
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
