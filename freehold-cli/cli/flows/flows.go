// Package flows holds the local operator helpers for driving a RUNNING runner:
// connecting over MCP, reading readiness, running scripted demo steps, and one
// exec. (The runner/agent identity loader lives in freehold/contract/identity;
// the CP-owned agent-org reconcile lives server-side in cpbuild.)
package flows

import (
	"freehold/contract/client"
	"freehold/contract/identity"
)

// ConnectURL builds the MCP endpoint from a --addr (both forms).
func ConnectURL(addr string) string { return client.ConnectURL(addr) }

// Connect returns an McpClient to a running runner at addr.
func Connect(addr, agentDir, runnerPubkey string) (*client.McpClient, error) {
	auth, err := identity.AgentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	return client.New(ConnectURL(addr), auth, runnerPubkey)
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

// DemoStep is one scripted exec step.
type DemoStep struct {
	Target   string   `json:"target"`
	Cmd      string   `json:"cmd"`
	Secrets  []string `json:"secrets,omitempty"`
	TimeoutS uint64   `json:"timeout_s,omitempty"`
}

// StepResult is one step's outcome.
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
