// Package backup implements `freehold backup` — restic on the substrate
// host, driven over the transient DOOR_SPEC SSH path. We implement ZERO
// backends: restic's `-r` URI IS the backend (sftp: → TrueNAS, b2:/s3: →
// Backblaze). freehold owns only the plumbing — the repo password (a 0600
// file in the profile dir, pushed to the host's /srv/nobackup so it never
// rides inside a snapshot), the credentials env file (0600, --env at init),
// and the recorded plane mount sources as the backup scope. restic itself
// is resolved on the host (PATH; a clear error names it when missing).
package backup

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"freehold/providers/proxmox/export"
)

const (
	hostPasswordFile = "/srv/nobackup/freehold-restic-password"
	hostEnvFile      = "/srv/nobackup/freehold-restic.env"
)

// HostPaths is exported for the tests' assertions.
func HostPaths() (password, envFile string) { return hostPasswordFile, hostEnvFile }

// ResticCmd builds the remote restic invocation: the env file (credentials —
// B2/AWS keys) is sourced when present, the password rides RESTIC_PASSWORD_FILE,
// and the repo URI is always explicit. A 2>/dev/null on the source keeps a
// missing env file silent.
func ResticCmd(uri, args string) string {
	return ". " + hostEnvFile + " 2>/dev/null; RESTIC_PASSWORD_FILE=" + hostPasswordFile + " restic -r " + uri + " " + args
}

// ProbeCmd checks restic is on the host + whether the repo answers
// (already-initialized detection).
func ProbeCmd(uri string) string {
	return "command -v restic >/dev/null || { echo RESTIC-MISSING; exit 127; }; " + ResticCmd(uri, "snapshots --json") + " 2>/dev/null"
}

// BackupArgs is the run scope: every recorded durable-plane mount source
// (host paths), tagged so one repo can hold several worlds.
func BackupArgs(cfg *config.Config) (string, error) {
	var sources []string
	var excludes []string
	for _, mounts := range cfg.Plane.Mounts {
		for _, m := range mounts {
			if m.Source == "" {
				continue
			}
			sources = append(sources, m.Source)
			// The relay's daemon-root carve-out: its named volumes (the DBs)
			// ride INSIDE /var/lib/docker — but the storage-driver dirs (the
			// pulled images' unpacked layers, live-verified: 812M of a 1.0G
			// root) re-pull at compose-up; restic skips them like the export
			// does.
			if m.GuestPath == "/var/lib/docker" {
				for _, d := range export.DriverDirs {
					excludes = append(excludes, "--exclude", m.Source+"/"+d)
				}
			}
		}
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("no durable-plane mounts recorded — nothing to back up")
	}
	if cfg.Name == "" {
		// Pre---name worlds (domain-derived names) have nothing to tag with
		// — a bare `--tag ` fails restic's parser.
		return "backup " + strings.Join(sources, " ") + " " + hostConfigFile + " " + strings.Join(excludes, " ") + " --tag freehold", nil
	}
	return "backup " + strings.Join(sources, " ") + " " + hostConfigFile + " " + strings.Join(excludes, " ") + " --tag freehold --tag " + cfg.Name, nil
}

// LoadPassword returns the profile-local restic password, generating a
// 32-byte hex one IN MEMORY when absent (fresh=true) — the file that UNLOCKS
// the repo, so it lives on the box and on the host's /srv/nobackup, never
// inside a snapshot. A fresh password is NOT persisted here: the caller must
// settle the adopt-vs-use decision against the host FIRST (the host's copy,
// if one exists, is the repo's key — a fresh password pushed over it bricks
// the existing repo forever), and only then SavePassword. A generated-
// then-abandoned password must leave no trace — one that survives to disk
// becomes "established" on the next run and pushes itself over the real key.
func LoadPassword() (password string, fresh bool, err error) {
	path := common.FreeholdHome() + "/restic-password"
	if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), false, nil
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(raw), true, nil
}

// SavePassword persists the settled password (0600, profile dir).
func SavePassword(password string) error {
	return os.WriteFile(common.FreeholdHome()+"/restic-password", []byte(password+"\n"), 0o600)
}

// sshDownload/sshUpload are the file-transfer seams (tests override them).
var (
	sshDownload = proxmox.SSHDownload
	sshUpload   = proxmox.SSHUpload
)

