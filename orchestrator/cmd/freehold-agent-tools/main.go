// Command freehold-agent-tools serves the CP-side MCP server exposing
// create_agent / grant_agent / manage_agent to agents. It is the dedicated
// privileged Go binary on the CP whose tool handlers call internal/agent/tools.go
// in-process — a distinct semantic surface from the runner's generic exec.
//
// Callers authenticate with the shared signed-header scheme (core/src/auth.rs);
// the grants list is seeded at bootstrap with the operator pubkey so the build
// can dogfood-agent-create the CPA. The deploy path executes through a runner
// (exec middleman) — CreateAgentFn runs the create-agent deploy script as a
// signed exec on the configured runner target.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/agenttools"
	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/crypto"
)

func main() {
	var (
		addr       = flag.String("addr", "127.0.0.1:8088", "MCP bind address")
		consoleURL = flag.String("console-url", os.Getenv("FREEHOLD_AGENT_TOOLS_CONSOLE_URL"), "CP console NIP-98 base URL")
		secretHex  = flag.String("secret", os.Getenv("FREEHOLD_AGENT_TOOLS_SECRET"), "this server's Nostr secret hex (audience + console/runner identity)")
		runnerAddr = flag.String("runner-addr", os.Getenv("FREEHOLD_AGENT_TOOLS_RUNNER_ADDR"), "MCP addr of the runner freehold-agent-tools execs deploys through")
		runnerPK   = flag.String("runner-pubkey", os.Getenv("FREEHOLD_AGENT_TOOLS_RUNNER_PUBKEY"), "runner pubkey (signature audience for the deploy exec)")
		runnerTgt  = flag.String("runner-target", os.Getenv("FREEHOLD_AGENT_TOOLS_RUNNER_TARGET"), "runner target to exec the create-agent deploy on")
		createCmd  = flag.String("create-cmd", os.Getenv("FREEHOLD_AGENT_TOOLS_CREATE_CMD"), "create-agent deploy: an exec command template with {name} and {purpose} placeholders (run on the runner target)")
		grantsCSV  = flag.String("grants", os.Getenv(agenttools.GrantsEnv), "comma-separated granted caller pubkeys")
	)
	flag.Parse()

	audience := os.Getenv(agenttools.AudienceEnv)
	if *secretHex != "" {
		secret, err := hex.DecodeString(*secretHex)
		if err != nil || len(secret) != 32 {
			log.Fatalf("secret must be 32-byte hex: %v", err)
		}
		audience, err = crypto.PubkeyFromSecret(secret)
		if err != nil {
			log.Fatal(err)
		}
	}

	tools := &agent.Tools{}
	if *consoleURL != "" && *secretHex != "" {
		secret, _ := hex.DecodeString(*secretHex)
		c, err := console.Login(*consoleURL, secret, 15*time.Second)
		if err != nil {
			log.Fatalf("console login: %v", err)
		}
		tools.Console = c
	}
	// CreateAgentFn: exec the create-agent deploy through the configured runner.
	tools.Create = func(name, purpose string) (string, error) {
		if *runnerAddr == "" || *runnerPK == "" || *runnerTgt == "" || *createCmd == "" {
			return "", fmt.Errorf("freehold-agent-tools create-agent: deploy backend not wired (no runner-addr/pubkey/target/create-cmd)")
		}
		secret, _ := hex.DecodeString(*secretHex)
		auth := &client.AgentAuth{Secret: [32]byte{}}
		copy(auth.Secret[:], secret)
		pk, _ := crypto.PubkeyFromSecret(secret)
		auth.Pubkey = pk
		mc, err := client.New(client.ConnectURL(*runnerAddr), auth, *runnerPK)
		if err != nil {
			return "", err
		}
		cmd := strings.ReplaceAll(*createCmd, "{name}", shellEscape(name))
		cmd = strings.ReplaceAll(cmd, "{purpose}", shellEscape(purpose))
		out, err := mc.Exec(*runnerTgt, cmd, []string{*runnerTgt}, 300)
		if err != nil {
			return "", err
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			return "", fmt.Errorf("create-agent exec failed (%d): %s %s", *out.ExitCode, out.Stdout, out.Stderr)
		}
		// The deploy script echoes the new pubkey on its last line.
		pub := strings.TrimSpace(out.Stdout)
		if p := lastNonEmptyLine(pub); p != "" {
			pub = p
		}
		return pub, nil
	}

	srv := &agenttools.Server{Audience: audience, Grants: agenttools.ParseGrants(*grantsCSV), Tools: tools}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.ServeHTTP)
	log.Printf("freehold-agent-tools serving on %s (audience %s, grants=%d)", *addr, audience, len(srv.Grants))
	s := &http.Server{Addr: *addr, Handler: mux}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	_ = context.Background
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return s
}
