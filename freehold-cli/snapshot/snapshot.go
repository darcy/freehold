// Package snapshot implements `freehold snapshot` — point-in-time snapshots
// of the whole durable plane (every recorded mount) under one name, with a
// guarded rollback. The transport is the transient DOOR_SPEC key (direct root
// SSH), the same path teardown's dead-CP flow rides, so every verb here works
// with the control plane down.
package snapshot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/providers/proxmox/drive"
)

// guestVmidOf maps a plane role to its recorded VMID (0 = absent/unmanaged).
func guestVmidOf(cfg *config.Config, role string) uint32 {
	switch role {
	case "relay":
		if cfg.Lxc.Relay.Vmid != nil {
			return *cfg.Lxc.Relay.Vmid
		}
	case "cp":
		if cfg.Lxc.Cp.Vmid != nil {
			return *cfg.Lxc.Cp.Vmid
		}
	case "k3s":
		if cfg.Lxc.K3s.Vmid != nil {
			return *cfg.Lxc.K3s.Vmid
		}
	}
	return 0
}

// planeSources is every recorded durable-plane mount source, roles in stable
// order. This is the COMPLETE snapshot-able inventory by the backup rule —
// guest rootfs is reconstructible, the plane is not.
func planeSources(cfg *config.Config) ([]string, map[string]string) {
	roles := make([]string, 0, len(cfg.Plane.Mounts))
	for role := range cfg.Plane.Mounts {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var sources []string
	sourceRole := map[string]string{}
	for _, role := range roles {
		for _, m := range cfg.Plane.Mounts[role] {
			if m.Source != "" {
				sources = append(sources, m.Source)
				sourceRole[m.Source] = role
			}
		}
	}
	return sources, sourceRole
}

// doorExec opens the direct-to-host transport: this box's DOOR_SPEC key over
// root SSH (common.DoorExec — the shared transient path), or — with sshKey
// set (the CP guest's staged cp-verb key) — that key instead, so the same
// verbs run ON the CP guest (the cp-local-root runner execs them for the
// data department).
func doorExec(cfg *config.Config, sshKey string) (drive.ExecFunc, func(), error) {
	exec, _, cleanup, err := common.DoorExecWithKey(cfg, sshKey)
	return drive.ExecFunc(exec), cleanup, err
}

// planeVolumes resolves the recorded sources per the recorded backend kind.
// The second return maps each source back to its role (guest mapping for the
// rollback's stop/start).
func planeVolumes(exec drive.ExecFunc, cfg *config.Config) ([]drive.PlaneVolume, map[string]string, error) {
	kind := ""
	if cfg.Plane.BackendKind != nil {
		kind = *cfg.Plane.BackendKind
	}
	sources, sourceRole := planeSources(cfg)
	volumes, err := drive.ResolvePlaneVolumes(exec, kind, sources)
	if err != nil {
		return nil, nil, err
	}
	return volumes, sourceRole, nil
}

// listJSON is the machine-readable list (the TUI's picker source).
type listJSON struct {
	Snapshots []drive.SnapshotInfo `json:"snapshots"`
}

var snapCmd = &cobra.Command{
	Use:   "snapshot [label]",
	Short: "Snapshot the whole durable plane (every recorded mount) under one name; --list / --rm <name> / `rollback` subcommand",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "snapshot")
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
		key, _ := cmd.Flags().GetString("ssh-key")
		exec, cleanup, err := doorExec(cfg, key)
		if err != nil {
			return err
		}
		defer cleanup()
		volumes, _, err := planeVolumes(exec, cfg)
		if err != nil {
			return err
		}

		if list, _ := cmd.Flags().GetBool("list"); list {
			return printList(exec, volumes, listJSONFlag(cmd))
		}
		if rm, _ := cmd.Flags().GetString("rm"); rm != "" {
			fmt.Printf("removing snapshot %s from every durable-plane volume…\n", rm)
			return drive.SnapshotRemove(exec, volumes, rm)
		}

		label := ""
		switch len(args) {
		case 0:
		case 1:
			label = args[0]
		default:
			return fmt.Errorf("one optional label expected (got %d args)", len(args))
		}
		if err := drive.ValidateSnapshotLabel(label); err != nil {
			return err
		}
		name := drive.SnapshotName(time.Now(), label)
		fmt.Printf("snapshotting %d durable-plane volume(s) as %s…\n", len(volumes), name)
		if err := drive.SnapshotCreate(exec, volumes, name); err != nil {
			return err
		}
		fmt.Printf("done: %s (survives teardown/rebuild — it is dataset-level)\n", name)
		return nil
	},
}

