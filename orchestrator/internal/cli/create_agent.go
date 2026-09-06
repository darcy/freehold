package cli

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/delegate"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/state"
	"freehold/orchestrator/prompts"
)

// agentDirForName returns the durable identity dir for a created agent (under
// the CP-owned control-plane area so it survives compute-only teardown).
func agentDirForName(name string) string {
	return filepath.Join(rbStateDir(), "agent-"+sanitizeDir(name))
}

func sanitizeDir(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// stageCreateAgent deploys a new conversational agent (Chunk 4 Phase E E1):
// mint a durable identity, add it as a relay member, seat it in its own
// channel + publish its profile, apply its pod (via the runner into the k3s
// node), and register it in the CP agent registry. The created agent is
// conversational-only (E2) — no skill, no target — with its own
// relay-persisted memory. Returns the new agent's pubkey. Runs over the
// operator-side runner today; the management access is the same surface the
// freehold agent will exercise through the CP runner once that migration lands.
func (e *rebuildEngine) stageCreateAgent(cfg *config.Config, name, purpose string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("create-agent needs a non-empty name")
	}
	if cfg == nil || cfg.Lxc.K3s.Vmid == nil {
		return "", fmt.Errorf("no k3s coords recorded — a created agent needs the k3s substrate")
	}
	if cfg.RelayURL == "" {
		return "", fmt.Errorf("no relay URL recorded — a created agent must join the community relay")
	}
	if cfg.Litellm.URL == "" {
		return "", fmt.Errorf("no litellm gateway recorded — a created agent needs a reasoning model")
	}
	// Collision guards: agent pods/ConfigMaps are keyed by sanitizePodName(name)
	// in the same `agents` namespace, so a name that sanitizes to the CPA's pod
	// name (or an already-created agent's) would DELETE + re-apply the CPA pod
	// (silently downgrading its brain) or collide with another agent. Reject it.
	if err := createAgentNameError(cfg, name); err != nil {
		return "", err
	}
	k3sVmid := *cfg.Lxc.K3s.Vmid

	dir := agentDirForName(name)
	pub, err := agent.EnsureIdentity(dir)
	if err != nil {
		return "", fmt.Errorf("mint %s identity: %w", name, err)
	}
	// Relay member + its own channel + Buzz profile (idempotent).
	if err := e.relayAddMember(cfg, pub); err != nil {
		return "", err
	}
	id, err := flows.LoadIdentity(dir)
	if err != nil {
		return "", err
	}
	if err := e.relayEnsureChannel(cfg, id.NostrSecretHex, name); err != nil {
		return "", err
	}
	// Identity secret (first-run-wins: identity continuity across rebuilds).
	ok, out := e.runBin(e.bins.Self, e.execArgs(
		agent.AgentIdentityScript(k3sVmid, id.NostrSecretHex, e.f.operatorPubkey, name), 120))
	if !ok {
		return "", fmt.Errorf("%s identity secret: %s", name, strings.TrimSpace(out))
	}
	// Pod manifest: a purely-conversational agent (E2). Its OPENAI_COMPAT key
	// reuses the CPA's litellm-key secret today (agents still share the gateway
	// key — the known gap; the litellm agent later gets its own scoped key).
	relayURL := cfg.RelayWsURL
	if relayURL == "" {
		relayURL = strings.Replace(cfg.RelayURL, "https://", "wss://", 1)
	}
	base := strings.TrimSuffix(cfg.Litellm.URL, "/") + "/v1"
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	ok, out = e.runBin(e.bins.Self, e.execArgs(agent.AgentManifestScript(
		k3sVmid, relayURL, prompts.AgentSystemPrompt(name, purpose), base,
		agent.CpaLiteLLMModel, name, agent.KeySecretFor(cpaName)), 420))
	if !ok {
		return "", fmt.Errorf("%s pod apply: %s", name, strings.TrimSpace(out))
	}
	// Register in the CP agent registry (A5-style) so the console sees it.
	if store, err := state.Open(rbStateDir()); err == nil {
		_ = agent.RegisterAgent(store, name, pub)
		_ = store.Save()
	}
	fmt.Fprintf(e.out, "  · agent %s live in Buzz (pubkey %s)\n", name, pub)
	return pub, nil
}

// recordCreatedAgent appends a created agent to the config (so a rebuild
// redeploys it — E3) and saves. Idempotent: re-adding an existing name updates
// its purpose/pubkey in place.
func recordCreatedAgent(configPath, name, purpose, pub string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", configPath)
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == name {
			cfg.Agents[i].Purpose = purpose
			cfg.Agents[i].Pubkey = pub
			return cfg.Save(configPath)
		}
	}
	cfg.Agents = append(cfg.Agents, config.AgentSpec{Name: name, Purpose: purpose, Pubkey: pub})
	return cfg.Save(configPath)
}

