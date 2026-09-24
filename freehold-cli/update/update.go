// Package update implements `freehold update` — the remote-world updater:
// resolve a source (channel / ref / dev), acquire its binaries + migration
// scripts, redeploy the CP, run pending migrations, then repin the version.
// It never touches the local CLI.
package update

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/version"
	"freehold/freehold-cli/internal/artifact"
	"freehold/freehold-cli/internal/common"
	"freehold/freehold-cli/internal/stages"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
)

type options struct {
	stable bool
	rc     bool
	dev    bool
	ref    string
	sha    string
	check  bool
	yes    bool
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update the world's CP to another version (release assets, a ref, or the local tree)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "update")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		o := options{}
		o.stable, _ = cmd.Flags().GetBool("stable")
		o.rc, _ = cmd.Flags().GetBool("rc")
		o.dev, _ = cmd.Flags().GetBool("dev")
		o.ref, _ = cmd.Flags().GetString("ref")
		o.sha, _ = cmd.Flags().GetString("sha")
		o.check, _ = cmd.Flags().GetBool("check")
		o.yes, _ = cmd.Flags().GetBool("yes")
		return run(cmd.Context(), o)
	},
}

func init() {
	updateCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
	updateCmd.Flags().Bool("stable", false, "update to the newest stable release")
	updateCmd.Flags().Bool("rc", false, "update to the newest release candidate")
	updateCmd.Flags().Bool("dev", false, "build + deploy the local working tree")
	updateCmd.Flags().String("ref", "", "build + deploy an untagged git ref (e.g. main)")
	updateCmd.Flags().String("sha", "", "build + deploy a specific commit")
	updateCmd.Flags().Bool("check", false, "report the available version + pending migrations without changing anything")
	updateCmd.Flags().Bool("yes", false, "do not ask for confirmation")
}

func run(ctx context.Context, o options) error {
	cfg, err := config.Load(common.ConfigPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cacheDir, err := cacheDir()
	if err != nil {
		return err
	}
	channel, err := resolveChannel(cfg, o)
	if err != nil {
		return err
	}

	if o.check {
		return check(ctx, cfg, channel, cacheDir, o)
	}

	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	if !o.yes {
		fmt.Printf("update the world at %s to channel %q? [y/N] ", cfg.CPURL, channel)
		var ans string
		fmt.Scanln(&ans)
		if !strings.HasPrefix(strings.ToLower(ans), "y") {
			fmt.Println("aborted.")
			return nil
		}
	}

	set, err := acquire(ctx, o, channel, cacheDir)
	if err != nil {
		return err
	}
	fmt.Printf("acquired %s (%s @ %s)\n", set.Version, set.Channel, short(set.Commit))

	// Only the running CLI (for the self-staged deploy) is needed locally; the
	// release/build artifacts supply the rest. Do NOT ResolveBins here — a box
	// installed from release assets has no local sibling set.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary: %w", err)
	}
	bins := box.Bins{
		Self:              self,
		ReleaseConsole:    set.Console,
		ReleaseRun:        set.Runner,
		ReleaseAgentTools: set.AgentTools,
	}

	eng, err := box.NewEngine(box.FlagsFromConfig(cfg), bins)
	if err != nil {
		return err
	}
	eng.Provider = proxmox.New(eng.HostExecFunc())
	eng.ProviderFactory = stages.TransientFactory(eng)
	eng.Out = os.Stdout
	eng.Stdin = bufio.NewReader(os.Stdin)

	fmt.Println("→ deploying binaries + copying migration scripts")
	if err := eng.RedeployCp(bins, set.MigrationsDir); err != nil {
		return err
	}

	// Reconcile FIRST: the deploy stopped every freehold-runner* unit (the
	// binary ship needs the inode free) and restarted only the console serve —
	// the world-build re-runs the CP's stages through its co-located runner:
	// the agent-tools serve comes back up, the capability doors re-stage,
	// grants re-assert, the pods re-apply, and the world-build's own tail runs
	// the pending migrations.
	fmt.Println("→ reconciling the world (doors re-stage, migrations run in the build tail)")
	if err := reconcileWorld(cfg); err != nil {
		return fmt.Errorf("world reconcile failed (the deploy landed; run `freehold build` then `freehold update` to finish): %w", err)
	}

	// A final migration sweep through the now-up agent-tools (the reconcile's
	// tail already ran what was pending; this catches a failed queue and makes
	// the report visible). Version pins only after this.
	fmt.Println("→ running pending migrations")
	if err := runMigrations(cfg); err != nil {
		return fmt.Errorf("migrations failed (version NOT promoted; re-run `freehold update` to retry): %w", err)
	}

	fmt.Println("→ pinning version")
	if err := eng.StampVersionPin(version.Pin{Version: set.Version, Channel: set.Channel, Commit: set.Commit}); err != nil {
		return err
	}
	fmt.Printf("✓ updated to %s (%s)\n", set.Version, set.Channel)
	return nil
}