func listJSONFlag(cmd *cobra.Command) bool {
	j, _ := cmd.Flags().GetBool("json")
	return j
}

func printList(exec drive.ExecFunc, volumes []drive.PlaneVolume, asJSON bool) error {
	snaps, err := drive.SnapshotList(exec, volumes)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(listJSON{Snapshots: snaps})
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots yet — `freehold snapshot [label]` takes one")
		return nil
	}
	for _, s := range snaps {
		created := "—"
		if s.Created > 0 {
			created = time.Unix(s.Created, 0).UTC().Format("2006-01-02 15:04:05Z")
		}
		partial := ""
		if s.Present < s.Volumes {
			partial = fmt.Sprintf("  (PARTIAL: %d/%d volumes)", s.Present, s.Volumes)
		}
		fmt.Printf("  %s  %s%s\n", s.Name, created, partial)
	}
	return nil
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll EVERY durable-plane volume back to a snapshot (guests stop; newer snapshots are destroyed; confirmation required)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "snapshot rollback")
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
		key, _ := cmd.Flags().GetString("ssh-key")
		exec, cleanup, err := doorExec(cfg, key)
		if err != nil {
			return err
		}
		defer cleanup()
		volumes, sourceRole, err := planeVolumes(exec, cfg)
		if err != nil {
			return err
		}
		snaps, err := drive.SnapshotList(exec, volumes)
		if err != nil {
			return err
		}
		if len(snaps) == 0 {
			return fmt.Errorf("no snapshots to roll back to — take one first: `freehold snapshot`")
		}

		yes, _ := cmd.Flags().GetBool("yes")
		to, _ := cmd.Flags().GetString("to")
		if to == "" && yes {
			// Scripted mode (the TUI's --yes path): blank --to means newest.
			to = snaps[0].Name
		}
		if to == "" {
			to, err = pickSnapshot(snaps)
			if err != nil {
				return err
			}
		}
		if err := drive.ValidateSnapshotName(to); err != nil {
			return err
		}
		// The named snapshot must exist COMPLETE: a partial (a create that
		// died mid-flight) rolled back per-volume would tear the plane —
		// rm it (the crash-cleanup path) or pick another.
		byName, found := findSnapshot(snaps, to)
		if !found {
			return fmt.Errorf("no snapshot named %s — `snapshot --list` for the names", to)
		}
		if byName.Present < byName.Volumes {
			return fmt.Errorf("%s is PARTIAL (%d/%d volumes) — refusing: `snapshot --rm %s` or pick a complete one", to, byName.Present, byName.Volumes, to)
		}

		guest, _ := cmd.Flags().GetBool("guest")
		if guest {
			// The handoff path: this process lives ON the CP guest, whose
			// data is among the volumes being rolled back — the process
			// cannot survive its own guest's stop. So: the net is taken
			// in-process (before any stop), a detached host-side script
			// carries the rollback + starts + CP revival, and the CP guest
			// stops LAST (dying with this exec). The agent re-polls once
			// the runner is back; the handoff's log is /srv/nobackup/
			// rollback-handoff.log on the host.
			return guestHandoffRollback(exec, cfg, volumes, to, sourceRole)
		}

		fresh, _ := cmd.Flags().GetBool("fresh-snapshot")
		if fresh {
			if err := preRollbackSnapshot(exec, volumes, snaps, to); err != nil {
				return err
			}
		}

		if !yes {
			fmt.Println("rollback stops every plane guest (relay/k3s/CP), restores ALL durable-plane data to " + to + ", and restarts them.")
			fmt.Println("  ZFS: NEWER snapshots than the target are DESTROYED on every volume — the safety net's")
			fmt.Println("    send-file in /srv/nobackup is what survives (zfs receive restores it).")
			fmt.Println("  LVM-thin: the snapshot block-copies onto the volume (dd) — snapshots survive.")
			if err := common.ConfirmDestructive("rollback to " + to); err != nil {
				return err
			}
		}

		// Stop per ROLE and remember what THIS run actually stopped: an
		// absent guest (a recorded VMID whose guest is gone — normal after
		// a compute-only teardown) is skipped at stop AND at start — it is
		// not a start failure, and it never blocks the CP's revival.
		stopped, guestsToStart, serr := stopAllGuests(exec, cfg, sourceRole)
		if serr != nil {
			return serr
		}
		if len(guestsToStart) > 0 {
			fmt.Printf("stopped %d guest(s): %s\n", len(guestsToStart), commaU32(guestsToStart))
		}
		fmt.Printf("rolling back to %s…\n", to)
		if err := drive.SnapshotRollback(exec, volumes, to); err != nil {
			// The plane may be half-rolled/mount-unverified: restore every
			// mount (best-effort), then only restart the guests when ALL of
			// them verify — guests up against an unmounted host path write
			// into the host root fs; a mixed plane gets stopped guests and
			// a named recovery (re-running the rollback converges — the
			// pre-rollback net exists for exactly this).
			fmt.Println("restoring the plane mounts before the guests come back…")
			var unmounted []string
			for _, v := range volumes {
				out, merr := exec("mountpoint -q "+v.Source+" || mount "+v.Source, 120)
				if merr != nil || (out.ExitCode != nil && *out.ExitCode != 0) {
					unmounted = append(unmounted, v.Source)
				}
			}
			if len(unmounted) > 0 {
				return fmt.Errorf("rollback FAILED and these plane volumes are NOT mounted — the guests stay STOPPED; mount each by hand, then re-run the rollback (the pre-rollback net converges it): %s",
					strings.Join(unmounted, ", "))
			}
			startGuests(exec, guestsToStart)
			return fmt.Errorf("rollback failed (plane verified, guests restarted): %w", err)
		}
		fmt.Printf("starting %d guest(s)…\n", len(guestsToStart))
		failed := startGuests(exec, guestsToStart)
		// The CP's revival runs whenever THIS RUN stopped the CP guest —
		// its console (a nohup serve) + co-located runner (a transient
		// unit) died with the stop and nothing on the guest restarts them;
		// an unrelated guest's failed start must not leave the CP down.
		// (Live-verified on the librem world: the update flow's idempotent
		// redeploy is the sanctioned revival.)
		if stopped["cp"] {
			fmt.Println("the CP guest restarted — re-running the update flow to bring the control plane back (a few minutes)…")
			if uerr := reRunUpdate(configPath); uerr != nil {
				return fmt.Errorf("rollback complete, but the control-plane restart FAILED — run `freehold update --config %s` by hand: %w", configPath, uerr)
			}
		}
		if failed > 0 {
			fmt.Printf("⚠ rollback complete, but %d guest(s) FAILED TO START — pct start <id> by hand\n", failed)
			return fmt.Errorf("%d guest(s) failed to start after the rollback", failed)
		}
		fmt.Printf("rollback complete: the plane is at %s\n", to)
		return nil
	},
}

