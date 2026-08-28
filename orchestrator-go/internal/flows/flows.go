// Package flows is the scripted flows (E1/E2) port — orchestrator/src/
// flows.rs: enabling an existing service (onboard), connecting to a running
// runner, reading readiness, running scripted demo steps.
//
// The one behavioral change: `onboard` no longer serves the runner
// IN-PROCESS. It execs the SHIPPED Rust runner binary as a subprocess
// (`runner serve --state-dir <package_dir> --addr 127.0.0.1:<port>`), connects
// over HTTP MCP to validate readiness, then terminates the subprocess (live
// serving is the operator's job, as the Rust onboard already left it).
package flows

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"freehold/orchestrator-go/internal/crypto"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"freehold/orchestrator-go/internal/client"
	"freehold/orchestrator-go/internal/provisioner"
	"freehold/orchestrator-go/internal/state"
)

const identityFile = "identity.json"

// Identity is the runner's JSON identity (nostr + enc secrets).
type Identity struct {
	NostrSecretHex string `json:"nostr_secret_hex"`
	EncSecretHex   string `json:"enc_secret_hex"`
}

// LoadIdentity reads identity.json in dir.
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// NostrPubkeyHex derives the Nostr x-only pubkey from the identity secret.
func (id *Identity) NostrPubkeyHex() (string, error) {
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return "", err
	}
	return pubkeyFromSecret(secret)
}

// AgentAuth loads the agent identity (identity.json in dir) and its signing
// auth — reproduces flows::agent_auth.
func AgentAuth(dir string) (*client.AgentAuth, error) {
	id, err := LoadIdentity(dir)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	pubkey, err := pubkeyFromSecret(secret)
	if err != nil {
		return nil, err
	}
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], secret)
	auth.Pubkey = pubkey
	return auth, nil
}

// ConnectURL builds the MCP endpoint from a --addr (both forms).
func ConnectURL(addr string) string { return client.ConnectURL(addr) }

// Connect returns an McpClient to a running runner at addr.
func Connect(addr, agentDir, runnerPubkey string) (*client.McpClient, error) {
	auth, err := AgentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	return client.New(ConnectURL(addr), auth, runnerPubkey)
}

// OnboardReport mirrors flows::OnboardReport.
type OnboardReport struct {
	Name         string                 `json:"name"`
	NostrPubkey  string                 `json:"nostr_pubkey"`
	EncPubkey    string                 `json:"enc_pubkey"`
	RunnerPubkey string                 `json:"runner_pubkey"`
	AgentPubkey  string                 `json:"agent_pubkey"`
	Readiness    map[string]interface{} `json:"readiness"`
	PackageDir   string                 `json:"package_dir"`
}

// Onboard is E1: provision, then validate readiness through a subprocess exec
// of the shipped Rust runner, then stop the subprocess.
func Onboard(name, kind, address string, secret []byte, agentDir, cpStateDir, runnerDir, runnerBinary string) (*OnboardReport, error) {
	agent, err := AgentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	defer agent.Zero()

	store, err := state.Open(cpStateDir)
	if err != nil {
		return nil, err
	}
	provisioned, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: name, Kind: kind, Address: address, Secret: secret,
		RunnerDir: runnerDir, Grants: []string{agent.Pubkey},
	})
	if err != nil {
		return nil, err
	}
	defer wipe(secret)

	// Load the runner identity for its pubkey.
	rid, err := LoadIdentity(runnerDir)
	if err != nil {
		return nil, err
	}
	runnerPubkey, err := rid.NostrPubkeyHex()
	if err != nil {
		return nil, err
	}

	// Exec the shipped Rust runner as a subprocess on an ephemeral port.
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	bin := runnerBinary
	if bin == "" {
		bin = "freehold-runner"
	}
	cmd := exec.Command(bin, "serve", "--state-dir", runnerDir, "--addr", addr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runner serve subprocess: %w", err)
	}
	// Always reap the subprocess on exit.
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	// Wait for the MCP endpoint to come up (bounded).
	clientConn, err := client.New("http://"+addr+"/mcp", agent, runnerPubkey)
	if err != nil {
		return nil, err
	}
	var readiness map[string]interface{}
	deadline := time.Now().Add(15 * time.Second)
	for {
		readiness, err = clientConn.Readiness()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("runner readiness timeout: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Hard failure: local self-check must be green.
	if s, ok := readiness["local"].(string); !ok || s != "green" {
		return nil, fmt.Errorf("local self-check not green after onboarding: %v", readiness)
	}

	return &OnboardReport{
		Name:         name,
		NostrPubkey:  provisioned.NostrPubkey,
		EncPubkey:    provisioned.EncPubkey,
		RunnerPubkey: runnerPubkey,
		AgentPubkey:  agent.Pubkey,
		Readiness:    readiness,
		PackageDir:   provisioned.PackageDir,
	}, nil
}

// Readiness reads the runner's readiness table.
func Readiness(addr, agentDir, runnerPubkey string) (map[string]interface{}, error) {
	clientConn, err := Connect(addr, agentDir, runnerPubkey)
	if err != nil {
		return nil, err
	}
	return clientConn.Readiness()
}

// Exec runs one command against a running runner.
func Exec(addr, agentDir, runnerPubkey, target, cmd string, secrets []string, timeoutS uint64) (*client.ExecOutcome, error) {
	clientConn, err := Connect(addr, agentDir, runnerPubkey)
	if err != nil {
		return nil, err
	}
	return clientConn.Exec(target, cmd, secrets, timeoutS)
}

// DemoStep mirrors flows::DemoStep.
type DemoStep struct {
	Target   string   `json:"target"`
	Cmd      string   `json:"cmd"`
	Secrets  []string `json:"secrets,omitempty"`
	TimeoutS uint64   `json:"timeout_s,omitempty"`
}

// StepResult mirrors flows::StepResult.
type StepResult struct {
	Index      int     `json:"index"`
	Target     string  `json:"target"`
	OK         bool    `json:"ok"`
	TimedOut   bool    `json:"timed_out"`
	ExitCode   *int    `json:"exit_code"`
	StdoutHead string  `json:"stdout_head"`
	StderrHead string  `json:"stderr_head"`
	Error      *string `json:"error"`
}

// RunDemo runs scripted exec steps against the runner, reporting each.
func RunDemo(clientConn *client.McpClient, steps []DemoStep) ([]StepResult, error) {
	results := make([]StepResult, 0, len(steps))
	for idx, step := range steps {
		timeout := step.TimeoutS
		if timeout == 0 {
			timeout = 60
		}
		outcome, err := clientConn.Exec(step.Target, step.Cmd, step.Secrets, timeout)
		if err != nil {
			errStr := err.Error()
			results = append(results, StepResult{
				Index: idx, Target: step.Target, OK: false, Error: &errStr,
			})
			continue
		}
		ok := !outcome.TimedOut && outcome.ExitCode != nil && *outcome.ExitCode == 0
		results = append(results, StepResult{
			Index:      idx,
			Target:     step.Target,
			OK:         ok,
			TimedOut:   outcome.TimedOut,
			ExitCode:   outcome.ExitCode,
			StdoutHead: head(outcome.Stdout, 160),
			StderrHead: head(outcome.Stderr, 80),
		})
	}
	return results, nil
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// freePort asks the OS for an ephemeral free port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// pubkeyFromSecret derives the Nostr x-only pubkey hex from a secret.
func pubkeyFromSecret(secret []byte) (string, error) {
	return crypto.PubkeyFromSecret(secret)
}