// reconcileCreatedAgents redeploys every created agent recorded in the config so
// a rebuild resurrects them (E3: created agents survive the rebuild drill, same
// as the CPA). Runs after the CPA stage; each deploy is idempotent.
func (e *rebuildEngine) reconcileCreatedAgents() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	for _, a := range cfg.Agents {
		if a.Name == "" {
			continue
		}
		if _, err := e.stageCreateAgent(cfg, a.Name, a.Purpose); err != nil {
			return fmt.Errorf("reconcile created agent %s: %w", a.Name, err)
		}
	}
	return nil
}

// createAgentNameError rejects a create name whose sanitized pod name collides
// with the CPA's pod or an already-created agent's pod (all share the `agents`
// namespace). Returns nil when the name is safe.
func createAgentNameError(cfg *config.Config, name string) error {
	if name == "" {
		return fmt.Errorf("create-agent needs a non-empty name")
	}
	pod := agent.PodName(name)
	defCPAName := agent.DefaultCPAName
	if cfg != nil && cfg.CPAName != "" {
		defCPAName = cfg.CPAName
	}
	if pod == agent.PodName(defCPAName) {
		return fmt.Errorf("create-agent %q: the sanitized pod name %q collides with the control plane agent (%q)", name, pod, defCPAName)
	}
	if cfg != nil {
		for _, a := range cfg.Agents {
			if a.Name == "" {
				continue
			}
			if agent.PodName(a.Name) == pod {
				return fmt.Errorf("create-agent %q: a created agent named %q already occupies pod name %q", name, a.Name, pod)
			}
		}
	}
	return nil
}

// --- create-agent: command ---

var createAgentCmd = &cobra.Command{
	Use:   "create-agent <name>",
	Short: "Deploy a new conversational agent (E1): own identity + relay membership + channel + pod, no skill/target",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		purpose, _ := cmd.Flags().GetString("purpose")
		configPath, _ := cmd.Flags().GetString("config")
		target, _ := cmd.Flags().GetString("target")
		addr, _ := cmd.Flags().GetString("addr")
		pk, _ := cmd.Flags().GetString("operator-pubkey")

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("no config at %s", configPath)
		}
		if pk == "" {
			pk = cfg.OperatorPubkey
		}
		e, err := newRebuildEngine(rebuildFlags{
			configPath:     configPath,
			target:         target,
			addr:           addr,
			operatorPubkey: pk,
			yes:            true,
		})
		if err != nil {
			return err
		}
		pub, err := e.stageCreateAgent(cfg, name, purpose)
		if err != nil {
			return err
		}
		if err := recordCreatedAgent(configPath, name, purpose, pub); err != nil {
			return err
		}
		fmt.Printf("%s\n", pub)
		return nil
	},
}

