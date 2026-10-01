// Package export implements `freehold export` — the durable plane's data
// bundle: every recorded plane mount (the ONLY durable half — guest rootfs
// is reconstructible, the rebuild model's job) tarred on the substrate host,
// pulled, and wrapped with the profile config into one gzip bundle. The
// estimate is real bytes (`du -sb` per mount) and confirmed BEFORE anything
// runs. The transport is the transient DOOR_SPEC key — it works with the CP
// down.
package export

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"freehold/providers/proxmox/export"
)

const hostDumpDir = "/srv/nobackup/freehold-export"

// planeSources is every recorded durable-plane mount source, roles in
// stable order — the export's complete scope.
func planeSources(cfg *config.Config) []string {
	roles := make([]string, 0, len(cfg.Plane.Mounts))
	for role := range cfg.Plane.Mounts {
		roles = append(roles, role)
	}
	sortStrings(roles)
	var sources []string
	for _, role := range roles {
		for _, m := range cfg.Plane.Mounts[role] {
			if m.Source != "" {
				sources = append(sources, m.Source)
			}
		}
	}
	return sources
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var exportCmd = &cobra.Command{
	Use:   "export [outfile]",
	Short: "Export the durable plane: the recorded mounts' data + the profile config into one gzip bundle (estimate first, confirm; no guest rootfs — the rebuild model rebuilds guests)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "export")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		configPath := common.ProfileConfigPath(cmd)
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil || cfg.Host == "" {
			return fmt.Errorf("no world recorded at %s", configPath)
		}
		sources := planeSources(cfg)
		if len(sources) == 0 {
			return fmt.Errorf("no durable-plane mounts recorded — nothing to export")
		}

		doorExec, keyPath, cleanup, err := common.DoorExec(cfg)
		if err != nil {
			return err
		}
		defer cleanup()
		driveExec := drive.ExecFunc(doorExec)

		// The docker daemon roots among the mounts (the relay's carve-out:
		// its named volumes ride INSIDE /var/lib/docker) skip their
		// storage-driver dirs — the pulled images' unpacked layers
		// (live-verified: 812M of a 1.0G daemon root) re-pull at `docker
		// compose pull`; the volumes (the DBs) stay in.
		var duExcludes, tarExcludes []string
		for _, mounts := range cfg.Plane.Mounts {
			for _, m := range mounts {
				if m.GuestPath != "/var/lib/docker" {
					continue
				}
				for _, d := range export.DriverDirs {
					duExcludes = append(duExcludes, d)
					tarExcludes = append(tarExcludes, m.Source+"/"+d)
				}
			}
		}

		// Estimate: REAL bytes per mount (du -sb) — the number the operator
		// confirms against; a failed du aborts (never a silent zero).
		mounts, total, err := export.EstimateMounts(driveExec, sources, duExcludes)
		if err != nil {
			return err
		}
		fmt.Println("export estimate (the durable plane's real content — gzip shrinks it):")
		for _, m := range mounts {
			fmt.Printf("  · %s (%s)\n", m.Source, humanB(m.Bytes))
		}
		fmt.Printf("  total ≤ %s\n", humanB(total))
		if yes, _ := cmd.Flags().GetBool("yes"); !yes {
			if err := common.ConfirmDestructive("export (writes ~" + humanB(total) + " to the host /srv/nobackup)"); err != nil {
				return err
			}
		}

		// Host staging: ONE gzipped tar of all the mounts (the mounts live
		// on the host; the guests need not stop — the tar is not
		// crash-consistent at the second level, so take a `freehold
		// snapshot` first when the moment matters).
		outfile := exportOutfile(args, cfg.Name)
		bundleDir, err := os.MkdirTemp("", "freehold-export-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(bundleDir)
		archive, tarCmd, err := export.TarCmd(hostDumpDir, bundleName(cfg.Name), sources, tarExcludes)
		if err != nil {
			return err
		}
		if _, err := driveExec("mkdir -p "+hostDumpDir+" && rm -f "+hostDumpDir+"/*.tar.gz", 60); err != nil {
			return err
		}
		// The profile config rides along — the bundle is self-describing.
		cfgBytes, rerr := os.ReadFile(configPath)
		if rerr == nil {
			cfgUp, werr := os.CreateTemp("", "freehold-export-cfg-*")
			if werr != nil {
				return werr
			}
			defer os.Remove(cfgUp.Name())
			if _, werr = cfgUp.Write(cfgBytes); werr != nil {
				cfgUp.Close()
				return werr
			}
			cfgUp.Close()
			if _, werr = proxmox.SSHUpload(strings.TrimPrefix(cfg.Host, "root@"), keyPath, cfgUp.Name(), hostDumpDir+"/config.toml", 120); werr != nil {
				fmt.Println("  ⚠ profile config not included:", werr)
			}
		} else {
			fmt.Println("  ⚠ profile config not included:", rerr)
		}

		fmt.Printf("tarring %d mount(s) on the host…\n", len(sources))
		tarOut, err := driveExec(tarCmd, 3600)
		if err != nil {
			return err
		}
		// tar's exit contract: 0 = clean; 1 = "some files differ" — the
		// LIVE world moved mid-tar (redis's AOF, postgres's unix socket
		// "ignored", …): the archive is complete, slightly torn where the
		// DBs' own crash recovery (WAL replay / AOF truncation) covers the
		// tail — accepted, loudly. 2 = fatal (a truncated archive — the
		// worst failure a backup tool can have) fails the export.
		tarCode := 0
		if tarOut.ExitCode != nil {
			tarCode = *tarOut.ExitCode
		}
		switch tarCode {
		case 0:
		case 1:
			fmt.Println("  ⚠ live files changed during the tar (expected on a running world; the DBs' crash recovery covers the tail):")
			fmt.Println("    " + strings.TrimSpace(tarOut.Stderr))
		default:
			return fmt.Errorf("the host tar FAILED (exit %d) — a truncated archive exports nothing:\n%s", tarCode, strings.TrimSpace(tarOut.Stderr))
		}
		// Pull (size-checked against the host side) + wrap locally: the
		// bundle = the archive + the config, one directory.
		out, err := driveExec("stat -c %s "+archive, 60)
		if err != nil {
			return err
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("stat on the host archive failed (exit %d)", derefOrNeg1(out.ExitCode))
		}
		local := filepath.Join(bundleDir, filepath.Base(archive))
		pulled, err := proxmox.SSHDownload(strings.TrimPrefix(cfg.Host, "root@"), keyPath, archive, local, 3600)
		if err != nil {
			return err
		}
		if remoteN, perr := parseSize(out.Stdout); perr == nil && remoteN != pulled {
			return fmt.Errorf("pulled %s truncated (host %d, local %d)", filepath.Base(archive), remoteN, pulled)
		}
		cfgLocal := filepath.Join(bundleDir, "config.toml")
		if cfgBytes != nil {
			if err := os.WriteFile(cfgLocal, cfgBytes, 0o600); err != nil {
				return err
			}
		}
		wrap := []string{"-cf", outfile, "-C", bundleDir, filepath.Base(archive)}
		if cfgBytes != nil {
			wrap = append(wrap, "config.toml")
		}
		if out, err := exec.Command("tar", wrap...).CombinedOutput(); err != nil {
			return fmt.Errorf("tar the bundle: %w: %s", err, strings.TrimSpace(string(out)))
		}
		// The bundle IS the world's root (the cp mount carries the console
		// identity + every sealed secret) — born 0600, never the weak link.
		if err := os.Chmod(outfile, 0o600); err != nil {
			return err
		}
		if _, err := driveExec("rm -rf "+hostDumpDir, 60); err != nil {
			fmt.Println("  ⚠ host tmp cleanup failed:", err)
		}
		info, err := os.Stat(outfile)
		if err != nil {
			return err
		}
		fmt.Printf("exported: %s (%s) — restore = untar onto a fresh host's plane paths + `freehold build`\n", outfile, humanB(uint64(info.Size())))
		return nil
	},
}

// bundleName is the host-side archive's stem.
func bundleName(name string) string {
	if name == "" {
		name = "freehold"
	}
	return name + "-" + time.Now().UTC().Format("20060102T150405Z")
}

// exportOutfile resolves the output path: the operator's arg, else
// freehold-<name>-<date>.tar in the cwd.
func exportOutfile(args []string, name string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return fmt.Sprintf("freehold-%s-%s.tar", name, time.Now().UTC().Format("20060102"))
}

// derefOrNeg1 reads an exit code (-1 when the transport left it unset).
func derefOrNeg1(code *int) int {
	if code == nil {
		return -1
	}
	return *code
}

// parseSize parses a stat %s output.
func parseSize(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, 64)
}

// humanB renders bytes as a compact human string.
func humanB(n uint64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// Command is the cobra command root.go registers.
func Command() *cobra.Command { return exportCmd }

func init() {
	exportCmd.Flags().String("config", config.ConfigPath(), "Config path (default: the active profile's)")
	exportCmd.Flags().Bool("yes", false, "skip the confirm (the estimate still prints — scripting/the detached flow)")
}
