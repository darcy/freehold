// The full provisioning drivers (Rust bootstrap.rs): proxmox-lxc
// (template ensure + pct create/start/verify + guest docker), vultr-vps and
// hetzner-vps curl drivers, and the A4 domain gate. Commands are executed
// through the runner's ONE exec primitive; operator values are Plain/
// PlainPath-guarded before interpolation.
package bootstrap

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/planebase"
)

// TargetKind is the bootstrap driver family.
type TargetKind int

const (
	KindProxmoxLxc TargetKind = iota
	KindVultrVps
)

func (k TargetKind) String() string {
	switch k {
	case KindProxmoxLxc:
		return "proxmox-lxc"
	default:
		return "vultr-vps"
	}
}

// ProxmoxLxcSpec is the proxmox-lxc driver's input.
type ProxmoxLxcSpec struct {
	Hostname string
	// VMID: nil = the driver picks the lowest free id >= 100
	// (pvesh get /cluster/nextid).
	VMID *uint32
	// Template name (e.g. debian-12-standard_12.7-1_amd64.tar.zst). If nil,
	// the driver ensures a Debian template matching the HOST arch.
	Template *string
	Storage  string
	// Rootfs size in GB on `storage` (the relay stack needs room for images;
	// the pct default of 4G is too tight).
	RootfsGB uint32
	// RAM in MB (the compose stack OOMs on the pct default of 512).
	MemoryMB uint32
	Bridge   string
	// Optional STATIC guest IP (CIDR) + gateway on `bridge` — for
	// Proxmox-on-Cloud-Compute hosts where the cloud DHCP won't lease to
	// LXC veths. Nil = dhcp (the LAN/home default).
	NetIP *string
	NetGW *string
	// Durable-plane reference MOUNTS baked into pct create (--mpN): each
	// dataset is bound to its guest path at FIRST creation — the locked
	// "born on the plane, never pct set post-hoc" rule. Empty = no mounts.
	Mounts []planebase.MountSpec
}

// VultrVpsSpec is the vultr-vps driver's input.
type VultrVpsSpec struct {
	Label        string
	Region       string
	Plan         string
	OsID         uint32
	DestroyAfter bool
}

// HetznerVpsSpec is the hetzner-vps driver's input.
type HetznerVpsSpec struct {
	Label        string
	Location     string
	ServerType   string
	Image        string
	DestroyAfter bool
}

// BootstrapResult is the driver's report.
type BootstrapResult struct {
	Kind TargetKind
	ID   string
	Name string
	// The target's first global IPv4, when the driver can learn it (the
	// A4 domain gate requires it so the DOMAIN — never the IP — is verified).
	IP     string // "" when unknown
	Detail string
}

// LxcMpArgs builds the LXC `--mpN` args: each MountSpec becomes
// `--mpN=<source>,mp=<guest>,backup=<0|1>` (the planebase backup rule).
func LxcMpArgs(specs []planebase.MountSpec) []string {
	out := make([]string, 0, len(specs))
	for i, m := range specs {
		out = append(out, fmt.Sprintf("--mp%d=%s,mp=%s,backup=%d", i, m.Source, m.GuestPath, planebase.BackupFlag(m.GuestPath)))
	}
	return out
}

// ParseMount parses a `<dataset>:<guest-path>` mount reference.
func ParseMount(s string) (planebase.MountSpec, error) {
	src, guest, ok := strings.Cut(s, ":")
	if !ok || src == "" || guest == "" {
		return planebase.MountSpec{}, fmt.Errorf("mount %q must be <dataset>:<guest-path>", s)
	}
	if err := PlainPath(src); err != nil {
		return planebase.MountSpec{}, err
	}
	if err := PlainPath(guest); err != nil {
		return planebase.MountSpec{}, err
	}
	return planebase.MountSpec{Source: src, GuestPath: guest}, nil
}

// HostArch maps the host arch to the template arch suffix. `uname -m` on
// PVE: x86_64 or aarch64 (arm64 alias included for safety). Anything else
// fails closed.
func HostArch(c *client.McpClient, target string) (string, error) {
	out, err := Exec(c, target, "uname -m", 120)
	if err != nil {
		return "", err
	}
	if err := ExpectOK(out, "uname -m"); err != nil {
		return "", err
	}
	switch strings.TrimSpace(out.Stdout) {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported host arch %q (expected x86_64 or aarch64)", strings.TrimSpace(out.Stdout))
	}
}