// sourceRoleHas reports whether the role appears among the plane's sources.
func sourceRoleHas(sourceRole map[string]string, role string) bool {
	for _, r := range sourceRole {
		if r == role {
			return true
		}
	}
	return false
}

// reRunUpdate re-runs the update flow (this binary, non-interactive) — the
// idempotent redeploy that restarts the CP's console + runner + agent-tools.
func reRunUpdate(configPath string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "update", "--config", configPath, "--non-interactive")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(tail))
	}
	return nil
}

// reviveScriptPath is the deploy-staged revival script's location (the
// BinDir convention deploy-cp ships to).
func reviveScriptPath() string { return "/srv/data/cp/bin/revive-cp.sh" }

// guestHandoffRollback rolls the plane back from a process that lives ON the
// CP guest. The CP guest's own data is among the volumes, so the process
// cannot survive the rollback: the pre-rollback net is taken in-process
// (before any stop), the non-CP guests stop, a detached script on the HOST
// carries the rollback commands (rendered by the same drive code that
// executes them in-process — no drift), and the CP guest stops LAST — this
// exec dies at that stop, which is the design: the host script takes over,
// starts the guests back, and revives the CP with the deploy-staged script.
// Everything logs to /srv/nobackup/rollback-handoff.log on the host.
func guestHandoffRollback(exec drive.ExecFunc, cfg *config.Config, volumes []drive.PlaneVolume, to string, sourceRole map[string]string) error {
	snaps, err := drive.SnapshotList(exec, volumes)
	if err != nil {
		return err
	}
	// The net is not optional when this process dies mid-flow.
	if err := preRollbackSnapshot(exec, volumes, snaps, to); err != nil {
		return err
	}

	// Stop the non-CP guests first, remembering what actually stopped (the
	// script restarts exactly those).
	roles := make([]string, 0, len(sourceRole))
	seen := map[string]bool{}
	for role := range sourceRole {
		if !seen[role] && role != "cp" {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	var toStart []uint32
	for _, role := range roles {
		id := guestVmidOf(cfg, role)
		if id == 0 {
			continue
		}
		wasRunning, serr := stopGuest(exec, id)
		if serr != nil {
			return fmt.Errorf("guest %d (%s) failed to stop — the rollback ABORTS before any data moved (no handoff launched yet): %w", id, role, serr)
		}
		if wasRunning {
			toStart = append(toStart, id)
		}
	}
	cpID := guestVmidOf(cfg, "cp")

	// Ship the handoff script (sync), then launch it detached.
	ship := fmt.Sprintf("echo %s | base64 -d > /srv/nobackup/rollback-handoff.sh && chmod 700 /srv/nobackup/rollback-handoff.sh",
		base64Encode(handoffScript(volumes, to, toStart, cpID)))
	out, err := exec(ship, 60)
	if err != nil {
		return fmt.Errorf("the handoff script failed to ship to the host: %w", err)
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return fmt.Errorf("the handoff script failed to ship to the host: %s", strings.TrimSpace(out.Stderr))
	}
	if _, err := exec("setsid nohup sh /srv/nobackup/rollback-handoff.sh >/dev/null 2>&1 < /dev/null & echo launched", 30); err != nil {
		return fmt.Errorf("the handoff script failed to launch on the host: %w", err)
	}

	if cpID == 0 {
		return fmt.Errorf("no cp guest recorded — this process cannot be the CP guest; use the box path (without --guest)")
	}
	_, _ = exec("echo \"$(date -Is) handoff: stopping the cp guest (the verb's exec ends here — the host script takes over)\" >> /srv/nobackup/rollback-handoff.log", 30)
	// The final stop ends this process; the return below is unreachable in
	// practice (the exec session dies), but a FAILED stop leaves the process
	// alive with the handoff armed — say so and how to disarm.
	_, serr := stopGuest(exec, cpID)
	return fmt.Errorf("the cp guest failed to stop and this process survives: kill the armed handoff (`pkill -f rollback-handoff.sh` on %s) and roll back from the operator's box: %v", cfg.Host, serr)
}

// handoffScript renders the detached host-side rollback: wait for the CP
// guest to stop, roll every volume back (the drive-rendered commands), start
// the stopped guests back, revive the CP.
func handoffScript(volumes []drive.PlaneVolume, to string, toStart []uint32, cpID uint32) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# freehold guest-handoff rollback — generated on the CP guest; do not edit.\n")
	b.WriteString("exec >> /srv/nobackup/rollback-handoff.log 2>&1\n")
	b.WriteString("echo \"$(date -Is) handoff: waiting for the cp guest to stop\"\n")
	if cpID != 0 {
		b.WriteString(fmt.Sprintf("for i in $(seq 1 120); do pct status %d | grep -q 'status: running' || break; sleep 2; done\n", cpID))
	}
	for _, v := range volumes {
		st := drive.RollbackStepFor(v, to)
		b.WriteString(fmt.Sprintf("echo \"rollback %s\"; { %s; } || { %s; echo \"$(date -Is) ROLLBACK FAILED at %s\"; exit 1; }\n",
			v.Source, st.Cmd, mountRestoreOf(st.Cmd, v.Source), v.Source))
	}
	for _, g := range append(toStart, cpID) {
		if g == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("echo \"$(date -Is) starting %d\"; pct start %d || echo \"$(date -Is) guest %d FAILED TO START\"\n", g, g, g))
	}
	if cpID != 0 {
		b.WriteString(fmt.Sprintf("sleep 3; echo \"$(date -Is) reviving the cp\"; pct exec %d -- sh -c %s || echo \"$(date -Is) REVIVAL FAILED — run freehold update from the operator's box\"\n",
			cpID, shQuote(reviveScriptPath())))
	}
	b.WriteString("echo \"$(date -Is) handoff complete\"\n")
	return b.String()
}

