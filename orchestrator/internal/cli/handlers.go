package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/flows"
	"github.com/spf13/cobra"
)

// --- exec family shared flags ---

func addCommonFlags(cmd *cobra.Command, _ *CommonArgs) {
	// Register the shared runner flags so they exist at parse time (rejected
	// as unknown if added inside RunE — BLOCKING-3).
	cmd.Flags().String("addr", "127.0.0.1:8787", "Running runner MCP address (host:port or full URL)")
	cmd.Flags().String("agent-dir", defaultAgentDir(), "Agent identity dir (minted on demand if missing). Defaults to the freehold home's ops identity (legacy cwd-relative fallback)")
	cmd.Flags().String("runner-pubkey", "", "The RUNNER's Nostr pubkey — RESOLVED from ./.freehold/runner/<target> when omitted (you can't know it before provisioning; freehold reads it)")
}

// readCommonFlags reads the shared runner flags into a fresh CommonArgs (RunE).
func readCommonFlags(cmd *cobra.Command) *CommonArgs {
	c := &CommonArgs{}
	c.Addr, _ = cmd.Flags().GetString("addr")
	c.AgentDir, _ = cmd.Flags().GetString("agent-dir")
	c.RunnerPubkey, _ = cmd.Flags().GetString("runner-pubkey")
	return c
}

// --- onboard ---

var onboardCmd = &cobra.Command{
	Use:   "onboard <NAME>",
	Short: "E1: existing service + credential -> runner -> provision -> self-check -> grant",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("onboard needs <NAME>")
		}
		name := args[0]
		kind, _ := cmd.Flags().GetString("kind")
		addr, _ := cmd.Flags().GetString("address")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		cpState, _ := cmd.Flags().GetString("cp-state-dir")
		runnerDir, _ := cmd.Flags().GetString("runner-dir")
		if kind == "" || addr == "" {
			return fmt.Errorf("onboard needs --kind and --address")
		}
		if runnerDir == "" {
			runnerDir = fmt.Sprintf("./.freehold/runner/%s", name)
		}
		ensureAgentIdentity(agentDir)
		secret, err := readSecret("paste the credential for " + name + " (ENTER when done): ")
		if err != nil {
			return err
		}
		report, err := flows.Onboard(name, kind, addr, secret, agentDir, cpState, runnerDir, "")
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
		return nil
	},
}

func init() {
	onboardCmd.Flags().String("kind", "", "")
	onboardCmd.Flags().String("address", "", "")
	onboardCmd.Flags().String("agent-dir", defaultAgentDir(), "Agent identity dir (from `control-plane agent-create`)")
	onboardCmd.Flags().String("cp-state-dir", "./.freehold/control-plane", "CP state dir (default ./freehold/control-plane)")
	onboardCmd.Flags().String("runner-dir", "", "Where the runner package lands; defaults to ./.freehold/runner/<name>")
}

// --- exec ---

var execCmd = &cobra.Command{
	Use:   "exec <TARGET> <CMD>",
	Short: "Signed exec against a running runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("exec needs <TARGET> <CMD>")
		}
		target, cmds := args[0], args[1]
		common := readCommonFlags(cmd)
		secrets, _ := cmd.Flags().GetStringSlice("secret")
		timeoutS, _ := cmd.Flags().GetUint64("timeout")
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		refs := secrets
		if len(refs) == 0 {
			refs = []string{target}
		}
		out, err := c.Exec(target, cmds, refs, timeoutS)
		if err != nil {
			return err
		}
		if out.Stdout != "" {
			fmt.Print(out.Stdout)
		}
		if out.Stderr != "" {
			fmt.Fprint(os.Stderr, out.Stderr)
		}
		if out.TimedOut {
			fmt.Fprintln(os.Stderr, "TIMED OUT")
			return fmt.Errorf("exec timed out")
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			return fmt.Errorf("exec exit %d", *out.ExitCode)
		}
		return nil
	},
}

func init() {
	addCommonFlags(execCmd, nil)
	execCmd.Flags().StringSliceP("secret", "s", nil, "Secret names to request (must be the target's own credential; defaults to the target name — the provision convention)")
	execCmd.Flags().Uint64("timeout", 60, "Runner-side watchdog in seconds (client deadline sits above it)")
}

// --- readiness ---

var readinessCmd = &cobra.Command{
	Use:   "readiness",
	Short: "Readiness table from a running runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		if target == "" {
			target = resolveDefaultTarget()
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		r, err := c.Readiness()
		if err != nil {
			return err
		}
		for t, state := range r {
			fmt.Printf("%-16s %s\n", t, state)
		}
		return nil
	},
}

func init() {
	addCommonFlags(readinessCmd, nil)
	readinessCmd.Flags().String("target", "", "Target runner name (resolves its pubkey)")
}