// AdoptHostPassword pulls the host's password file and saves it as the
// profile-local one (the lost-profile-dir recovery: the host's copy is the
// repo's key — adopt, never overwrite).
func AdoptHostPassword(exec drive.ExecFunc, host, keyPath string) (string, error) {
	password, err := PullHostPassword(exec, host, keyPath)
	if err != nil {
		return "", err
	}
	return password, SavePassword(password)
}

// hostProbeFile is where a candidate password rides while the repo judges
// it — deliberately NOT the real password file (a wrong candidate must
// never displace the host's copy).
const hostProbeFile = "/srv/nobackup/freehold-restic-probe"

// hostConfigFile is the world's profile config shipped beside the password
// at setup: the backup's scope includes it (the restore that boots a lost
// world needs the domains/coords, and restic-only restores read THIS repo —
// the export bundle is a different flow).
const hostConfigFile = "/srv/nobackup/freehold-config.toml"

// pushSecret ships one local file to the host at dest without the destination
// ever sitting at the host's umask: the upload lands on a dest+".push" temp
// (its rm registered BEFORE the chmod, so a failed or dropped chmod still
// removes the 0644 copy), is chmodded 600, then renamed into place — the real
// path is only ever mv'd over, never written at, and a dead transport leaves
// at worst the fixed-name temp (the next push overwrites and tightens it).
func pushSecret(exec drive.ExecFunc, host, keyPath, localPath, dest string) error {
	tmp := dest + ".push"
	if _, err := proxmox.SSHUpload(host, keyPath, localPath, tmp, 300); err != nil {
		return err
	}
	defer exec("rm -f "+tmp, 30)
	if _, err := exec("chmod 600 "+tmp, 30); err != nil {
		return err
	}
	if _, err := exec("mv "+tmp+" "+dest, 30); err != nil {
		return err
	}
	return nil
}

// probeRepoPassword asks THE REPO: does it accept this password, and does
// the repo exist at all? The candidate rides a 0600 probe file (never the
// host's real one, never a command line). Classification by restic's own
// behavior: exit 0 = accepts; "unable to open config file" / "Is there a
// repository" = no repo; "wrong password" = a repo that refused; anything
// else = ambiguous → an error (fail loudly, write nothing).
func probeRepoPassword(exec drive.ExecFunc, uri, host, keyPath, candidate string) (accepts, repoExists bool, err error) {
	tmp, err := os.CreateTemp("", "freehold-restic-probe-*")
	if err != nil {
		return false, false, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(candidate); err != nil {
		return false, false, err
	}
	tmp.Close()
	if _, err = sshUpload(host, keyPath, tmp.Name(), hostProbeFile, 300); err != nil {
		return false, false, err
	}
	// The upload rides the host's default umask (0644 on a stock PVE host);
	// the candidate can be the REAL repo key (the adopt path) — the rm is
	// registered BEFORE the chmod, so a failed (or dropped) chmod still
	// removes the 0644 candidate; on success the rm -f is a no-op.
	defer exec("rm -f "+hostProbeFile, 30)
	if _, err = exec("chmod 600 "+hostProbeFile, 30); err != nil {
		return false, false, err
	}
	out, err := exec(". "+hostEnvFile+" 2>/dev/null; RESTIC_PASSWORD_FILE="+hostProbeFile+" restic -r "+uri+" snapshots --json", 300)
	if err != nil {
		return false, false, err
	}
	code := -1
	if out.ExitCode != nil {
		code = *out.ExitCode
	}
	if code == 0 {
		return true, true, nil
	}
	se := strings.ToLower(out.Stderr) + strings.ToLower(out.Stdout)
	switch {
	case strings.Contains(se, "wrong password"), strings.Contains(se, "password is incorrect"):
		return false, true, nil
	case strings.Contains(se, "unable to open config file"), strings.Contains(se, "is there a repository"), strings.Contains(se, "does not exist"):
		return false, false, nil
	}
	return false, false, fmt.Errorf("restic probe ambiguous (exit %d): %s", code, strings.TrimSpace(out.Stderr))
}

// PullHostPassword reads the HOST's password file content (trimmed) — the
// copy the repo's runs actually answered to.
func PullHostPassword(exec drive.ExecFunc, host, keyPath string) (string, error) {
	tmp, err := os.CreateTemp("", "freehold-restic-adopt-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := sshDownload(host, keyPath, hostPasswordFile, tmp.Name(), 120); err != nil {
		return "", err
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(b))
	if password == "" {
		return "", fmt.Errorf("the host's password file is empty")
	}
	return password, nil
}

// EnvFileBody renders the --env pairs as a shell-sourceable file. Single-
// quoted with the `'\”` idiom — %q is GO quoting, not shell: `$` and
// backticks pass through it unescaped, and this file is sourced AS ROOT, so
// a backticked credential would run command substitution at source time.
func EnvFileBody(pairs []string) (string, error) {
	var b strings.Builder
	for _, pair := range pairs {
		key, val, ok := strings.Cut(pair, "=")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			return "", fmt.Errorf("--env wants KEY=VALUE pairs (got %q)", pair)
		}
		fmt.Fprintf(&b, "export %s='%s'\n", key, strings.ReplaceAll(val, "'", `'\''`))
	}
	return b.String(), nil
}