// mountRestoreOf renders the failure-path mount restore for a rollback
// command (the LVM seq unmounts; a failure must leave the mount back).
func mountRestoreOf(cmd, source string) string {
	if strings.Contains(cmd, "umount") {
		return "mount " + source + " 2>/dev/null; true"
	}
	return "true"
}

// shQuote single-quotes a string for sh -c.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\'`) + "'" }

// base64Encode encodes for a shell pipe.
func base64Encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// findSnapshot locates a listed snapshot by name.
func findSnapshot(snaps []drive.SnapshotInfo, name string) (drive.SnapshotInfo, bool) {
	for _, s := range snaps {
		if s.Name == name {
			return s, true
		}
	}
	return drive.SnapshotInfo{}, false
}

// preRollbackSnapshot guarantees a SURVIVING escape hatch for the rollback:
// it captures the pre-rollback LIVE state fresh (everything newer than the
// target is doomed by ZFS's `rollback -r` — snapshots AND the un-snapshotted
// divergence, up to a day's worth when the target is old — and only a NEW
// snapshot carries the live state) and sends every ZFS volume's net to a
// file under /srv/nobackup, the copy that survives -r. On LVM the dd-copy
// rollback spares every snapshot — the net LV IS the hatch (no file needed).
// A failed send ABORTS the rollback (before any data moves): a hatch that
// silently doesn't exist is worse than none.
func preRollbackSnapshot(exec drive.ExecFunc, volumes []drive.PlaneVolume, snaps []drive.SnapshotInfo, to string) error {
	name := drive.SnapshotName(time.Now(), "pre-rollback")
	fmt.Printf("taking the safety net %s (the pre-rollback LIVE state — what a mistaken rollback destroys)…\n", name)
	if err := drive.SnapshotCreate(exec, volumes, name); err != nil {
		return err
	}
	for _, v := range volumes {
		if v.Zfs == "" {
			continue
		}
		file := hostNetFile(name, v.Zfs)
		fmt.Printf("  exporting the net: %s → %s (survives the rollback)\n", v.Zfs+"@"+name, file)
		out, err := exec("zfs send "+v.Zfs+"@"+name+" > "+file, 1800)
		if err != nil {
			return fmt.Errorf("the net's send-file failed — rollback ABORTED before any data moved (free /srv/nobackup and re-run): %w", err)
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			return fmt.Errorf("the net's send-file failed (exit %d) — rollback ABORTED before any data moved (free /srv/nobackup and re-run)", *out.ExitCode)
		}
	}
	return nil
}