func resolveDefaultTarget() string {
	return "proxmox-box"
}

// --- demo ---

var demoCmd = &cobra.Command{
	Use:   "demo",
	Short: "E2: run scripted exec steps (JSON file) against a running runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		stepsPath, _ := cmd.Flags().GetString("steps")
		if stepsPath == "" {
			return fmt.Errorf("demo needs --steps")
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		if target == "" {
			target = resolveDefaultTarget()
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(stepsPath)
		if err != nil {
			return err
		}
		var steps []flows.DemoStep
		if err := json.Unmarshal(raw, &steps); err != nil {
			return err
		}
		results, err := flows.RunDemo(c, steps)
		if err != nil {
			return err
		}
		for _, r := range results {
			status := "ok"
			if !r.OK {
				status = "FAIL"
			}
			fmt.Printf("%d %s %s\n", r.Index, status, r.Target)
			if r.StdoutHead != "" {
				fmt.Println("  out:", r.StdoutHead)
			}
			if r.Error != nil {
				fmt.Println("  err:", *r.Error)
			}
		}
		return nil
	},
}

func init() {
	addCommonFlags(demoCmd, nil)
	demoCmd.Flags().String("target", "", "Target runner name")
	demoCmd.Flags().String("steps", "", "JSON steps file: [{target, cmd, secrets?}]")
}

// --- console-login ---

var consoleLoginCmd = &cobra.Command{
	Use:   "console-login",
	Short: "C3.5: log into a console over NIP-98 with YOUR identity; prints the session cookie for use in a browser or curl",
	RunE: func(cmd *cobra.Command, args []string) error {
		url, _ := cmd.Flags().GetString("url")
		identity, _ := cmd.Flags().GetString("identity")
		nsec, _ := cmd.Flags().GetString("nsec")
		if url == "" {
			return fmt.Errorf("console-login needs --url")
		}
		var secret [32]byte
		if identity != "" {
			h, err := agentSecretHex(identity)
			if err != nil {
				return fmt.Errorf("console-login needs YOUR key: pass --identity <DIR> or --nsec <64-hex>: %v", err)
			}
			secret, err = secretFromHex(h)
			if err != nil {
				return err
			}
		} else if nsec != "" {
			s, err := nsecToSecret(nsec)
			if err != nil {
				return err
			}
			secret = s
		} else {
			return fmt.Errorf("console-login needs YOUR key: pass --identity <DIR> or --nsec <64-hex>")
		}
		c, err := console.Login(url, secret[:], 15*1000*1000*1000)
		if err != nil {
			return err
		}
		fmt.Println(c.Cookie())
		return nil
	},
}

func init() {
	consoleLoginCmd.Flags().String("url", "", "Console base URL (e.g. http://freehold.example:8080)")
	consoleLoginCmd.Flags().String("identity", "", "YOUR identity dir (its nsec signs the NIP-98 login; never leaves)")
	consoleLoginCmd.Flags().String("nsec", "", "YOUR Nostr secret — nsec1<bech32> (what you actually hold) or bare 64-hex; signs the login directly and never leaves your machine. Either --identity or --nsec is required")
}

// --- memory ---

var memoryCmd = &cobra.Command{
	Use:   "memory <set|get> <key> [value]",
	Short: "D3: the agent's encrypted relay memory (kind 30174, self-sealed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		relayURL, _ := cmd.Flags().GetString("relay-url")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		if relayURL == "" || agentDir == "" {
			return fmt.Errorf("memory needs --relay-url and --agent-dir")
		}
		if len(args) < 2 {
			return fmt.Errorf("memory needs <ACTION> <KEY>")
		}
		action, key := args[0], args[1]
		secret, err := agentSecretFromDir(agentDir)
		if err != nil {
			return err
		}
		sec, err := secretFromHex(secret)
		if err != nil {
			return err
		}
		switch action {
		case "set":
			if len(args) < 3 {
				return fmt.Errorf("memory set needs a VALUE")
			}
			return writeMemoryCmd(relayURL, sec, key, args[2])
		case "get":
			val, ok, err := readMemoryCmd(relayURL, sec, key)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no memory for %s", key)
			}
			fmt.Println(val)
			return nil
		default:
			return fmt.Errorf("action must be set|get (got %s)", action)
		}
	},
}

func init() {
	memoryCmd.Flags().String("relay-url", "", "Relay URL (http://host:port)")
	memoryCmd.Flags().String("agent-dir", "", "Agent identity dir (the CPA's keypair — memory seals to its enc key)")
}

// --- readSecret ---

func readSecret(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

func parseUint(s string) (uint64, error) { return strconv.ParseUint(s, 10, 64) }

var _ = client.ExecOutcome{}
