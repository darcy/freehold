package cli

import (
	"fmt"
	"time"

	"freehold/orchestrator-go/internal/bootstrap"
	"freehold/orchestrator-go/internal/delegate"
	"freehold/orchestrator-go/internal/flows"
	"freehold/orchestrator-go/internal/relay"
	"github.com/spf13/cobra"
)

func addCommonTo(cmd *cobra.Command, c *CommonArgs) {
	addCommonFlags(cmd, c)
}

// --- relay-profile ---

var relayProfileCmd = &cobra.Command{
	Use:   "relay-profile",
	Short: "Relay surface: publish a profile (kind 0) so clients show a name",
	RunE: func(cmd *cobra.Command, args []string) error {
		relayURL, _ := cmd.Flags().GetString("relay-url")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		name, _ := cmd.Flags().GetString("name")
		about, _ := cmd.Flags().GetString("about")
		if relayURL == "" || agentDir == "" || name == "" {
			return fmt.Errorf("relay-profile needs --agent-dir --relay-url --name")
		}
		ensureAgentIdentity(agentDir)
		secretHex, err := agentSecretFromDir(agentDir)
		if err != nil {
			return err
		}
		sec, err := secretFromHex(secretHex)
		if err != nil {
			return err
		}
		if err := relay.PublishProfile(relayURL, sec[:], name, about); err != nil {
			return err
		}
		fmt.Printf("profile published for %s\n", name)
		return nil
	},
}

func init() {
	relayProfileCmd.Flags().String("agent-dir", "", "Identity dir (the agent whose profile this is)")
	relayProfileCmd.Flags().String("relay-url", "", "")
	relayProfileCmd.Flags().String("name", "", `Display name (e.g. "freehold" for the CPA)`)
	relayProfileCmd.Flags().String("about", "", "")
}

// --- relay-join ---

var relayJoinCmd = &cobra.Command{
	Use:   "relay-join",
	Short: "Relay surface: join a channel (kind 9021) — agents join #freehold by default after a deploy",
	RunE: func(cmd *cobra.Command, args []string) error {
		relayURL, _ := cmd.Flags().GetString("relay-url")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		channel, _ := cmd.Flags().GetString("channel")
		if relayURL == "" || agentDir == "" || channel == "" {
			return fmt.Errorf("relay-join needs --agent-dir --relay-url --channel")
		}
		ensureAgentIdentity(agentDir)
		secretHex, _ := agentSecretFromDir(agentDir)
		sec, _ := secretFromHex(secretHex)
		if err := relay.JoinChannel(relayURL, sec[:], channel); err != nil {
			return err
		}
		fmt.Printf("join request sent for %s\n", channel)
		return nil
	},
}

func init() {
	relayJoinCmd.Flags().String("agent-dir", "", "Identity dir (the agent joining)")
	relayJoinCmd.Flags().String("relay-url", "", "")
	relayJoinCmd.Flags().String("channel", "", "Channel id (uuid) — e.g. the #freehold channel")
}

// --- relay-setup ---

var relaySetupCmd = &cobra.Command{
	Use:   "relay-setup",
	Short: "Relay surface: the bootstrap step — ensures the #freehold channel exists (open, deterministic id) and joins every listed agent to it",
	RunE: func(cmd *cobra.Command, args []string) error {
		relayURL, _ := cmd.Flags().GetString("relay-url")
		channel, _ := cmd.Flags().GetString("channel")
		agents, _ := cmd.Flags().GetString("agents")
		if relayURL == "" || agents == "" {
			return fmt.Errorf("relay-setup needs --relay-url --agents")
		}
		dirs := splitCSV(agents)
		if len(dirs) == 0 {
			return fmt.Errorf("no agents given")
		}
		// First = the creator/@freehold.
		ensureAgentIdentity(dirs[0])
		secretHex, err := agentSecretFromDir(dirs[0])
		if err != nil {
			return err
		}
		sec, _ := secretFromHex(secretHex)
		if err := delegate.EnsureChannel(relayURL, sec[:], channel, "#freehold"); err != nil {
			return err
		}
		fmt.Printf("ensured #freehold channel %s\n", channel)
		// Join every agent (including the creator).
		for _, dir := range dirs {
			ensureAgentIdentity(dir)
			sh, _ := agentSecretFromDir(dir)
			s, _ := secretFromHex(sh)
			if err := relay.JoinChannel(relayURL, s[:], channel); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warn: join for %s failed: %v\n", dir, err)
				continue
			}
		}
		fmt.Printf("joined %d agents to %s\n", len(dirs), channel)
		return nil
	},
}

func init() {
	relaySetupCmd.Flags().String("relay-url", "", "")
	relaySetupCmd.Flags().String("channel", "00000000-0000-4000-8000-00000000f0ef", "#freehold channel id (deterministic; clients render the NAME from the channel metadata, so the id only needs to be stable)")
	relaySetupCmd.Flags().String("agents", "", "Comma-separated agent identity dirs to join (first = the creator/@freehold)")
}

