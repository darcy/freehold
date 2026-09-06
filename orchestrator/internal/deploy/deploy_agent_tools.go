package deploy

import (
	"fmt"
	"os"
	"strings"
	"time"

	"freehold/orchestrator/internal/bootstrap"
	"freehold/orchestrator/internal/client"
)

// DeployAgentToolsSpec wires the freehold-agent-tools MCP server onto the CP:
// the binary ships into the CP's bin dir, it mints a durable identity, its
// own roster channel is seeded with the bootstrap grants, it is granted on the
// CP's co-located runner (so it can apply agent pods), and serve is launched.
type DeployAgentToolsSpec struct {
	LXc                *uint32 // cp vmid
	BinDir             string  // cp bin dir (control-plane + agent-tools live here)
	StateDir           string  // cp control-plane state dir (for `control-plane grant`)
	AgentToolsBinary   string  // local path to the built freehold-agent-tools binary
	AgentToolsStateDir string  // durable server state dir on the cp (e.g. /srv/data/cp/agent-tools)
	BindAddr           string  // serve bind (0.0.0.0:8089)
	RelayURL           string
	RelayPubkey        string
	RelayWS            string
	RelayLxc           *uint32 // relay vmid
	RelayCompose       string
	K3sVmid            uint32
	RunnerAddr         string
	RunnerPubkey       string
	RunnerTarget       string
	RunnerName         string // co-located runner grant name (e.g. proxmox-box)
	CpaName            string
	OwnerPubkey        string
	LiteLLMBase        string
	GrantsCSV          string // bootstrap member pubkeys ("pk1,pk2")
}

// DeployAgentToolsResult is the agent-tools deploy outcome.
type DeployAgentToolsResult struct {
	Pubkey  string // the server's own Nostr pubkey (its MCP audience)
	BindAddr string
	StateDir string
}

// execOut runs cmd through the runner and returns its stdout.
func execOut(clientConn *client.McpClient, target, cmd, step string, timeoutS uint64) (string, error) {
	out, err := bootstrap.ExecToOK(clientConn, target, cmd, step, timeoutS)
	if err != nil {
		return "", err
	}
	return out.Stdout, nil
}