// EnsureDebianTemplate ensures a Debian LXC template exists in the PVE
// `local` directory store, downloading the newest available one when
// missing. Idempotent: a present template is reused, and `pveam update` is
// only run when the local store has none. The template store is `local`
// (not the container's `storage`): `pct create` addresses templates as
// `local:vztmpl/<name>`. pvesm/pveam take NO --output-format (verified
// against PVE 9.2) — plain tables are parsed.
func EnsureDebianTemplate(c *client.McpClient, target string, specTemplate *string) (string, error) {
	const templateStorage = "local"
	// Operator pinned a template: use it verbatim, no download.
	if specTemplate != nil {
		if err := Plain(*specTemplate); err != nil {
			return "", err
		}
		return *specTemplate, nil
	}
	arch, err := HostArch(c, target)
	if err != nil {
		return "", err
	}

	// pvesm plain table: header + rows of `<volid> <format> <type> <size> <vmid>`.
	list, err := Exec(c, target, "pvesm list "+templateStorage, 120)
	if err != nil {
		return "", err
	}
	if err := ExpectOK(list, "template list"); err != nil {
		return "", err
	}
	// `_<arch>.tar.[gz|zst]` — the catalog and store mix amd64 and arm64
	// rows for the SAME release; an arm64 guest cannot spawn on x86_64
	// (observed live). Only the host's arch is a candidate.
	archSuffix := "_" + arch + ".tar."
	var bestName string
	var bestVer []uint32
	for _, line := range strings.Split(list.Stdout, "\n")[1:] { // skip header
		volid := firstField(line)
		if volid == "" {
			continue
		}
		name, ok := strings.CutPrefix(volid, templateStorage+":vztmpl/")
		if !ok {
			continue
		}
		if !strings.Contains(name, "debian-") || !strings.Contains(name, "standard_") || !strings.Contains(name, archSuffix) {
			continue
		}
		if ver := TemplateVersion(name); ver != nil && verCmp(ver, bestVer) > 0 {
			bestVer, bestName = ver, name
		}
	}
	if bestName != "" {
		if err := Plain(bestName); err != nil {
			return "", err
		}
		return bestName, nil
	}

	// None in the store: sync the catalog and download the newest Debian
	// standard template. Update runs first on purpose — the local catalog
	// may predate the template the user wants.
	update, err := Exec(c, target, "pveam update", 180)
	if err != nil {
		return "", err
	}
	if err := ExpectOK(update, "pveam update"); err != nil {
		return "", err
	}
	available, err := Exec(c, target, "pveam available --section system", 120)
	if err != nil {
		return "", err
	}
	if err := ExpectOK(available, "pveam available"); err != nil {
		return "", err
	}
	// Columns: `<section> <template> <size> <needs-reboot>` — the FIRST
	// token is the section (`system`), the template name is the SECOND.
	bestName, bestVer = "", nil
	for _, line := range strings.Split(available.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[1]
		if !strings.Contains(name, "debian-") || !strings.Contains(name, "standard_") {
			continue
		}
		if !strings.HasSuffix(name, ".tar.zst") && !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		if !strings.Contains(name, archSuffix) {
			continue
		}
		if ver := TemplateVersion(name); ver != nil && verCmp(ver, bestVer) > 0 {
			bestVer, bestName = ver, name
		}
	}
	if bestName == "" {
		return "", fmt.Errorf("no Debian LXC template available — run `pveam update` on the PVE host and check its internet access")
	}
	// The name came from the host's own catalog, not the operator, but it
	// still lands in a shell command — same guard as every other value.
	if err := Plain(bestName); err != nil {
		return "", err
	}
	download, err := Exec(c, target, "pveam download "+templateStorage+" "+bestName, 600)
	if err != nil {
		return "", err
	}
	if err := ExpectOK(download, "pveam download"); err != nil {
		return "", err
	}
	return bestName, nil
}

func firstField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return "" // blank/whitespace-only line (the output's trailing newline)
	}
	return strings.TrimSpace(f[0])
}