func init() {
	addCommonFlags(createAgentCmd, nil)
	createAgentCmd.Flags().String("target", "proxmox-box", "Target runner (the box + door key used to deploy)")
	createAgentCmd.Flags().String("config", defaultConfigPath(), "Config path")
	createAgentCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (defaults to the config's)")
	createAgentCmd.Flags().String("purpose", "", "One-line purpose for the new agent")
}

// --- watch-agents: polls the #freehold control channel for CPA create-agent
// requests and executes them (the operator-side executor; E1's deployment side).

// createReqRe matches the CPA's structured request, e.g.
//
//	create-agent name: helper purpose: helps with installs
var createReqRe = regexp.MustCompile(`(?i)create-agent`)

var watchAgentsCmd = &cobra.Command{
	Use:   "watch-agents",
	Short: "Watch #freehold for the CPA's create-agent requests and deploy them (Phase E executor)",
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		target, _ := cmd.Flags().GetString("target")
		addr, _ := cmd.Flags().GetString("addr")
		pk, _ := cmd.Flags().GetString("operator-pubkey")
		pollMs, _ := cmd.Flags().GetInt("poll-ms")
		sinceUnix, _ := cmd.Flags().GetInt64("since")

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("no config at %s", configPath)
		}
		if pk == "" {
			pk = cfg.OperatorPubkey
		}
		e, err := newRebuildEngine(rebuildFlags{
			configPath:     configPath,
			target:         target,
			addr:           addr,
			operatorPubkey: pk,
			yes:            true,
		})
		if err != nil {
			return err
		}
		if cfg.RelayURL == "" {
			return fmt.Errorf("no relay URL recorded")
		}
		// Poll as the CPA identity (a relay member) so reads are authorized.
		cpaSecret, err := cpaNostrSecret()
		if err != nil {
			return err
		}
		cpaPub, err := agent.EnsureIdentity(cpaIdentityDir())
		if err != nil {
			return err
		}
		sec := cpaSecret[:]

		fmt.Fprintf(os.Stderr, "watch-agents: watching #%s for create-agent requests from %s…\n", relayFreeholdChannel, cpaPub[:12])
		since := time.Now().Unix()
		if sinceUnix > 0 {
			since = sinceUnix
		}
		poll := time.Duration(pollMs) * time.Millisecond
		if poll < 1000 {
			poll = 10 * time.Second
		}
		// seen dedupes same-second events (Nostr timestamps are second-resolution,
		// so a second create-agent in the SAME second would otherwise be skipped
		// forever by a strict `>` watermark). Only the boundary second is kept.
		seen := map[string]bool{}
		for {
			time.Sleep(poll)
			msgs, perr := delegate.PollStream(cfg.RelayURL, sec, relayFreeholdChannel, since)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "watch-agents: poll error: %v\n", perr)
				continue
			}
			maxCt := since
			for _, m := range msgs {
				if m.CreatedAt < since {
					continue
				}
				if m.CreatedAt > maxCt {
					maxCt = m.CreatedAt
				}
				// Dedupe same-second events (process every one in this batch, not just
				// the last, which is what let a twin request get dropped).
				sig := fmt.Sprintf("%d:%s:%s", m.CreatedAt, m.Author, m.Content)
				if seen[sig] {
					continue
				}
				seen[sig] = true
				if !strings.HasPrefix(m.Author, cpaPub) {
					continue
				}
				name, purpose, okm := parseCreateReq(m.Content)
				if !okm {
					continue
				}
				fmt.Fprintf(os.Stderr, "watch-agents: %s requested create-agent %q purpose=%q\n", m.Author[:12], name, purpose)
				pub, cerr := e.stageCreateAgent(cfg, name, purpose)
				if cerr != nil {
					msg := fmt.Sprintf("create-agent for %s failed: %v", name, cerr)
					_ = delegate.PostMessage(cfg.RelayURL, sec, relayFreeholdChannel, "", msg)
					fmt.Fprintf(os.Stderr, "watch-agents: %s\n", msg)
					continue
				}
				_ = recordCreatedAgent(configPath, name, purpose, pub)
				resp := fmt.Sprintf("created agent %s — pubkey %s, live in its own channel", name, pub)
				_ = delegate.PostMessage(cfg.RelayURL, sec, relayFreeholdChannel, "", resp)
				fmt.Fprintf(os.Stderr, "watch-agents: %s\n", resp)
			}
			since = maxCt
			// Bound `seen` to the new boundary second so it can't grow unbounded.
			for sig := range seen {
				if !strings.HasPrefix(sig, fmt.Sprintf("%d:", since)) {
					delete(seen, sig)
				}
			}
		}
	},
}

func init() {
	addCommonFlags(watchAgentsCmd, nil)
	watchAgentsCmd.Flags().String("target", "proxmox-box", "Target runner (the box + door key used to deploy)")
	watchAgentsCmd.Flags().String("config", defaultConfigPath(), "Config path")
	watchAgentsCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (defaults to the config's)")
	watchAgentsCmd.Flags().Int("poll-ms", 10000, "Control-channel poll interval in ms")
	watchAgentsCmd.Flags().Int64("since", 0, "Unix-seconds to start polling from (0 = now)")
}

// cpaNostrSecret loads the CPA identity's nostr secret (the relay member used
// to poll/post on the control channel).
func cpaNostrSecret() ([]byte, error) {
	id, err := flows.LoadIdentity(cpaIdentityDir())
	if err != nil {
		return nil, fmt.Errorf("cpa identity unreadable: %w", err)
	}
	return hex.DecodeString(id.NostrSecretHex)
}

func parseCreateReq(content string) (name, purpose string, ok bool) {
	parts := createReqRe.Split(content, 2)
	if len(parts) != 2 {
		return "", "", false
	}
	rest := strings.TrimLeft(strings.TrimSpace(parts[1]), ":=\t ")
	nameRe := regexp.MustCompile(`(?i)^name[:=]\s*([a-z0-9-]+)`)
	if m := nameRe.FindStringSubmatch(rest); m != nil {
		name = m[1]
		rest = strings.TrimSpace(rest[len(m[0]):])
	} else {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", "", false
		}
		name = fields[0]
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
	}
	if m := regexp.MustCompile(`(?i)^purpose[:=]\s*(.*)$`).FindStringSubmatch(rest); m != nil {
		purpose = strings.TrimSpace(m[1])
	}
	if name == "" {
		return "", "", false
	}
	return name, purpose, true
}