// RecordRepo / RecordedRepo persist the last-initialized repo URI in the
// profile dir, so `backup run`/`snapshots` resolve it without retyping.
func RecordRepo(uri string) error {
	return os.WriteFile(common.FreeholdHome()+"/restic-repo", []byte(uri+"\n"), 0o600)
}

func RecordedRepo() string {
	b, err := os.ReadFile(common.FreeholdHome() + "/restic-repo")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveRepo prefers the flag, falls back to the recorded URI.
func resolveRepo(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if uri := RecordedRepo(); uri != "" {
		return uri, nil
	}
	return "", fmt.Errorf("no restic repo — give -r <uri> (or run `freehold backup init -r <uri>` first)")
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "restic on the substrate host — the off-site leg (the -r URI is the backend: sftp://nas/…, b2:…, s3:…, local:…)",
}

var initCmd = &cobra.Command{
	Use:   "init -r <uri> [--env KEY=VAL,KEY=VAL]",
	Short: "initialize (or adopt) the restic repo; the password + credentials land on the host's /srv/nobackup (never inside a snapshot)",
	RunE: func(cmd *cobra.Command, args []string) error {
		uri, _, _, exec, cleanup, err := setup(cmd, true)
		if err != nil {
			return err
		}
		defer cleanup()

		// Probe: restic present? repo already initialized?
		out, err := exec(ProbeCmd(uri), 300)
		if err != nil {
			return err
		}
		if strings.Contains(out.Stdout, "RESTIC-MISSING") {
			return fmt.Errorf("restic is not installed on the host (apt install restic / the static binary from restic.net)")
		}
		alreadyInit := out.ExitCode != nil && *out.ExitCode == 0
		if alreadyInit {
			fmt.Println("repo already initialized — adopting it")
			return nil
		}
		if out2, err := exec(ResticCmd(uri, "init"), 300); err != nil {
			return err
		} else if out2.ExitCode == nil || *out2.ExitCode != 0 {
			return fmt.Errorf("restic init failed:\n%s", strings.TrimSpace(out2.Stderr))
		}
		fmt.Println("repo initialized:", uri)
		return nil
	},
}

var runCmd = &cobra.Command{
	Use:   "run [-r <uri>]",
	Short: "back up every recorded durable-plane mount to the repo (tagged per world; the first run is the expensive one)",
	Long: "back up every recorded durable-plane mount to the repo (tagged per world; the first run is the expensive one).\n\n" +
		"CONSISTENCY LIMIT: restic reads LIVE files on the substrate host over\n" +
		"minutes — a service writing during the run can leave a torn copy that\n" +
		"only fails at restore time. For a consistent point take a snapshot or\n" +
		"export first (`freehold snapshot` / `freehold export`), or quiesce the\n" +
		"writing services yourself.",
	RunE: func(cmd *cobra.Command, args []string) error {
		uri, _, cfg, exec, cleanup, err := setup(cmd, false)
		if err != nil {
			return err
		}
		defer cleanup()
		bargs, err := BackupArgs(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("restic backup of the durable plane → %s…\n", uri)
		fmt.Println("  (live files — for a consistent point, `freehold snapshot` or `freehold export` first)")
		// NO deadline: the first upload is the whole plane over a WAN — a
		// fixed timeout would kill the ssh while restic keeps running
		// orphaned on the host (the timeout-kills-shell-not-descendants
		// gap) and a retry would race it.
		out, err := exec(ResticCmd(uri, bargs), 0)
		if err != nil {
			return err
		}
		if out.TimedOut {
			// SSHExec maps a killed/overrun command to TimedOut=true,
			// exit -1, empty stderr — "restic backup failed:" with no
			// reason is the report a first big-plane run would get. Name
			// the real cause + the resume.
			return fmt.Errorf("the restic run exceeded the command cap — a first run of a big plane can; the run is incremental, so re-running continues where this stopped")
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("restic backup failed:\n%s", strings.TrimSpace(out.Stderr))
		}
		fmt.Println(strings.TrimSpace(out.Stdout))
		return nil
	},
}

var snapshotsCmd = &cobra.Command{
	Use:   "snapshots [-r <uri>]",
	Short: "list the repo's snapshots (the round-trip check)",
	RunE: func(cmd *cobra.Command, args []string) error {
		uri, _, _, exec, cleanup, err := setup(cmd, false)
		if err != nil {
			return err
		}
		defer cleanup()
		out, err := exec(ResticCmd(uri, "snapshots"), 300)
		if err != nil {
			return err
		}
		if out.TimedOut {
			return fmt.Errorf("restic snapshots exceeded the command cap — the repo may be slow to answer; retry")
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("restic snapshots failed:\n%s", strings.TrimSpace(out.Stderr))
		}
		fmt.Println(out.Stdout)
		return nil
	},
}

// setup is the shared prologue: profile, transport, password push, env push,
// repo URI. withEnv gates the --env handling (init only — the env file is
// written once; later runs source it).
// setupTransport is the shared prologue: profile negotiation, config load,
// the -r record/fallback, and the door transport (this box's DOOR_SPEC key,
// or --ssh-key — the CP guest's staged cp-verb key, so the data department
// runs these verbs through cp-local-root). The credential-carrying verbs go
// on to setup()'s settle/push dance; install-timer stops here — it must NOT
// mint+push a password on a pre-init host (its guard fails loudly instead).
func setupTransport(cmd *cobra.Command) (uri string, cfg *config.Config, exec drive.ExecFunc, host, keyPath string, cleanup func(), err error) {
	ok, nerr := common.NegotiateProfile(cmd, "backup")
	if nerr != nil {
		return uri, cfg, exec, host, keyPath, cleanup, fmt.Errorf("backup: %w", nerr)
	}
	if !ok {
		return uri, cfg, exec, host, keyPath, cleanup, fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
	}
	configPath := common.ProfileConfigPath(cmd)
	cfg, err = config.Load(configPath)
	if err != nil {
		return
	}
	if cfg == nil || cfg.Host == "" {
		err = fmt.Errorf("no world recorded at %s", configPath)
		return
	}

	// The URI resolves FIRST — the password settlement's arbiter probes
	// `restic -r <uri>`, and an unresolved uri probed nothing (live finding:
	// every probe ran against an empty repo arg and misclassified).
	rflag, _ := cmd.Flags().GetString("r")
	if rflag != "" {
		if rerr := RecordRepo(rflag); rerr != nil {
			return uri, cfg, exec, host, keyPath, cleanup, rerr
		}
		uri = rflag
	} else {
		uri = RecordedRepo()
	}
	if uri == "" {
		return uri, cfg, exec, host, keyPath, cleanup, fmt.Errorf("no restic repo — give -r <uri> (sftp://nas/srv/restic/freehold, b2:bucket:path, s3:https://s3.<region>.backblazeb2.com/bucket, …)")
	}

	sshKey, _ := cmd.Flags().GetString("ssh-key")
	execRaw, kPath, cleanupFn, derr := common.DoorExecWithKey(cfg, sshKey)
	if derr != nil {
		return uri, cfg, exec, host, keyPath, cleanup, derr
	}
	keyPath = kPath
	cleanup = cleanupFn
	// The door key's temp file: the CALLER owns the success-path release
	// (defer cleanup()) and its own error paths after the nil check; the
	// failures INSIDE this function must not leak the key.
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	exec = drive.ExecFunc(execRaw)

	// Trim a root@-prefixed recorded host (SSHUpload prepends its own
	// root@), and make sure the drop dir exists before any push lands.
	host = strings.TrimPrefix(cfg.Host, "root@")
	if _, perr := exec("mkdir -p "+filepath.Dir(hostPasswordFile), 60); perr != nil {
		return uri, cfg, exec, host, keyPath, cleanup, perr
	}
	return
}

func setup(cmd *cobra.Command, withEnv bool) (uri string, envPairs []string, cfg *config.Config, exec drive.ExecFunc, cleanup func(), err error) {
	uri, cfg, exec, host, keyPath, cleanup, terr := setupTransport(cmd)
	if terr != nil {
		err = terr
		return
	}
	// From here the key's temp file is alive: every error path below must
	// release it (the refuse verdicts are the DESIGNED outcome — the key
	// must not ride them).
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	if withEnv {
		if env, eerr := cmd.Flags().GetString("env"); eerr == nil && env != "" {
			envPairs = strings.Split(env, ",")
		}
	}

	// The password: generated on the box (0600, profile dir), pushed to the
	// host's /srv/nobackup (0600) — never inside a snapshot, never on a
	// command line.
	password, fresh, perr := LoadPassword()
	if perr != nil {
		err = perr
		return
	}
	// The credentials env file pushes BEFORE the settlement: probeRepoPassword
	// sources it, so on a host that holds a password but no env file yet a
	// b2:/s3: URI could never settle (the credentials error matched neither
	// refuse signature — every verb refused and no CLI path could ever push
	// the env file). The env pairs are THIS run's operator-supplied backend
	// credentials, not the repo key — pushing them early displaces nothing
	// the settlement protects.
	if withEnv && len(envPairs) > 0 {
		body, berr := EnvFileBody(envPairs)
		if berr != nil {
			err = berr
			return
		}
		envTmp, berr := os.CreateTemp("", "freehold-restic-env-*")
		if berr != nil {
			err = berr
			return
		}
		defer os.Remove(envTmp.Name())
		if _, berr = envTmp.WriteString(body); berr != nil {
			err = berr
			return
		}
		envTmp.Close()
		if berr = pushSecret(exec, host, keyPath, envTmp.Name(), hostEnvFile); berr != nil {
			err = berr
			return
		}
	}
	// Settle the password BEFORE anything persists — and the ARBITER is the
	// REPO, not either file: a password is only the repo's key when `restic
	// snapshots` accepts it. Neither file's word is enough: the host's can
	// be a wrong push's leftovers, the box's a stale restore.
	probe, perr := exec("test -s "+hostPasswordFile, 30)
	if perr != nil {
		err = perr
		return
	}
	hostHasKey := probe.ExitCode != nil && *probe.ExitCode == 0
	// Settled by settle() below: when its push is false, the push leg is
	// skipped (the host's file already holds the working key).
	push := true
	var hostPw string
	if hostHasKey {
		hostPw, perr = PullHostPassword(exec, host, keyPath)
		if perr != nil {
			err = fmt.Errorf("the host holds a restic password this box can't read (refusing to touch it): %w", perr)
			return
		}
	}
	accepts := func(pw string) (ok, repoExists bool, perr error) {
		return probeRepoPassword(exec, uri, host, keyPath, pw)
	}
	decision, derr := settle(fresh, hostHasKey, password, hostPw, uri, accepts)
	if derr != nil {
		err = derr
		return
	}
	password = decision.password
	push = decision.push
	if decision.note != "" {
		fmt.Println(decision.note)
	}
	if decision.save {
		if perr := SavePassword(password); perr != nil {
			err = perr
			return
		}
	}
	if push {
		passFile, perr := os.CreateTemp("", "freehold-restic-pass-*")
		if perr != nil {
			err = perr
			return
		}
		defer os.Remove(passFile.Name())
		if _, perr = passFile.WriteString(password); perr != nil {
			err = perr
			return
		}
		passFile.Close()
		if perr = pushSecret(exec, host, keyPath, passFile.Name(), hostPasswordFile); perr != nil {
			err = perr
			return
		}
	}

	// The world's profile config rides alongside (the restore that boots a
	// lost world needs the domains/coords; this repo = restic-only restores
	// read it here). Pushed EVERY setup — it is the CURRENT world's desired
	// state, and a stale config here would restore wrong coords.
	if raw, rerr := os.ReadFile(common.ProfileConfigPath(cmd)); rerr == nil {
		if _, perr := exec(fmt.Sprintf("mkdir -p %s && echo %s | base64 -d > %s && chmod 600 %s",
			filepath.Dir(hostConfigFile), base64.StdEncoding.EncodeToString(raw), hostConfigFile, hostConfigFile), 60); perr != nil {
			err = perr
			return
		}
	}

	return
}

// settleDecision is the password settlement's outcome: which password is the
// repo's key and what to persist.
type settleDecision struct {
	password string
	save     bool   // SavePassword(password) — the box's copy follows the verdict
	push     bool   // overwrite the HOST's password file with password
	note     string // the operator-facing line ("" = silent)
}

// refuse is the fail-closed verdict: "no repo at this URI" is also what a
// typo'd URI, stale backend creds, or an unreachable backend produce — and a
// host-held password may be the only surviving copy of the real key.
func refuse(uri string) error {
	return fmt.Errorf("the probe says no repo at %s, but the host holds a restic password — that verdict is also what a typo'd URI or an unreachable backend produce, and the host's file may be the only surviving copy of the real key; resolve by hand (check the URI/backend, or delete the host's file deliberately and re-run); the host's password file was not touched", uri)
}

// settle chooses the working password for the repo at uri from the two
// copies — the REPO is the arbiter (`restic snapshots` accepting it), never
// either file's word. fresh = the box generated a new password (its copy is
// missing); hostHasKey = the host's /srv/nobackup file exists. accepts probes
// the repo. A refused settlement writes NOTHING anywhere.
func settle(fresh, hostHasKey bool, boxPw, hostPw, uri string, accepts func(string) (ok, repoExists bool, err error)) (settleDecision, error) {
	switch {
	case fresh && hostHasKey:
		// The box lost its copy and the host holds one: adopt it ONLY when
		// the repo accepts it — the host file can be a wrong push's
		// leftovers. When it doesn't unlock the repo and the box's fresh
		// one can't either (it can't — just generated), fail loudly: the
		// repo's key exists nowhere we can reach, and nothing was written.
		ok, repoExists, err := accepts(hostPw)
		if err != nil {
			return settleDecision{}, err
		}
		if ok {
			return settleDecision{password: hostPw, save: true, push: false,
				note: "adopted the host's restic password (the repo accepts it; the profile's copy was missing)"}, nil
		}
		if repoExists {
			return settleDecision{}, fmt.Errorf("the host's restic password does NOT unlock the repo and this box has none — resolve by hand (restic key recover from a working copy, or re-init deliberately); nothing was written")
		}
		return settleDecision{}, refuse(uri)
	case fresh:
		// No copy on the box, no file on the host — the fresh password
		// becomes the key. STILL arbiter'd: a repo that exists and rejects
		// the fresh password (a lost box AND a wiped /srv/nobackup, off-site
		// repo alive) must refuse — persisting here would establish garbage
		// in both files against a repo whose key exists nowhere we reach.
		ok, repoExists, err := accepts(boxPw)
		if err != nil {
			return settleDecision{}, err
		}
		if repoExists && !ok {
			return settleDecision{}, fmt.Errorf("a repo exists at %s and does NOT accept a fresh password — its key exists nowhere we can reach (restic key recover from a working copy, or re-init deliberately after removing the repo); nothing was written", uri)
		}
		return settleDecision{password: boxPw, save: true, push: true}, nil
	case hostHasKey:
		boxOK, boxExists, err := accepts(boxPw)
		if err != nil {
			return settleDecision{}, err
		}
		if !boxExists {
			// A deliberate re-point would start here — but "no repo at
			// this URI" is also the typo'd-URI / unreachable-backend
			// verdict, and the host's file may be the only surviving copy
			// of a real repo's key. Refuse; resolve by hand.
			return settleDecision{}, refuse(uri)
		}
		if boxOK {
			// The box's copy is the working key — pushing it over a stale
			// host file is the fix, not a hazard.
			return settleDecision{password: boxPw, save: false, push: true,
				note: "the box's restic password unlocks the repo — pushing it over the host's stale copy"}, nil
		}
		hostOK, _, err := accepts(hostPw)
		if err != nil {
			return settleDecision{}, err
		}
		if !hostOK {
			return settleDecision{}, fmt.Errorf("neither the box's nor the host's restic password unlocks the repo — resolve by hand; nothing was written")
		}
		return settleDecision{password: hostPw, save: true, push: false,
			note: "⚠ the box's restic password DIFFERS from the host's — adopting the host's copy (the repo accepts it)"}, nil
	default:
		// !fresh && !hostHasKey: the box holds a copy and the host has no
		// file — the box's copy is the only key in play; push it to the
		// host (the file's birth, not an overwrite).
		return settleDecision{password: boxPw, save: false, push: true}, nil
	}
}

var installTimerCmd = &cobra.Command{
	Use:   "install-timer [--at 04:00] [--verify-at 'Sun *-*-* 05:00:00'] [-r <uri>]",
	Short: "install the host-side systemd timers (nightly backup + weekly repo check), rendered from this verb's own restic line — re-run after plane/profile changes",
	Long: "install the host-side systemd timers (nightly backup + weekly repo check), rendered from this verb's own restic line — re-run after plane/profile changes.\n\n" +
		"The scripts embed the exact restic command `freehold backup run` builds\n" +
		"(BackupArgs) — the timer is a render, never a fork: when the plane's\n" +
		"mounts or the verb's scope change, re-run this verb and the host's\n" +
		"timer follows. The scripts carry no credentials — they reference the\n" +
		"0600 files `backup init` pushed and fail loudly (named reason) until\n" +
		"those exist. Runs on the PVE host, so the 04:00 push needs neither\n" +
		"the box nor the CP. Works through --ssh-key too (the data department\n" +
		"re-renders it via its cp-local-root door).",
	RunE: func(cmd *cobra.Command, args []string) error {
		uri, cfg, exec, host, _, cleanup, err := setupTransport(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		bargs, err := BackupArgs(cfg)
		if err != nil {
			return err
		}
		at, _ := cmd.Flags().GetString("at")
		verifyAt, _ := cmd.Flags().GetString("verify-at")
		if !atRe.MatchString(at) {
			return fmt.Errorf("--at wants HH:MM (the host's local clock), got %q", at)
		}
		files := []struct{ dest, content, mode string }{
			{"/srv/nobackup/freehold-backup.sh", renderBackupScript(uri, bargs), "700"},
			{"/srv/nobackup/freehold-backup-verify.sh", renderVerifyScript(uri), "700"},
			{"/etc/systemd/system/freehold-backup.service", renderService("freehold restic backup of the durable plane (generated by freehold backup install-timer — do not edit; re-run the verb)", "/srv/nobackup/freehold-backup.sh"), "644"},
			{"/etc/systemd/system/freehold-backup.timer", renderTimer("nightly freehold restic backup of the durable plane", "*-*-* "+at+":00"), "644"},
			{"/etc/systemd/system/freehold-backup-verify.service", renderService("freehold restic repo round-trip check (generated by freehold backup install-timer)", "/srv/nobackup/freehold-backup-verify.sh"), "644"},
			{"/etc/systemd/system/freehold-backup-verify.timer", renderTimer("weekly freehold restic repo round-trip check", verifyAt), "644"},
		}
		for _, f := range files {
			if perr := pushFileOverExec(exec, f.content, f.dest, f.mode); perr != nil {
				return fmt.Errorf("%s: %w", f.dest, perr)
			}
		}
		if out, eerr := exec("systemctl daemon-reload && systemctl enable --now freehold-backup.timer freehold-backup-verify.timer", 60); eerr != nil {
			return fmt.Errorf("enabling the timers failed: %w", eerr)
		} else if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("enabling the timers failed:\n%s", strings.TrimSpace(out.Stderr))
		}
		fmt.Println("installed on " + host + ": freehold-backup.timer (daily " + at + " host-local) + freehold-backup-verify.timer (repo round-trip, " + verifyAt + "), Persistent")
		fmt.Println("  scope = the backup verb's own: " + strings.TrimSpace(bargs))
		out, lerr := exec("systemctl list-timers freehold-backup.timer freehold-backup-verify.timer --no-pager", 30)
		if lerr == nil && out.ExitCode != nil && *out.ExitCode == 0 {
			fmt.Println(strings.TrimSpace(out.Stdout))
		}
		return nil
	},
}

var atRe = regexp.MustCompile(`^\d{1,2}:\d{2}$`)

// pushFileOverExec ships one generated file to the host over the exec
// channel (base64 — no SSHUpload key juggling; the same pattern the profile
// config push uses). The contents here are never secrets: they reference
// the credential paths, they don't contain them.
func pushFileOverExec(exec drive.ExecFunc, content, dest, mode string) error {
	if _, err := exec(fmt.Sprintf("mkdir -p %s && echo %s | base64 -d > %s && chmod %s %s",
		filepath.Dir(dest), base64.StdEncoding.EncodeToString([]byte(content)), dest, mode, dest), 60); err != nil {
		return err
	}
	return nil
}

// renderBackupScript embeds the backup verb's OWN restic line (ResticCmd of
// BackupArgs) — the timer renders the verb, never forks it. No set -e: the
// guards exit explicitly with named reasons, and the last line is restic's
// own (its exit code becomes the unit's result). The `. env 2>/dev/null`
// prefix (ResticCmd) is silent-but-continuing here — a repo whose backend
// needs credentials fails with restic's own named error otherwise.
func renderBackupScript(uri, args string) string {
	return `#!/bin/sh
# Generated by 'freehold backup install-timer' — the backup verb's own restic
# line; do not hand-edit. Re-run the verb after plane/profile changes and the
# timer follows.
command -v restic >/dev/null || { echo "freehold-backup: restic is not installed on this host (apt install restic / the static binary from restic.net)" >&2; exit 1; }
[ -r ` + hostPasswordFile + ` ] || { echo "freehold-backup: no ` + hostPasswordFile + ` on this host — run 'freehold backup init' (from the box) first" >&2; exit 1; }
` + ResticCmd(uri, args) + `
`
}

// renderVerifyScript is the weekly round-trip: does the repo answer, and
// does it hold snapshots? restic's stderr rides into the failure message.
// The env file is sourced (ResticCmd's own prefix) — restic cannot reach a
// b2:/s3: repo without its backend credentials, and those live in the env
// file, not the password file.
func renderVerifyScript(uri string) string {
	return `#!/bin/sh
# Generated by 'freehold backup install-timer' — the weekly repo round-trip
# check: the repo answers and holds snapshots.
command -v restic >/dev/null || { echo "freehold-backup-verify: restic is not installed on this host" >&2; exit 1; }
[ -r ` + hostPasswordFile + ` ] || { echo "freehold-backup-verify: no ` + hostPasswordFile + ` on this host — run 'freehold backup init' (from the box) first" >&2; exit 1; }
out=$(` + ResticCmd(uri, "snapshots --json") + ` 2>&1) || { echo "freehold-backup-verify: the repo did not answer: $out" >&2; exit 1; }
echo "$out" | grep -q '"time"' || { echo "freehold-backup-verify: the repo answered but holds no snapshots: $out" >&2; exit 1; }
`
}

func renderService(desc, script string) string {
	return `[Unit]
Description=` + desc + `

[Service]
Type=oneshot
ExecStart=` + script + `
`
}

func renderTimer(desc, onCalendar string) string {
	return `[Unit]
Description=` + desc + `

[Timer]
OnCalendar=` + onCalendar + `
Persistent=true

[Install]
WantedBy=timers.target
`
}

func init() {
	for _, c := range []*cobra.Command{initCmd, runCmd, snapshotsCmd, installTimerCmd} {
		c.Flags().StringP("r", "r", "", "the restic repo URI (init records it; the others fall back to the recorded one)")
		// The verb-level --config (the convention every verb carries — on a
		// box with several profiles the picker reads a subprocess's /dev/null
		// and silently defaults to the alphabetically first: the WRONG
		// tenant's plane, backed up to the wrong repo).
		c.Flags().String("config", config.ConfigPath(), "Config path (default: the active profile's)")
		// The CP guest's verbs pass the staged cp-verb key
		// (/srv/data/cp/verb-ssh.key) — the data department runs the backup
		// through its cp-local-root door, the same shape snapshot/export got.
		c.Flags().String("ssh-key", "", "SSH private key for the host door (default: this box's derived DOOR_SPEC key)")
	}
	initCmd.Flags().String("env", "", "credentials for the host env file, KEY=VAL,KEY=VAL (e.g. B2_ACCOUNT_ID=…,B2_ACCOUNT_KEY=… / AWS_*)")
	installTimerCmd.Flags().String("at", "04:00", "the nightly backup time, HH:MM on the host's local clock")
	installTimerCmd.Flags().String("verify-at", "Sun *-*-* 05:00:00", "the weekly repo-check timer, a systemd OnCalendar expression")
	backupCmd.AddCommand(initCmd, runCmd, snapshotsCmd, installTimerCmd)
}

// Command is the cobra command root.go registers.
func Command() *cobra.Command { return backupCmd }