// DeployAgentTools ships + seeds + launches the CP's freehold-agent-tools
// server. Caller supplies an McpClient to the runner that reaches the box.
func DeployAgentTools(clientConn *client.McpClient, target string, spec *DeployAgentToolsSpec) (*DeployAgentToolsResult, error) {
	if spec.LXc == nil || spec.BinDir == "" || spec.AgentToolsBinary == "" {
		return nil, fmt.Errorf("agent-tools deploy needs an lxc, bin dir, and binary")
	}
	if _, err := os.Stat(spec.AgentToolsBinary); err != nil {
		return nil, fmt.Errorf("agent-tools binary not built: %w", err)
	}
	agentToolsBin := spec.BinDir + "/freehold-agent-tools"

	// Ensure the durable state dir exists inside the cp guest.
	mkdir := fmt.Sprintf("mkdir -p %s && chmod 700 %s", spec.AgentToolsStateDir, spec.AgentToolsStateDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, mkdir), "mkdir agent-tools state", 30); err != nil {
		return nil, err
	}
	// Stop a PRIOR serve instance before writing over the binary.
	stop := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true", spec.AgentToolsStateDir, spec.AgentToolsStateDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, stop), "stop prior agent-tools", 30); err != nil {
		return nil, err
	}

	// Ship the binary.
	if err := shipFile(clientConn, target, &DeployCpSpec{LXc: spec.LXc, BinDir: spec.BinDir},
		spec.AgentToolsBinary, agentToolsBin, "agent-tools binary"); err != nil {
		return nil, err
	}

	// Mint the server's durable identity (reused across rebuilds).
	pkOut, err := execOut(clientConn, target, lxcCmd(spec.LXc,
		agentToolsBin+" identity --state-dir "+spec.AgentToolsStateDir), "agent-tools identity", 30)
	if err != nil {
		return nil, err
	}
	pubkey := strings.TrimSpace(pkOut)
	if !isHexPubkey(pubkey) {
		return nil, fmt.Errorf("agent-tools identity readback not 64-hex: %q", pubkey)
	}

	// Relay membership (relay-administered; the CP cannot self-add): run
	// buzz-admin through the runner into the relay LXC.
	if spec.RelayLxc != nil {
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", spec.RelayCompose, pubkey)
		full := fmt.Sprintf("pct exec %d -- sh -c '%s'", *spec.RelayLxc, cmdLine)
		if _, err := bootstrap.ExecToOK(clientConn, target, full, "relay member agent-tools", 120); err != nil {
			return nil, fmt.Errorf("add agent-tools as relay member: %w", err)
		}
	}

	// Seed the server's own channel + member the bootstrap grants into its
	// roster (the one-time seed; the roster is then the durable source).
	seed := fmt.Sprintf("%s seed --state-dir %s --relay-url %s --granted %s --name agent-tools",
		agentToolsBin, spec.AgentToolsStateDir, spec.RelayURL, spec.GrantsCSV)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, seed), "seed agent-tools roster", 60); err != nil {
		return nil, fmt.Errorf("seed agent-tools roster: %w", err)
	}

	// Grant the server on the co-located runner so it can apply agent pods.
	if spec.RunnerName != "" {
		grant := fmt.Sprintf("%s/control-plane grant --state-dir %s --pubkey %s %s",
			spec.BinDir, spec.StateDir, pubkey, spec.RunnerName)
		if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, grant), "grant agent-tools on runner", 60); err != nil {
			return nil, fmt.Errorf("grant agent-tools on the co-located runner: %w", err)
		}
	}

	// Launch serve.
	serveFlags := fmt.Sprintf(
		"--state-dir %s --addr %s --relay-url %s --relay-pubkey %s --relay-lxc %d --relay-compose %s --k3s-vmid %d --runner-addr %s --runner-pubkey %s --runner-target %s --cpa-name %s --owner-pubkey %s",
		spec.AgentToolsStateDir, spec.BindAddr, spec.RelayURL, spec.RelayPubkey, deref(spec.RelayLxc), spec.RelayCompose,
		spec.K3sVmid, spec.RunnerAddr, spec.RunnerPubkey, spec.RunnerTarget, spec.CpaName, spec.OwnerPubkey)
	if spec.LiteLLMBase != "" {
		serveFlags += " --litellm-base " + spec.LiteLLMBase
	}
	if spec.RelayURL != "" && spec.RelayWS != "" {
		serveFlags += " --relay-ws " + spec.RelayWS
	}
	start := fmt.Sprintf(
		"setsid nohup %s serve %s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid",
		agentToolsBin, serveFlags, spec.AgentToolsStateDir, spec.AgentToolsStateDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, start), "start agent-tools", 30); err != nil {
		return nil, err
	}

	// Poll until the serve endpoint answers (any HTTP status proves the
	// listener is up; "000" means not yet bound).
	up := false
	for i := 0; i < 15; i++ {
		probe := fmt.Sprintf("curl -s -m 3 -o /dev/null -w %%{http_code} http://%s/mcp", loopbackOf(spec.BindAddr))
		code, err := execOut(clientConn, target, lxcCmd(spec.LXc, probe), "agent-tools up", 15)
		if err == nil && strings.TrimSpace(code) != "000" {
			up = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !up {
		return nil, fmt.Errorf("agent-tools serve did not answer on %s within the poll window — check %s/serve.log", spec.BindAddr, spec.AgentToolsStateDir)
	}
	return &DeployAgentToolsResult{Pubkey: pubkey, BindAddr: spec.BindAddr, StateDir: spec.AgentToolsStateDir}, nil
}

// loopbackOf rewrites a 0.0.0.0 bind to 127.0.0.1 for an in-guest probe.
func loopbackOf(bind string) string {
	if strings.HasPrefix(bind, "0.0.0.0:") || strings.HasPrefix(bind, "[::]:") || strings.HasPrefix(bind, ":") {
		return "127.0.0.1" + bind[strings.Index(bind, ":"):]
	}
	return bind
}

func deref(u *uint32) uint32 {
	if u == nil {
		return 0
	}
	return *u
}
