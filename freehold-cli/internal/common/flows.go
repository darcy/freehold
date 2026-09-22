package common

import (
	"freehold/contract/client"
	"freehold/contract/identity"
)

// ConnectURL builds the MCP endpoint from a --addr (both forms).
func ConnectURL(addr string) string { return client.ConnectURL(addr) }

// ConnectDirect returns an McpClient to a running runner at addr with an
// explicit agent dir + runner pubkey (the runner-direct path the teardown
// engine and uninstall use).
func ConnectDirect(addr, agentDir, runnerPubkey string) (*client.McpClient, error) {
	auth, err := identity.AgentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	return client.New(ConnectURL(addr), auth, runnerPubkey)
}

// ExecDirect runs one command against a running runner.
func ExecDirect(addr, agentDir, runnerPubkey, target, cmd string, secrets []string, timeoutS uint64) (*client.ExecOutcome, error) {
	c, err := ConnectDirect(addr, agentDir, runnerPubkey)
	if err != nil {
		return nil, err
	}
	return c.Exec(target, cmd, secrets, timeoutS)
}