// --- relay-member ---

var relayMemberCmd = &cobra.Command{
	Use:   "relay-member",
	Short: "C2: add a relay member through the relay-admin runner (buzz-admin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		pubkey, _ := cmd.Flags().GetString("pubkey")
		role, _ := cmd.Flags().GetString("role")
		lxcStr, _ := cmd.Flags().GetString("lxc")
		composeDir, _ := cmd.Flags().GetString("compose-dir")
		if pubkey == "" {
			return fmt.Errorf("relay-member needs --pubkey")
		}
		if !isHex64(pubkey) {
			return fmt.Errorf("relay member pubkey must be a 64-hex Nostr pubkey (got %q)", pubkey)
		}
		if role != "" && role != "member" && role != "admin" {
			return fmt.Errorf("relay member role must be 'member' or 'admin' (got %q)", role)
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		var lxc *uint32
		if lxcStr != "" {
			var v uint32
			fmt.Sscanf(lxcStr, "%d", &v)
			lxc = &v
		}
		roleSuffix := ""
		if role != "" {
			roleSuffix = " --role " + role
		}
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s%s", composeDir, pubkey, roleSuffix)
		full := cmdLine
		if lxc != nil {
			full = fmt.Sprintf("pct exec %d -- sh -c '%s'", *lxc, cmdLine)
		}
		out, err := bootstrap.ExecToOK(c, target, full, "buzz-admin add-member", 120)
		if err != nil {
			return err
		}
		roleNote := ""
		if role != "" {
			roleNote = " (" + role + ")"
		}
		fmt.Printf("relay member %s%s: %s\n", pubkey, roleNote, out.Stdout)
		return nil
	},
}

func init() {
	addCommonFlags(relayMemberCmd, nil)
	relayMemberCmd.Flags().String("target", "proxmox-box", "Target runner (the box holding the relay host)")
	relayMemberCmd.Flags().String("pubkey", "", "Nostr pubkey (64-hex) to add as a relay member")
	relayMemberCmd.Flags().String("role", "", "Role: member (default) or admin (owner comes from RELAY_OWNER_PUBKEY)")
	relayMemberCmd.Flags().String("lxc", "", "The LXC on the box holding the relay compose stack")
	relayMemberCmd.Flags().String("compose-dir", "/srv/data/relay/deploy/compose", "Compose project dir on the relay host")
}

// --- delegate ---

var delegateCmd = &cobra.Command{
	Use:   "delegate",
	Short: "E: the CPA delegates a task to a relay-addressable peer agent",
	RunE: func(cmd *cobra.Command, args []string) error {
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		relayURL, _ := cmd.Flags().GetString("relay-url")
		channel, _ := cmd.Flags().GetString("channel")
		peer, _ := cmd.Flags().GetString("peer")
		task, _ := cmd.Flags().GetString("task")
		timeoutS, _ := cmd.Flags().GetUint64("timeout-secs")
		if agentDir == "" || relayURL == "" || peer == "" || task == "" {
			return fmt.Errorf("delegate needs --agent-dir --relay-url --peer --task")
		}
		ensureAgentIdentity(agentDir)
		secretHex, err := agentSecretFromDir(agentDir)
		if err != nil {
			return err
		}
		sec, _ := secretFromHex(secretHex)
		id, _ := randomHex24()
		if err := delegate.EnsureChannel(relayURL, sec[:], channel, "#freehold"); err != nil {
			return err
		}
		if err := delegate.PostMessage(relayURL, sec[:], channel, peer, delegate.RequestContent(id, task)); err != nil {
			return err
		}
		// Poll unfiltered (the requester path — a #p-filtered query hangs for
		// some identities, verified live).
		deadline := time.Now().Add(time.Duration(timeoutS) * time.Second)
		since := time.Now().Unix()
		for {
			if time.Now().After(deadline) {
				return fmt.Errorf("delegate: no result from peer within %ds (id %s)", timeoutS, id)
			}
			msgs, err := delegate.PollStream(relayURL, sec[:], channel, since)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				env := delegate.ParseEnvelope(m.Content)
				if env == nil || env.Type != delegate.JobResult || env.ID != id {
					continue
				}
				ok := env.OK != nil && *env.OK
				out := ""
				if env.Out != nil {
					out = *env.Out
				}
				if ok {
					fmt.Println(out)
					return nil
				}
				return fmt.Errorf("delegate task failed: %s", out)
			}
			since = time.Now().Unix()
			time.Sleep(2 * time.Second)
		}
	},
}

