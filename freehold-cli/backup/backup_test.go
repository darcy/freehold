package backup

import (
	"os"
	"strings"
	"testing"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/providers/proxmox/drive"
)

func TestResticCmdPlumbsPasswordAndEnv(t *testing.T) {
	got := ResticCmd("b2:fh-bucket:/repo", "init")
	if !strings.HasPrefix(got, ". /srv/nobackup/freehold-restic.env 2>/dev/null; ") {
		t.Errorf("env file must be sourced first: %q", got)
	}
	if !strings.Contains(got, "RESTIC_PASSWORD_FILE=/srv/nobackup/freehold-restic-password") {
		t.Errorf("password file not plumbed: %q", got)
	}
	if !strings.Contains(got, "restic -r b2:fh-bucket:/repo init") {
		t.Errorf("repo/cmd not explicit: %q", got)
	}
}

func TestProbeCmdDetectsMissingBinary(t *testing.T) {
	if !strings.Contains(ProbeCmd("sftp://nas/srv/restic"), "command -v restic") {
		t.Error("the probe must check the binary exists first")
	}
}

func TestBackupArgsScopeIsTheRecordedPlane(t *testing.T) {
	cfg := &config.Config{Name: "world", Plane: config.PlaneSpec{
		Mounts: map[string][]config.PlaneMount{
			"relay": {{Source: "/plane/relay", GuestPath: "/srv/data/relay"}},
			"cp":    {{Source: "/plane/cp", GuestPath: "/srv/data/cp"}},
		},
	}}
	got, err := BackupArgs(cfg)
	if err != nil {
		t.Fatalf("backup args: %v", err)
	}
	for _, want := range []string{"/plane/cp", "/plane/relay", "--tag freehold", "--tag world"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if _, err := BackupArgs(&config.Config{}); err == nil {
		t.Error("no mounts must error")
	}
}

func TestEnvFileBodyQuotesPairs(t *testing.T) {
	// Single-quoted with the '\'' idiom: $ and backticks stay LITERAL (the
	// file is sourced as root — a backtick must never run).
	body, err := EnvFileBody([]string{"B2_ACCOUNT_ID=abc", "B2_ACCOUNT_KEY=se'cret", "A_KEY=with$dollar`tick"})
	if err != nil {
		t.Fatalf("env body: %v", err)
	}
	if !strings.Contains(body, "export B2_ACCOUNT_ID='abc'") {
		t.Errorf("plain value wrong:\n%s", body)
	}
	if !strings.Contains(body, `export B2_ACCOUNT_KEY='se'\''cret'`) {
		t.Errorf("quote escaping wrong:\n%s", body)
	}
	if !strings.Contains(body, "export A_KEY='with$dollar`tick'") {
		t.Errorf("$ and backticks must stay literal:\n%s", body)
	}
	if _, err := EnvFileBody([]string{"noequals"}); err == nil {
		t.Error("a pair without = must error")
	}
}

// TestLoadPasswordFreshUntilSaved: a generated password is NOT persisted
// (an abandoned generate must leave no trace — one that survives to disk
// becomes "established" and pushes itself over the host's real key on the
// next run); SavePassword settles it, and after that it's stable.
func TestLoadPasswordFreshUntilSaved(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	p1, fresh1, err := LoadPassword()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(p1) != 64 || !fresh1 {
		t.Errorf("a fresh dir must generate (64-hex, fresh=true): %d chars, fresh=%v", len(p1), fresh1)
	}
	if _, err := os.Stat(common.FreeholdHome() + "/restic-password"); err == nil {
		t.Error("a fresh generate must not persist anything")
	}
	p2, fresh2, err := LoadPassword()
	if err != nil || !fresh2 || p2 == p1 {
		t.Errorf("an unsaved generate must generate AGAIN: %q vs %q (fresh=%v)", p1, p2, fresh2)
	}
	if err := SavePassword(p2); err != nil {
		t.Fatalf("save: %v", err)
	}
	p3, fresh3, err := LoadPassword()
	if err != nil || fresh3 || p3 != p2 {
		t.Errorf("after SavePassword the password is stable: %q vs %q (fresh=%v)", p2, p3, fresh3)
	}
}

// TestAdoptHostPasswordPrefersTheHostCopy: a box that lost its profile dir
// must ADOPT the host's password (the repo's key), never push a fresh one
// over it — the bricked-repo scenario.
func TestAdoptHostPasswordPrefersTheHostCopy(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	oldDL := sshDownload
	sshDownload = func(host, keyPath, remotePath, localPath string, timeoutS uint64) (uint64, error) {
		return 11, os.WriteFile(localPath, []byte("thehostkey\n"), 0o600)
	}
	defer func() { sshDownload = oldDL }()

	got, err := AdoptHostPassword(nil, "host", "/dev/null")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got != "thehostkey" {
		t.Errorf("adopted %q", got)
	}
	local, _ := os.ReadFile(common.FreeholdHome() + "/restic-password")
	if strings.TrimSpace(string(local)) != "thehostkey" {
		t.Errorf("the profile copy must be the adopted key: %q", local)
	}
}

func TestRepoRecordingRoundTrip(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	if got := RecordedRepo(); got != "" {
		t.Errorf("no repo recorded yet, got %q", got)
	}
	if err := RecordRepo("sftp://nas/srv/restic"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := RecordedRepo(); got != "sftp://nas/srv/restic" {
		t.Errorf("round trip: %q", got)
	}
	if got, err := resolveRepo(""); err != nil || got != "sftp://nas/srv/restic" {
		t.Errorf("resolveRepo fallback: %q %v", got, err)
	}
	if got, _ := resolveRepo("b2:x:y"); got != "b2:x:y" {
		t.Errorf("the flag must win: %q", got)
	}
}

// TestProbeCommandCarriesTheResolvedURI: the arbiter's probe runs `restic
// -r <uri>` — an empty uri probed nothing and misclassified every repo
// (live finding): the probe command must carry the resolved repo.
func TestProbeCommandCarriesTheResolvedURI(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	oldDL := sshDownload
	sshDownload = func(host, keyPath, remotePath, localPath string, timeoutS uint64) (uint64, error) {
		return 11, os.WriteFile(localPath, []byte("hostkey\n"), 0o600)
	}
	defer func() { sshDownload = oldDL }()
	oldUL := sshUpload
	sshUpload = func(host, keyPath, localPath, remotePath string, timeoutS uint64) (uint64, error) {
		return 11, nil
	}
	defer func() { sshUpload = oldUL }()

	var probeCmd string
	exec := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		if strings.Contains(cmd, "restic") && strings.Contains(cmd, "snapshots") {
			probeCmd = cmd
		}
		code := 0
		out := ""
		// A no-repo host: the password probe's restic fails with the
		// no-repo signature; the file checks stay exit 0.
		if strings.Contains(cmd, "restic") && strings.Contains(cmd, "snapshots") {
			code = 1
			out = "Fatal: unable to open config file: is there a repository at the following location?"
		}
		return &client.ExecOutcome{Stdout: out, Stderr: out, ExitCode: &code}, nil
	})
	ok, repoExists, err := probeRepoPassword(exec, "sftp://nas/srv/restic", "host", "/dev/null", "cand")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if ok || repoExists {
		t.Errorf("a stubbed no-repo host must read (false, false), got (%v, %v)", ok, repoExists)
	}
	if !strings.Contains(probeCmd, "-r sftp://nas/srv/restic") {
		t.Errorf("the probe must carry the RESOLVED uri, got: %q", probeCmd)
	}
	if !strings.Contains(probeCmd, "RESTIC_PASSWORD_FILE="+hostProbeFile) {
		t.Errorf("the probe must use its own candidate file, not the host's: %q", probeCmd)
	}
}

// TestBackupArgsExcludeDriverDirs: the relay's daemon-root carve-out keeps
// the DBs in but skips the pulled images' unpacked layers (812M of a 1.0G
// root live-verified) — restic --excludes the storage-driver dirs, same as
// the export's tar.
func TestBackupArgsExcludeDriverDirs(t *testing.T) {
	cfg := &config.Config{Name: "world", Plane: config.PlaneSpec{
		Mounts: map[string][]config.PlaneMount{
			"relay": {
				{Source: "/plane/docker-root", GuestPath: "/var/lib/docker"},
				{Source: "/plane/deploy", GuestPath: "/srv/data/relay"},
			},
		},
	}}
	got, err := BackupArgs(cfg)
	if err != nil {
		t.Fatalf("backup args: %v", err)
	}
	for _, want := range []string{
		"--exclude /plane/docker-root/fuse-overlayfs",
		"--exclude /plane/docker-root/overlay2",
		"/plane/deploy", // the non-docker mounts ride untouched
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