// verCmp compares two numeric template versions component-wise.
func verCmp(a, b []uint32) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// BootstrapProxmoxLxc is the proxmox-lxc driver: pct create + pct start +
// pct exec verify, with idempotent template ensurement and guest docker.
func BootstrapProxmoxLxc(c *client.McpClient, target string, spec *ProxmoxLxcSpec) (*BootstrapResult, error) {
	// Template ensurement: reuse the newest Debian template already in the
	// store; only when none is present, sync the catalog (pveam update) and
	// download the newest available. The template MUST match the HOST arch —
	// the pveam catalog mixes amd64/arm64 rows and an arm64 guest cannot
	// spawn on x86_64 (observed live). Idempotent — a re-run with a template
	// present makes NO pveam calls.
	tpl, err := EnsureDebianTemplate(c, target, spec.Template)
	if err != nil {
		return nil, err
	}

	if err := Plain(spec.Hostname); err != nil {
		return nil, err
	}
	if spec.Template != nil {
		if err := Plain(*spec.Template); err != nil {
			return nil, err
		}
	}
	if err := Plain(spec.Storage); err != nil {
		return nil, err
	}
	if err := Plain(spec.Bridge); err != nil {
		return nil, err
	}

	// Explicit vmid must be in the PVE system range; an omitted one is
	// picked cluster-wide (pvesh get /cluster/nextid) — and a same-name
	// container is REFUSED, never duplicated.
	var vmid uint32
	if spec.VMID != nil {
		if *spec.VMID < 100 {
			return nil, fmt.Errorf("vmid %d is below the PVE system range (100+); pick a free id", *spec.VMID)
		}
		vmid = *spec.VMID
	} else {
		vmid, err = PickFreeVMID(c, target, spec.Hostname)
		if err != nil {
			return nil, err
		}
	}

	// Idempotent create — re-runs (and the installer's re-run after a
	// partial bring-up) must not fail on an existing guest. If the vmid
	// ALREADY exists with OUR hostname, reuse it; a foreign container is
	// refused outright.
	reuse := false
	cfg, err := Exec(c, target, "pct config "+strconv.FormatUint(uint64(vmid), 10), 30)
	if err == nil && cfg.ExitCode != nil && *cfg.ExitCode == 0 {
		name := ""
		for _, l := range strings.Split(cfg.Stdout, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "hostname: "); ok {
				name = strings.TrimSpace(v)
				break
			}
		}
		if name == spec.Hostname {
			reuse = true
		} else {
			return nil, fmt.Errorf("vmid %d already exists as container %q (expected %q) — destroy it on the host or pick a different --vmid", vmid, name, spec.Hostname)
		}
	}

	// --unprivileged 1 is explicit, not the CLI default: pct's CLI defaults
	// to PRIVILEGED, and this container will host the relay + control plane.
	// --features nesting=1 lets dockerd (overlay2) run inside; keyctl=1 is
	// required alongside for docker itself; fuse=1 exposes /dev/fuse, the
	// fuse-overlayfs fallback path. The durable-plane reference mounts
	// (--mpN) are baked into FIRST creation only — never applied post-hoc
	// to a reused guest (the locked born-at-create rule).
	mpArgs := LxcMpArgs(spec.Mounts)
	mp := ""
	if len(mpArgs) > 0 {
		mp = " " + strings.Join(mpArgs, " ")
	}
	net0 := fmt.Sprintf("name=eth0,bridge=%s,ip=dhcp,type=veth", spec.Bridge)
	if spec.NetIP != nil && spec.NetGW != nil {
		net0 = fmt.Sprintf("name=eth0,bridge=%s,ip=%s,gw=%s,type=veth", spec.Bridge, *spec.NetIP, *spec.NetGW)
	}
	create := fmt.Sprintf(
		"pct create %d local:vztmpl/%s --rootfs %s:%d --memory %d --hostname %s --unprivileged 1 --features fuse=1,keyctl=1,nesting=1 --net0 %s%s",
		vmid, tpl, spec.Storage, spec.RootfsGB, spec.MemoryMB, spec.Hostname, net0, mp,
	)
	if !reuse {
		out, err := Exec(c, target, create, 120)
		if err != nil {
			return nil, err
		}
		if err := ExpectOK(out, "pct create"); err != nil {
			return nil, err
		}
	}

	vid := strconv.FormatUint(uint64(vmid), 10)
	status, err := Exec(c, target, "pct status "+vid, 30)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(status.Stdout, "status: running") {
		out, err := Exec(c, target, "pct start "+vid, 120)
		if err != nil {
			return nil, err
		}
		if err := ExpectOK(out, "pct start"); err != nil {
			return nil, err
		}
	}

	// A3 — reachability self-check: the runner asks the guest, through the
	// host, `pct exec`; the guest answers. No IP guessing.
	out, err := Exec(c, target, fmt.Sprintf("pct exec %s -- sh -c 'hostname && uname -s && whoami'", vid), 120)
	if err != nil {
		return nil, err
	}
	if err := ExpectOK(out, "pct exec verify"); err != nil {
		return nil, err
	}
	if !strings.Contains(out.Stdout, "Linux") || !strings.Contains(out.Stdout, spec.Hostname) {
		return nil, fmt.Errorf("guest did not answer as expected (hostname %s): %s", spec.Hostname, out.Stdout)
	}

	// Get ahead of docker-in-LXC: the guest hosts the relay, so it needs
	// docker + compose BEFORE deploy's docker gate runs.
	if err := EnsureGuestDocker(c, target, vmid); err != nil {
		return nil, err
	}

	// A4 support — the domain gate needs the guest's IP so the install can
	// require the DOMAIN to resolve to it (the IP is never the identity).
	// `pct exec <vmid> -- ip -4 -o addr` needs no shell quoting.
	ip := ""
	if ipOut, err := Exec(c, target, fmt.Sprintf("pct exec %s -- ip -4 -o addr", vid), 60); err == nil {
		fields := strings.Fields(ipOut.Stdout)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "inet" && !strings.HasPrefix(fields[i+1], "127.") {
				ip = strings.SplitN(fields[i+1], "/", 2)[0]
				break
			}
		}
	}

	kernel := "?"
	lines := strings.Split(out.Stdout, "\n")
	if len(lines) > 1 {
		kernel = lines[1]
	}
	return &BootstrapResult{
		Kind: KindProxmoxLxc,
		ID:   vid,
		Name: spec.Hostname,
		IP:   ip,
		Detail: fmt.Sprintf(
			"lxc %d (%s) created from %s on %s (%dG rootfs), started, and the guest verified via `pct exec` (kernel %s); docker+compose ready",
			vmid, spec.Hostname, tpl, spec.Storage, spec.RootfsGB, kernel),
	}, nil
}