func init() {
	delegateCmd.Flags().String("agent-dir", "", "CPAs identity dir")
	delegateCmd.Flags().String("relay-url", "", "")
	delegateCmd.Flags().String("channel", "00000000-0000-4000-8000-00000000f0ee", "The auto-ops channel id (uuid) — created idempotently on first use")
	delegateCmd.Flags().String("peer", "", "The peer agent's Nostr pubkey (64-hex)")
	delegateCmd.Flags().String("task", "", "The task (a shell command the peer runs via the runner)")
	delegateCmd.Flags().Uint64("timeout-secs", 90, "Seconds to wait for the peer's result")
}

// --- delegate-peer ---

var delegatePeerCmd = &cobra.Command{
	Use:   "delegate-peer",
	Short: "E: the peer agent — watches for delegated requests, execs them via the runner (runner-direct), posts the results",
	RunE: func(cmd *cobra.Command, args []string) error {
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		relayURL, _ := cmd.Flags().GetString("relay-url")
		channel, _ := cmd.Flags().GetString("channel")
		runnerAddr, _ := cmd.Flags().GetString("runner-addr")
		runnerPubkey, _ := cmd.Flags().GetString("runner-pubkey")
		target, _ := cmd.Flags().GetString("target")
		requester, _ := cmd.Flags().GetString("requester")
		watch, _ := cmd.Flags().GetUint64("watch")
		interval, _ := cmd.Flags().GetUint64("interval")
		if agentDir == "" || relayURL == "" || runnerPubkey == "" || target == "" || requester == "" {
			return fmt.Errorf("delegate-peer needs --agent-dir --relay-url --runner-pubkey --target --requester")
		}
		ensureAgentIdentity(agentDir)
		secretHex, err := agentSecretFromDir(agentDir)
		if err != nil {
			return err
		}
		sec, _ := secretFromHex(secretHex)
		mcpClient, err := flows.Connect(runnerAddr, agentDir, runnerPubkey)
		if err != nil {
			return err
		}
		if interval == 0 {
			interval = 3
		}
		deadline := time.Time{}
		if watch > 0 {
			deadline = time.Now().Add(time.Duration(watch) * time.Second)
		}
		for {
			msgs, err := delegate.PollStreamP(relayURL, sec[:], channel, pubkeyOfRequester(sec), time.Now().Unix()-5)
			if err == nil {
				for _, m := range msgs {
					if m.Author != requester {
						continue
					}
					env := delegate.ParseEnvelope(m.Content)
					if env == nil || env.Type != delegate.JobRequest {
						continue
					}
					task := ""
					if env.Task != nil {
						task = *env.Task
					}
					outcome, err := mcpClient.Exec(target, task, []string{target}, 60)
					if err != nil {
						detail := err.Error()
						delegate.PostMessage(relayURL, sec[:], channel, requester, delegate.ResultContent(env.ID, false, detail))
					} else if outcome.TimedOut || outcome.ExitCode == nil || *outcome.ExitCode != 0 {
						detail := "exec failed"
						if outcome.Stderr != "" {
							detail = outcome.Stderr
						}
						delegate.PostMessage(relayURL, sec[:], channel, requester, delegate.ResultContent(env.ID, false, detail))
					} else {
						delegate.PostMessage(relayURL, sec[:], channel, requester, delegate.ResultContent(env.ID, true, outcome.Stdout))
					}
				}
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Duration(interval) * time.Second)
		}
		return nil
	},
}

func init() {
	delegatePeerCmd.Flags().String("agent-dir", "", "The peer agent's identity dir")
	delegatePeerCmd.Flags().String("relay-url", "", "")
	delegatePeerCmd.Flags().String("channel", "00000000-0000-4000-8000-00000000f0ee", "")
	delegatePeerCmd.Flags().String("runner-addr", "127.0.0.1:8787", "The runner's MCP address (the peer execs tasks runner-direct)")
	delegatePeerCmd.Flags().String("runner-pubkey", "", "The runner's Nostr pubkey (signature audience)")
	delegatePeerCmd.Flags().String("target", "", "The runner TARGET name for the exec (e.g. proxmox-box)")
	delegatePeerCmd.Flags().String("requester", "", "The ONLY delegator allowed to request execs (the CPA's pubkey, 64-hex) — REQUIRED, fail-closed: the peer proxies for NOBODY else (any-member requests would hollow out the grant model)")
	delegatePeerCmd.Flags().Uint64("watch", 0, "Seconds to watch for requests (0 = single pass)")
	delegatePeerCmd.Flags().Uint64("interval", 3, "Poll interval seconds")
	delegatePeerCmd.Flags().String("console-url", "", "The console base URL where this agent REGISTERS at start")
	delegatePeerCmd.Flags().String("console-identity", "", "Operator identity DIR for the registry login")
	delegatePeerCmd.Flags().String("name", "", "The agent's registry name (default: the agent dir name)")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range splitOn(s, ",") {
		if t := trimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitOn(s, sep string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if string(r) == sep {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(out, cur)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
