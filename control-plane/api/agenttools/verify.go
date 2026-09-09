// Package agenttools hosts freehold-agent-tools: the CP-side MCP server that
// exposes create_agent / grant_agent / manage_agent to agents. This is a
// DISTINCT semantic MCP surface from the runner's generic exec funnel — the
// handlers call internal/agent/tools.go in-process ("MCP server, not a
// proxy"), never exec, never an HTTP API hop.
//
// Callers authenticate with the SAME shared signed-header scheme the runner
// uses (core/src/auth.rs): x-freehold-pubkey/sig/ts over
// `audience|ts|raw_body`, BIP-340, granted-pubkey check, 60s window. The
// audience is this server's own pubkey (the CP's agent-tools identity), and
// the grants list is seeded at bootstrap with the operator pubkey so a build
// can dogfood-agent-create the CPA.
package agenttools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"freehold/contract/crypto"
)

// Auth headers mirror the runner (client/mcp.go).
const (
	TSWindowSecs  = 60
	PubkeyHeader  = "x-freehold-pubkey"
	SigHeader     = "x-freehold-sig"
	TSHeader      = "x-freehold-ts"
	DefaultCPName = "freehold"
	AudienceEnv   = "FREEHOLD_AGENT_TOOLS_PUBKEY"
	GrantsEnv     = "FREEHOLD_AGENT_TOOLS_GRANTS" // comma-separated granted pubkeys
)

// VerifyRequest authorizes a MCP tools/call the same way the runner does:
// pubkey must be granted, signature must verify over
// `audience|ts|raw_body`, timestamp within TSWindowSecs. Returns the caller
// pubkey on success. Fail closed on any missing/ill-formed input.
func VerifyRequest(grants []string, audience, pubkey, sig, tsStr, rawBody string) (string, error) {
	if pubkey == "" || sig == "" || tsStr == "" {
		return "", fmt.Errorf("missing auth headers (%s/%s/%s)", PubkeyHeader, SigHeader, TSHeader)
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad timestamp: %s", tsStr)
	}
	now := time.Now().Unix()
	if now-ts > TSWindowSecs || ts-now > TSWindowSecs {
		return "", fmt.Errorf("request stale (timestamp outside the %ds window)", TSWindowSecs)
	}
	granted := false
	for _, g := range grants {
		if g == pubkey {
			granted = true
			break
		}
	}
	if !granted {
		return "", fmt.Errorf("pubkey %s is not granted to call freehold-agent-tools", pubkey)
	}
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return "", fmt.Errorf("signature not valid hex")
	}
	canonical := audience + "|" + strconv.FormatInt(ts, 10) + "|" + rawBody
	digest := sha256.Sum256([]byte(canonical))
	if err := crypto.VerifyBIP340(pubkey, digest[:], sigBytes); err != nil {
		return "", fmt.Errorf("signature does not verify for this request")
	}
	return pubkey, nil
}

// ParseGrants splits a comma-separated grant list (env-shaped), trimming.
func ParseGrants(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}