// PickFreeVMID picks a vmid when the operator omitted --vmid. NAMES are
// unique on the box — a second bootstrap with the same hostname is refused.
// The vmid namespace is SHARED with QEMU VMs, which pct list misses — so
// the id comes from `pvesh get /cluster/nextid` (canonical cluster-wide
// next free id), not a pct-only scan.
func PickFreeVMID(c *client.McpClient, target, hostname string) (uint32, error) {
	list, err := Exec(c, target, "pct list", 120)
	if err != nil {
		return 0, err
	}
	if err := ExpectOK(list, "pct list"); err != nil {
		return 0, err
	}
	// The name is the LAST token: the Lock column is blank for an unlocked
	// container, so the normal row is only three fields.
	for _, line := range strings.Split(list.Stdout, "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[len(fields)-1] == hostname {
			return 0, fmt.Errorf("a container named %q already exists on this host; pass --vmid to target it, or pick a different name", hostname)
		}
	}
	next, err := Exec(c, target, "pvesh get /cluster/nextid", 120)
	if err != nil {
		return 0, err
	}
	if err := ExpectOK(next, "cluster nextid"); err != nil {
		return 0, err
	}
	vmid64, err := strconv.ParseUint(strings.TrimSpace(next.Stdout), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("pvesh nextid did not yield a number: %q", next.Stdout)
	}
	if vmid64 < 100 {
		return 0, fmt.Errorf("pvesh nextid returned a vmid below the PVE system range: %d", vmid64)
	}
	return uint32(vmid64), nil
}