// hostNetFile is the send-file path for one dataset's net (the dataset name
// slashed to dashes — a flat file in /srv/nobackup).
func hostNetFile(name, dataset string) string {
	return "/srv/nobackup/" + strings.ReplaceAll(dataset, "/", "-") + "-" + name + ".zfs"
}

// affectedGuests maps the plane's roles to their recorded VMIDs (0 = gone —
// skipped, e.g. after a compute-only teardown).
func affectedGuests(cfg *config.Config, sourceRole map[string]string) []uint32 {
	seen := map[uint32]bool{}
	var guests []uint32
	for _, role := range sourceRole {
		if id := guestVmidOf(cfg, role); id != 0 && !seen[id] {
			seen[id] = true
			guests = append(guests, id)
		}
	}
	sort.Slice(guests, func(i, j int) bool { return guests[i] < guests[j] })
	return guests
}

// commaU32 renders ids for the log line.
func commaU32(ids []uint32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(uint64(id), 10))
	}
	return strings.Join(parts, ", ")
}

// stopAllGuests stops every affected guest and reports which ones THIS run
// actually stopped (a guest absent at stop — its VMID recorded but the
// guest destroyed — stays absent: not restarted, never a start failure).
func stopAllGuests(exec drive.ExecFunc, cfg *config.Config, sourceRole map[string]string) (stopped map[string]bool, toStart []uint32, err error) {
	stopped = map[string]bool{}
	roles := make([]string, 0, len(sourceRole))
	seen := map[string]bool{}
	for _, role := range sourceRole {
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	for _, role := range roles {
		id := guestVmidOf(cfg, role)
		if id == 0 {
			continue
		}
		wasRunning, serr := stopGuest(exec, id)
		if serr != nil {
			// A failed stop is a REAL abort: a guest whose pct stop failed
			// (busy/locked) is still RUNNING — the rollback must not fire
			// under a live writer (zfs rollback -r on a mounted dataset
			// under a live writer = corruption).
			return nil, nil, fmt.Errorf("guest %d (%s) failed to stop — the rollback ABORTS before any data moved (fix the guest, re-run): %w", id, role, serr)
		}
		if wasRunning {
			stopped[role] = true
			toStart = append(toStart, id)
		}
	}
	return stopped, toStart, nil
}

// stopGuest stops one recorded guest, classifying AFFIRMATIVELY: the status
// probe runs first — a probe failure aborts (a live guest whose state is
// unknown must never be rolled back under), a confirmed non-running or
// absent guest is a skip (stopped=false), only a confirmed running guest
// stops. The error aborts the rollback BEFORE any data moves.
func stopGuest(exec drive.ExecFunc, vmid uint32) (stopped bool, err error) {
	id := strconv.FormatUint(uint64(vmid), 10)
	status, err := exec("pct status "+id, 60)
	if err != nil {
		return false, err
	}
	if status.ExitCode != nil && *status.ExitCode != 0 {
		fmt.Printf("  · guest %s absent — skipping\n", id)
		return false, nil
	}
	if !strings.Contains(status.Stdout, "status:") {
		return false, fmt.Errorf("guest %s's status is unparseable (%q) — refusing to roll back a guest in unknown state", id, strings.TrimSpace(status.Stdout))
	}
	if !strings.Contains(status.Stdout, "status: running") {
		fmt.Printf("  · guest %s already stopped — skipping\n", id)
		return false, nil
	}
	stop, err := exec("pct stop "+id, 300)
	if err != nil {
		return false, err
	}
	if stop.ExitCode != nil && *stop.ExitCode != 0 {
		return false, fmt.Errorf("pct stop exited %d: %s", *stop.ExitCode, strings.TrimSpace(stop.Stderr))
	}
	fmt.Printf("  ✓ stopped %s\n", id)
	return true, nil
}

// startGuests starts every stopped guest and returns how many FAILED — a
// failed start never hides behind a ✓.
func startGuests(exec drive.ExecFunc, guests []uint32) int {
	failed := 0
	for _, g := range guests {
		id := strconv.FormatUint(uint64(g), 10)
		out, err := exec("pct start "+id, 300)
		if err != nil || (out.ExitCode != nil && *out.ExitCode != 0) {
			detail := ""
			if err != nil {
				detail = err.Error()
			} else {
				detail = strings.TrimSpace(out.Stderr)
			}
			fmt.Printf("  ⚠ guest %s FAILED TO START — pct start %s by hand: %s\n", id, id, detail)
			failed++
			continue
		}
		fmt.Printf("  ✓ started %s\n", id)
	}
	return failed
}

// pickSnapshot is the interactive picker — the same numbered-list pattern as
// the profile selector.
func pickSnapshot(snaps []drive.SnapshotInfo) (string, error) {
	fmt.Println("snapshots (pick one to roll back to):")
	for i, s := range snaps {
		fmt.Printf("  %d) %s\n", i+1, s.Name)
	}
	var in string
	fmt.Printf("select snapshot [1] : ")
	if _, err := fmt.Scanln(&in); err != nil || strings.TrimSpace(in) == "" {
		in = "1"
	}
	n, err := strconv.Atoi(strings.TrimSpace(in))
	if err != nil || n < 1 || n > len(snaps) {
		return "", fmt.Errorf("no snapshot %q", in)
	}
	return snaps[n-1].Name, nil
}

func init() {
	for _, c := range []*cobra.Command{snapCmd, rollbackCmd} {
		// The verb-level --config (the convention every verb carries): the
		// TUI pins the profile on BOTH its child runs (the picker fetch AND
		// the rollback dispatch) — without it NegotiateProfile's picker
		// reads stdin (a subprocess's /dev/null) and silently defaults to
		// the alphabetically first profile — the wrong tenant's plane.
		c.Flags().String("config", config.ConfigPath(), "Config path (default: the active profile's)")
		// --ssh-key overrides the box's DOOR_SPEC derivation: the CP guest's
		// verbs pass the staged cp-verb key (/srv/data/cp/verb-ssh.key).
		c.Flags().String("ssh-key", "", "SSH private key for the host door (default: this box's derived DOOR_SPEC key)")
	}
	rollbackCmd.Flags().Bool("guest", false, "revive the CP with the staged guest-local script (the verbs run ON the CP guest)")
	snapCmd.Flags().Bool("list", false, "list plane snapshots")
	snapCmd.Flags().Bool("json", false, "with --list: emit JSON (the TUI picker's source)")
	snapCmd.Flags().String("rm", "", "remove the named snapshot from every volume")
	rollbackCmd.Flags().String("to", "", "the snapshot name to roll back to (blank = pick interactively; with --yes, blank = newest)")
	rollbackCmd.Flags().Bool("yes", false, "confirm the destructive rollback (the TUI's path)")
	rollbackCmd.Flags().Bool("fresh-snapshot", true, "take a pre-rollback snapshot when none exists from today")
	snapCmd.AddCommand(rollbackCmd)
}

// Command is the cobra command root.go registers.
func Command() *cobra.Command { return snapCmd }