// resolveChannel picks the channel for this run: an explicit flag wins, else
// the CP's recorded channel, else stable.
func resolveChannel(cfg *config.Config, o options) (string, error) {
	switch {
	case o.stable:
		return "stable", nil
	case o.rc:
		return "rc", nil
	case o.dev || o.ref != "" || o.sha != "":
		return "dev", nil
	}
	if ch := currentChannel(cfg); ch != "" {
		return ch, nil
	}
	return "stable", nil
}

// currentChannel reads the CP's stamped channel, or "" when unreachable.
func currentChannel(cfg *config.Config) string {
	w, err := readWorld(cfg)
	if err != nil {
		return ""
	}
	return w.Version.Channel
}

// readWorld fetches the CP's world summary (version pin + pending migrations),
// preferring the https CPURL and falling back to the LAN IP only if unreachable.
func readWorld(cfg *config.Config) (*console.WorldSummary, error) {
	c, err := common.ConsoleLogin(cfg)
	if err != nil {
		return nil, fmt.Errorf("console login: %v", err)
	}
	return c.World()
}

// acquire resolves the source into a Set. Release channels download + verify
// assets; dev/ref/sha build the local tree or a sandbox clone.
func acquire(ctx context.Context, o options, channel, cacheDir string) (artifact.Set, error) {
	switch {
	case o.dev, channel == "dev":
		// A CP on the dev channel tracks the local tree; both `--dev` and a
		// bare update (using the recorded channel) build it.
		return artifact.BuildTree(ctx, "", "", repoRoot(), cacheDir)
	case o.ref != "" || o.sha != "":
		return artifact.BuildTree(ctx, o.ref, o.sha, "", cacheDir)
	case channel == "stable" || channel == "rc":
		return artifact.AcquireRelease(ctx, channel, filepath.Join(cacheDir, "release"))
	default:
		return artifact.Set{}, fmt.Errorf("unknown channel %q — pass --stable/--rc/--dev/--ref/--sha", channel)
	}
}

// check performs steps 1–3 in dry-run: report the CP's current version, pending
// migration count, and the available version; change nothing.
func check(ctx context.Context, cfg *config.Config, channel, cacheDir string, o options) error {
	cur := ""
	if w, err := readWorld(cfg); err == nil {
		cur = w.Version.Version
		fmt.Printf("CP version:   %s (%s)\n", orDash(cur), orDash(w.Version.Channel))
		fmt.Printf("migrations:   %d pending\n", w.MigrationsPending)
	} else {
		fmt.Printf("CP version:   unreachable (%v)\n", err)
	}
	if o.dev || o.ref != "" || o.sha != "" || channel == "dev" {
		fmt.Printf("source:       local tree / ref (build on apply)\n")
		return nil
	}
	tags, err := artifact.ListTags(ctx)
	if err != nil {
		return err
	}
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	if tag, ok := artifact.SelectTag(channel, names); ok {
		fmt.Printf("available:    %s (%s)\n", tag, channel)
		if tag == cur {
			fmt.Println("status:       up to date")
		} else {
			fmt.Println("status:       update available")
		}
	} else {
		fmt.Printf("available:    no %s release found\n", channel)
	}
	return nil
}

// runMigrations runs the CP's pending scripts through the agent-tools
// world_migrate tool (the same surface the CPA uses).
func runMigrations(cfg *config.Config) error {
	mc, err := common.WorldMCP(cfg)
	if err != nil {
		return err
	}
	text, err := common.CallAgentToolsText(mc, "world_migrate", map[string]interface{}{})
	if err != nil {
		return err
	}
	if t := strings.TrimSpace(text); t != "" {
		fmt.Println(t)
	}
	return nil
}

// reconcileWorld triggers the console's /api/world-build (the same trigger
// `freehold build` uses): the CP re-runs its build stages through its
// co-located runner — the capability doors re-stage, grants re-assert, the
// agent pods re-apply.
func reconcileWorld(cfg *config.Config) error {
	secStr, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator identity: %v", err)
	}
	key, err := oplogin.NsecToSecret(secStr)
	if err != nil {
		return err
	}
	loginURL := cfg.CPURL
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		loginURL = "http://" + ip + ":8080"
	}
	c, err := oplogin.Login(loginURL, key)
	if err != nil {
		return fmt.Errorf("console login at %s: %v", loginURL, err)
	}
	res, err := c.WorldBuild()
	if err != nil {
		return fmt.Errorf("world_build (console /api/world-build): %v", err)
	}
	if t := strings.TrimSpace(res.Report); t != "" {
		fmt.Println(t)
	}
	return nil
}

// repoRoot resolves the local tree for --dev: the running binary's repo root.
func repoRoot() string {
	self, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(self)
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "."
}

// cacheDir is the profile-scoped artifact cache (never target/).
func cacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	d := filepath.Join(base, "freehold", "update")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// lock guards against two concurrent updates.
func lock() (func(), error) {
	d, err := cacheDir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(d, "update.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// A stale lock (> 1h) is reclaimed.
			if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(p)
				return lock()
			}
			return nil, fmt.Errorf("another update is in progress (%s); remove it if stale", p)
		}
		return nil, err
	}
	f.Close()
	return func() { _ = os.Remove(p) }, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Command returns the update command for root registration.
func Command() *cobra.Command { return updateCmd }