// EnsureGuestDocker ensures docker + compose run inside the guest.
// Install-if-missing is one pct exec (baked to skip when already present —
// idempotent re-runs). The daemon must then answer `docker info`; when the
// default overlay2 driver fails inside the unprivileged container, fall
// back to fuse-overlayfs (the canonical fix), restart, and re-verify.
func EnsureGuestDocker(c *client.McpClient, target string, vmid uint32) error {
	vid := strconv.FormatUint(uint64(vmid), 10)
	// Docker + compose v2: debian-13/trixie carries docker-compose-v2 in
	// main, debian-12/bookworm does NOT — when the Debian name is
	// unavailable, fall back to download.docker.com's docker-compose-plugin.
	// Retried 3x because pct start may succeed before the guest has a DHCP
	// lease — apt against no network fails on the first attempt.
	install := `pct exec ` + vid + ` -- sh -c 'export DEBIAN_FRONTEND=noninteractive; ` +
		`if ! docker compose version >/dev/null 2>&1; then ` +
		`apt-get update >/dev/null 2>&1; ` +
		`if ! apt-get install -y docker.io docker-compose-v2 >/dev/null; then ` +
		`apt-get install -y curl gpg >/dev/null && ` +
		`curl -fsSL https://download.docker.com/linux/debian/gpg | ` +
		`gpg --batch --yes --dearmor -o /usr/share/keyrings/docker.gpg && ` +
		`echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker.gpg] ` +
		`https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) ` +
		`stable" > /etc/apt/sources.list.d/docker.list && ` +
		`apt-get update >/dev/null 2>&1 && ` +
		`apt-get install -y docker.io docker-compose-plugin >/dev/null; fi; fi; ` +
		`docker compose version'`

	var installErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := Exec(c, target, install, 600)
		if err == nil {
			err = ExpectOK(out, "guest docker install")
		}
		if err == nil {
			installErr = nil
			break
		}
		installErr = err
		// escalating: fast retry, then a longer wait — a slow DHCP lease
		// needs the grace, a transient apt hiccup doesn't want a long sit.
		if attempt < 2 {
			wait := 2
			if attempt == 1 {
				wait = 6
			}
			time.Sleep(time.Duration(wait) * time.Second)
		}
	}
	if installErr != nil {
		return fmt.Errorf("guest docker/compose install failed after 3 attempts (the guest may still be getting its DHCP lease): %w", installErr)
	}

	info := fmt.Sprintf("pct exec %s -- docker info", vid)
	// A non-zero exit arrives as Ok(outcome), so BOTH a transport error and
	// a failing exec must fall through to the overlay fallback.
	var first error
	if out, err := Exec(c, target, info, 120); err != nil {
		first = err
	} else if e := ExpectOK(out, "guest docker info"); e != nil {
		first = e
	} else if strings.Contains(out.Stdout, "Server Version") {
		return nil
	} else {
		first = fmt.Errorf("guest docker info answered without 'Server Version'")
	}

	// overlay2 can fail to mount inside this unprivileged/NESTED guest:
	// fuse-overlayfs is the drop-in storage driver for exactly that.
	fallback := `pct exec ` + vid + ` -- sh -c 'export DEBIAN_FRONTEND=noninteractive; ` +
		`apt-get install -y fuse-overlayfs >/dev/null && ` +
		`mkdir -p /etc/docker && ` +
		`printf "{\"storage-driver\":\"fuse-overlayfs\"}\n" > /etc/docker/daemon.json && ` +
		`systemctl restart docker >/dev/null 2>&1 || ` +
		`service docker restart >/dev/null 2>&1'`
	// Even a failed fallback must surface the ORIGINAL daemon error + hint.
	if out, err := Exec(c, target, fallback, 600); err != nil || ExpectOK(out, "guest docker fuse fallback") != nil {
		return fmt.Errorf("fuse-overlayfs fallback failed in the guest (%v); original daemon error: (%v); consider a privileged container", err, first)
	}

	out, err := Exec(c, target, info, 120)
	if err != nil {
		return fmt.Errorf("docker daemon failed in the guest after the fuse-overlayfs fallback (%v; re-check: %v); consider a privileged container", first, err)
	}
	if e := ExpectOK(out, "guest docker info (after fallback)"); e != nil {
		return fmt.Errorf("docker daemon failed in the guest after the fuse-overlayfs fallback (%v; re-check: %v); consider a privileged container", first, e)
	}
	if !strings.Contains(out.Stdout, "Server Version") {
		return fmt.Errorf("docker daemon failed in the guest after the fuse-overlayfs fallback (%v); consider a privileged container", first)
	}
	return nil
}

// envName mirrors the runner's exec::env_name: alnum upcased, everything
// else '_'. The runner injects the credential as <ENV> and the base URL as
// <ENV>_URL.
func envName(target string) string {
	var b strings.Builder
	for _, r := range target {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.ToUpper(b.String())
}

// BootstrapVultrVps is the vultr-vps driver — the proven curl shapes,
// wrapped in the bootstrap orchestration (create -> poll active -> report;
// optional destroy for tests/cleanup).
func BootstrapVultrVps(c *client.McpClient, target string, spec *VultrVpsSpec) (*BootstrapResult, error) {
	if err := Plain(spec.Label); err != nil {
		return nil, err
	}
	if err := Plain(spec.Region); err != nil {
		return nil, err
	}
	if err := Plain(spec.Plan); err != nil {
		return nil, err
	}
	env := envName(target)
	envCred := "${" + env + "}"
	envURL := "${" + env + "_URL}"

	create := fmt.Sprintf(
		`curl -sS -X POST "%s/v2/instances" -H "Authorization: Bearer %s" -H 'Content-Type: application/json' -d '{"region":"%s","plan":"%s","os_id":%d,"label":"%s","hostname":"%s"}'`,
		envURL, envCred, spec.Region, spec.Plan, spec.OsID, spec.Label, spec.Label,
	)
	out, err := Exec(c, target, create, 120)
	if err != nil {
		return nil, err
	}
	if err := ExpectOK(out, "vultr create"); err != nil {
		return nil, err
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &created); err != nil {
		return nil, fmt.Errorf("create parse: %w: %s", err, out.Stdout)
	}
	inst, _ := created["instance"].(map[string]any)
	id, _ := inst["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("no instance id: %s", out.Stdout)
	}

	// Poll until the instance is reachable (bounded). 'active' alone is NOT
	// enough: Vultr reports 0.0.0.0 until the IP is assigned. destroy_after
	// ALWAYS destroys — including on every post-create failure — so a test
	// or a bad run can never leak a billed instance.
	var detail string
	verified := false
	var pollErr string
	mainIP := ""
	for i := 0; i < 60; i++ {
		poll := fmt.Sprintf(`curl -sS "%s/v2/instances/%s" -H "Authorization: Bearer %s"`, envURL, id, envCred)
		pout, err := Exec(c, target, poll, 120)
		if err == nil && ExpectOK(pout, "vultr poll") == nil {
			var status map[string]any
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(pout.Stdout)), &status); jerr == nil {
				sinst, _ := status["instance"].(map[string]any)
				state, _ := sinst["status"].(string)
				ip, _ := sinst["main_ip"].(string)
				if ip != "" && ip != "0.0.0.0" {
					mainIP = ip
				}
				if state == "active" && ip != "" && ip != "0.0.0.0" {
					detail = fmt.Sprintf("vultr instance %s active; main_ip %s; label %s — PVE host on Vultr (LXC-only appliance, domain identity)", id, ip, spec.Label)
					verified = true
					break
				}
				pollErr = ""
			} else {
				pollErr = "poll parse: " + jerr.Error()
			}
		} else if err != nil {
			pollErr = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	if !verified {
		if pollErr != "" {
			detail = "polling failed: " + pollErr
		} else {
			detail = fmt.Sprintf("instance %s did not become reachable within the poll window", id)
		}
	}

	if spec.DestroyAfter {
		if err := destroyVultr(c, target, envURL, envCred, id); err != nil {
			return nil, err
		}
		detail += "; destroyed"
	}

	if !verified {
		return nil, fmt.Errorf("%s", detail)
	}
	return &BootstrapResult{Kind: KindVultrVps, ID: id, Name: spec.Label, IP: mainIP, Detail: detail}, nil
}

// destroyVultr deletes an instance, retrying transient non-2xx (Vultr
// destroys transitionally: a delete issued right after verification can 409
// while the instance settles). A MISSED destroy is the one failure that
// keeps billing. --fail: curl exits non-zero on any HTTP >= 400.
func destroyVultr(c *client.McpClient, target, envURL, envCred, id string) error {
	destroy := fmt.Sprintf(
		`curl -sS -X DELETE "%s/v2/instances/%s" -H "Authorization: Bearer %s" -o /dev/null -w '%%{http_code}'`,
		envURL, id, envCred,
	)
	for attempt := 0; attempt < 5; attempt++ {
		out, err := Exec(c, target, destroy, 120)
		if err != nil {
			return err
		}
		if err := ExpectOK(out, "vultr destroy"); err != nil {
			return err
		}
		code := strings.TrimSpace(out.Stdout)
		if strings.HasPrefix(code, "2") {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("destroy of instance %s never confirmed (HTTP not 2xx after retries) — the instance may still be running and billing", id)
}

func buildHetznerCreate(envURL, envCred, label, location, serverType, image string) string {
	return fmt.Sprintf(
		`curl -sS -X POST "%s/v1/servers" -H "Authorization: Bearer %s" -H 'Content-Type: application/json' -d '{"name":"%s","server_type":"%s","image":"%s","location":"%s","hostname":"%s"}'`,
		envURL, envCred, label, serverType, image, location, label,
	)
}

// BootstrapHetznerVps is the hetzner-vps driver — Hetzner Cloud, same wire
// discipline as vultr: create -> poll (running) -> report; optional destroy.
// The Hetzner catalog churns: the asked-for type may be deprecated or
// unavailable in the location (seen live). On an invalid_input create,
// query availability and retry ONCE with a known-working type before
// failing.
func BootstrapHetznerVps(c *client.McpClient, target string, spec *HetznerVpsSpec) (*BootstrapResult, error) {
	for _, v := range []string{spec.Label, spec.Location, spec.ServerType, spec.Image} {
		if err := Plain(v); err != nil {
			return nil, err
		}
	}
	env := envName(target)
	envCred := "${" + env + "}"
	envURL := "${" + env + "_URL}"

	create := buildHetznerCreate(envURL, envCred, spec.Label, spec.Location, spec.ServerType, spec.Image)
	out, err := Exec(c, target, create, 120)
	if err != nil {
		return nil, err
	}
	if err := ExpectOK(out, "hetzner create"); err != nil {
		return nil, err
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &created); err != nil {
		return nil, fmt.Errorf("create response parse: %w", err)
	}
	srv, _ := created["server"].(map[string]any)
	idF, _ := srv["id"].(float64)

	if idF == 0 {
		// Ask the API which types the LOCATION actually sells; pick the
		// first non-deprecated one with availability there.
		avail := fmt.Sprintf(`curl -sS "%s/v1/server_types?per_page=100" -H "Authorization: Bearer %s"`, envURL, envCred)
		ao, err := Exec(c, target, avail, 120)
		if err != nil {
			return nil, err
		}
		var av map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(ao.Stdout)), &av); err != nil {
			return nil, fmt.Errorf("availability parse: %w", err)
		}
		fallback := ""
		if types, ok := av["server_types"].([]any); ok {
			for _, t := range types {
				tm, _ := t.(map[string]any)
				if dep, _ := tm["deprecated"].(bool); dep {
					continue
				}
				locs, _ := tm["locations"].([]any)
				for _, l := range locs {
					lm, _ := l.(map[string]any)
					if name, _ := lm["name"].(string); name == spec.Location {
						if avl, _ := lm["available"].(bool); avl {
							fallback, _ = tm["name"].(string)
						}
					}
				}
				if fallback != "" {
					break
				}
			}
		}
		if fallback == "" {
			return nil, fmt.Errorf("hetzner create failed (%s): no available server type %q in location %s",
				strings.TrimSpace(out.Stdout), spec.ServerType, spec.Location)
		}
		create = buildHetznerCreate(envURL, envCred, spec.Label, spec.Location, fallback, spec.Image)
		out, err = Exec(c, target, create, 120)
		if err != nil {
			return nil, err
		}
		if err := ExpectOK(out, "hetzner create (availability fallback)"); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &created); err != nil {
			return nil, fmt.Errorf("create response parse: %w", err)
		}
		srv, _ = created["server"].(map[string]any)
		idF, _ = srv["id"].(float64)
	}
	if idF == 0 {
		return nil, fmt.Errorf("no server id in create response: %s", strings.TrimSpace(out.Stdout))
	}
	id := strconv.FormatUint(uint64(idF), 10)

	verified := false
	var pollErr string
	mainIP := ""
	for i := 0; i < 120; i++ {
		poll := fmt.Sprintf(`curl -sS "%s/v1/servers/%s" -H "Authorization: Bearer %s"`, envURL, id, envCred)
		pout, err := Exec(c, target, poll, 120)
		if err == nil && ExpectOK(pout, "hetzner poll") == nil {
			var v map[string]any
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(pout.Stdout)), &v); jerr == nil {
				s, _ := v["server"].(map[string]any)
				status, _ := s["status"].(string)
				ip := ""
				if pn, ok := s["public_net"].(map[string]any); ok {
					if v4, ok := pn["ipv4"].(map[string]any); ok {
						ip, _ = v4["ip"].(string)
					}
				}
				if status == "running" && ip != "" {
					mainIP = ip
					verified = true
					break
				}
				pollErr = ""
			} else {
				pollErr = "poll parse: " + jerr.Error()
			}
		} else if err != nil {
			pollErr = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	if !verified {
		if pollErr != "" {
			return nil, fmt.Errorf("server %s did not become running: %s", id, pollErr)
		}
		return nil, fmt.Errorf("server %s did not become running within the poll window", id)
	}

	if spec.DestroyAfter {
		if err := destroyHetzner(c, target, envURL, envCred, id); err != nil {
			return nil, err
		}
		return &BootstrapResult{
			Kind:   KindVultrVps, // shared API-target family (reporting-only)
			ID:     id,
			Name:   spec.Label,
			IP:     mainIP,
			Detail: fmt.Sprintf("hetzner server %s (%s) active + destroyed — PVE host probe (domain identity)", id, spec.Label),
		}, nil
	}
	return &BootstrapResult{
		Kind:   KindVultrVps, // shared API-target family (reporting-only)
		ID:     id,
		Name:   spec.Label,
		IP:     mainIP,
		Detail: fmt.Sprintf("hetzner server %s active; ipv4 %s; label %s — PVE host on Hetzner (LXC-only appliance, domain identity)", id, mainIP, spec.Label),
	}, nil
}

// destroyHetzner deletes a server, retrying transient non-2xx like the
// vultr driver; a 404 counts as destroyed (idempotent).
func destroyHetzner(c *client.McpClient, target, envURL, envCred, id string) error {
	destroy := fmt.Sprintf(
		`curl -sS -X DELETE "%s/v1/servers/%s" -H "Authorization: Bearer %s" -o /dev/null -w '%%{http_code}'`,
		envURL, id, envCred,
	)
	for attempt := 0; attempt < 5; attempt++ {
		out, err := Exec(c, target, destroy, 120)
		if err != nil {
			return err
		}
		if err := ExpectOK(out, "hetzner destroy"); err != nil {
			return err
		}
		code := strings.TrimSpace(out.Stdout)
		if strings.HasPrefix(code, "2") || code == "404" {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("destroy of server %s never confirmed — the server may still be running and billing", id)
}

// WaitForDomainResolution is the A4 DOMAIN GATE (blocking): after the
// target is up, the install waits until `domain` resolves to `wantIP` (LAN
// DNS, or /etc/hosts for the POC). The domain is the community's identity;
// an IP-hosted community IS IP-identity, which is exactly what this gate
// prevents from ever happening.
func WaitForDomainResolution(domain, wantIP string, waitSecs uint64, resolve func(string) (net.IP, bool), sleep func(time.Duration)) error {
	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	hint := fmt.Sprintf("map '%s %s' in your LAN DNS (or /etc/hosts for the POC)", domain, wantIP)
	hintPrinted := false
	var pollCount uint64
	for {
		pollCount++
		ip, ok := resolve(domain)
		if ok {
			if ip.String() == wantIP {
				fmt.Printf("DOMAIN-GATE: %s resolves to %s — continuing\n", domain, wantIP)
				return nil
			}
			// The domain's identity is established — it resolves. It may
			// legitimately point at an OPERATOR-MANAGED PROXY (e.g. an
			// nginx on the tailnet) that forwards to this target rather
			// than at the target directly. Proceed, but say so loudly.
			fmt.Printf("DOMAIN-GATE: %s resolves to %s — continuing. NOTE: that is NOT the target %s; if %s is a proxy, ensure it forwards %s to %s\n",
				domain, ip, wantIP, ip, domain, wantIP)
			return nil
		}
		// the hint is the actionable part — print it ONCE, then only every
		// 20th poll (the print previously ran per-iteration).
		if !hintPrinted {
			fmt.Printf("DOMAIN-GATE: %s does not resolve yet — %s\n", domain, hint)
			hintPrinted = true
		} else if pollCount%20 == 0 {
			fmt.Printf("DOMAIN-GATE: %s still does not resolve — %s\n", domain, hint)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("domain gate: %s still does not resolve within %ds — %s", domain, waitSecs, hint)
		}
		sleep(time.Second)
	}
}

// ResolveIP resolves a domain to its first address (honors the system
// resolver, including /etc/hosts).
func ResolveIP(domain string) (net.IP, bool) {
	ips, err := net.LookupIP(domain)
	if err != nil || len(ips) == 0 {
		return nil, false
	}
	return ips[0], true
}
