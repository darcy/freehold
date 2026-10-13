// Package pangolin implements `freehold pangolin` — provisioning the world's
// Pangolin VPS through the created-host lifecycle: the Vultr mint (the same
// seam the world host uses, the pangolin-compose payload instead of
// PVE-on-Debian), the root integration key captured at mint and sealed to the
// console identity (CP secret "pangolin"), and the profile recording the bill
// (`[pangolin_host]`) that `freehold uninstall --destroy-host` destroys.
// After this, `freehold build` carries the pangolin coords into the world and
// the build tail asserts the Newt tunnel on the gateway + the integration
// tunnel on the CP.
package pangolin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/freehold-cli/internal/certcred"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/box"
	cert "freehold/platform/services/certificates/letsencrypt"
	dnsman "freehold/platform/services/externaldns/cloudflare"
	"freehold/providers/proxmox"
	"freehold/providers/vultr"
)

// emailRe bounds the ACME contact before it rides the install script (an
// email with a quote or a shell metacharacter would break out of the
// script's arguments).
var emailRe = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+$`)

var pangolinCmd = &cobra.Command{
	Use:   "pangolin",
	Short: "Provision the world's Pangolin VPS (the public path's edge) through the created-host lifecycle",
	RunE:  run,
}

func run(cmd *cobra.Command, args []string) error {
	cfgPath, _ := cmd.Flags().GetString("config")
	if p := config.ProfileForConfigPath(cfgPath); p != nil && p.Name != "" {
		config.SetCurrent(p)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg == nil {
		return fmt.Errorf("pangolin needs an installed world — no config at %s (run `freehold install` first)", cfgPath)
	}
	if cfg.CPURL == "" {
		return fmt.Errorf("pangolin needs the world's control plane up (no cp_url on record) — run `freehold install` + `freehold build` first")
	}
	if cfg.Pangolin != nil {
		fmt.Printf("  the world already carries a pangolin public path (org %s, site %s) — re-running re-adopts/updates the VPS\n", cfg.Pangolin.Org, cfg.Pangolin.Site)
	}
	worldDomain := certcred.DNSZone(cfg.RelayHost())
	if worldDomain == "" {
		return fmt.Errorf("pangolin: no world domain under the relay host %q — the app siblings anchor there", cfg.RelayHost())
	}
	providerName, _ := cmd.Flags().GetString("provider")
	if providerName != "vultr" {
		return fmt.Errorf("the pangolin VPS rides the created-host lifecycle (vultr today; the provider registry grows) — pass --provider vultr")
	}
	email, _ := cmd.Flags().GetString("email")
	email = strings.ToLower(strings.TrimSpace(email))
	if !emailRe.MatchString(email) {
		return fmt.Errorf("pangolin needs the ACME contact email (--email) — a plain address; it rides the install script's arguments")
	}
	region, _ := cmd.Flags().GetString("region")
	plan, _ := cmd.Flags().GetString("plan")
	if region == "" {
		region = vultr.DefaultRegion
	}
	if plan == "" {
		plan = "vc2-2c-4gb"
	}

	existingID := ""
	if cfg.PangolinHost != nil {
		existingID = cfg.PangolinHost.ID
	}
	session, captured, cleanup, err := buildPangolinSession(cfg, region, plan)
	if err != nil {
		return err
	}
	defer cleanup() // the temp door key dies when the run does
	// A mint + an install takes minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*60*time.Second)
	defer cancel()
	host, err := vultr.PreparePangolin(ctx, session, existingID, worldDomain, email)
	if err != nil {
		return err
	}
	keyLine := parseRootKey(*captured)
	if keyLine == "" {
		return fmt.Errorf("the pangolin install printed no root key — the instance %s is LIVE AND BILLING; recover the key from /srv/pangolin/root-key.env on it (the door key reaches it)", host.ID)
	}
	if err := verifyOrgJSON(*captured); err != nil {
		return err
	}
	// The APIs bind loopback on the VPS: the CP's client dials the SSH
	// forward's loopback; Newt dials TLS via the edge's own hostname.
	endpoint := "http://127.0.0.1:3003"
	newtEndpoint := "https://pangolin." + worldDomain

	// Record the bill + the public-path shape in the profile (uninstall's
	// --destroy-host finds it here). DNSIP = the VPS: the exposed apps' A
	// records move to the edge the moment the build runs.
	cfg.PangolinHost = &config.PangolinHostSpec{Provider: "vultr", ID: host.ID, IP: host.IP, Endpoint: endpoint}
	cfg.Pangolin = &config.PangolinCoords{Org: "freehold", Site: cfg.Name, Domain: worldDomain, DNSIP: host.IP}
	// The edge's own hostname (pangolin.<world>) points at the VPS: its ACME
	// cert issues there (HTTP-01) and Newt dials TLS through it.
	if err := pointPangolinHost(cfg, "pangolin."+worldDomain, host.IP); err != nil {
		fmt.Printf("  (the pangolin.<world> record could not be written: %v — the edge cert + the Newt TLS dial need it; write %s A %s by hand)\n", err, "pangolin."+worldDomain, host.IP)
	}
	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("record the pangolin host: %w", err)
	}

	// The tunnel's key: minted ON the CP guest (the private half never
	// transits — the door private key never leaves the box), its pub line
	// authorized on the VPS over the door SSH (the VPS was born with the
	// door's line).
	if err := mintTunnelKey(cfg); err != nil {
		return fmt.Errorf("mint the tunnel key on the CP guest: %w", err)
	}
	pubLine, err := readTunnelPub(cfg)
	if err != nil {
		return err
	}
	if err := authorizeOnVPS(host.IP, pubLine); err != nil {
		return fmt.Errorf("authorize the tunnel key on the VPS: %w", err)
	}

	// Seal the integration credential to the console identity and store it
	// CP-side (the same path the build's DNS-cred hand-off uses): the world's
	// CP opens it to drive the expose verbs.
	client, err := consoleLogin(cfg)
	if err != nil {
		return err
	}
	pub, err := consoleEncPubkey(client)
	if err != nil {
		return err
	}
	seal := func(recipient, aad, plain []byte) ([]byte, error) { return crypto.Seal(recipient, aad, plain) }
	id, secret, ok := strings.Cut(keyLine, ".")
	if !ok {
		return fmt.Errorf("the pangolin root key is not apiKeyId.secret: %.24s…", keyLine)
	}
	blob, err := certcred.CPSecretBlob("pangolin", "pangolin", map[string]string{
		"PANGOLIN_BASE":     endpoint, // the CP's loopback — the tunnel stage's forward
		"PANGOLIN_KEY":      id + "." + secret,
		"PANGOLIN_ENDPOINT": newtEndpoint, // what Newt dials: TLS via the edge's own cert
	}, seal, pub)
	if err != nil {
		return err
	}
	if err := client.PutSecret("pangolin", blob); err != nil {
		return fmt.Errorf("seal the pangolin credential on the CP: %w", err)
	}

	fmt.Printf(`
  │  The Pangolin VPS is up: the public path's edge        │
  ╰────────────────────────────────────────────────────────╯

  instance:    %s (%s, %s)
  integration: the CP's loopback through the door-key tunnel
  edge host:   pangolin.%s
  base domain: %s
  root key:    sealed on the CP (secret "pangolin") — printed once at mint above

  Now run `+"`freehold build`"+`: the coords carry the pangolin path and the build
  tail asserts the tunnels (the CP's forward + the gateway's Newt). Exposed
  apps then point at the VPS instead of at home.
`, host.ID, region, plan, worldDomain, worldDomain)
	return nil
}

func buildPangolinSession(cfg *config.Config, region, plan string) (*provisioning.HostSession, *[]string, func(), error) {
	// The env check BEFORE the key file exists — the most common user error
	// must not leave the 0600 door PEM behind.
	if strings.TrimSpace(os.Getenv("VULTR_API_KEY")) == "" {
		return nil, nil, nil, fmt.Errorf("pangolin needs the Vultr API key (VULTR_API_KEY in the env; used for this mint, never stored)")
	}
	if err := box.EnsureIdentity(box.OpsDir()); err != nil {
		return nil, nil, nil, fmt.Errorf("ops identity: %w", err)
	}
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("door key: %w", err)
	}
	line, err := crypto.ExtractED25519PublicKeyLine(pem)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("door key: %w", err)
	}
	keyPath, cleanup, err := proxmox.WriteTempKey(pem)
	if err != nil {
		return nil, nil, nil, err
	}
	captured := &[]string{}
	session := &provisioning.HostSession{
		Answers: map[string]string{
			"VULTR_API_KEY": strings.TrimSpace(os.Getenv("VULTR_API_KEY")),
			"region":        region,
			"plan":          plan,
		},
		DoorLine: line,
		World:    cfg.Name,
	}
	session.ExecOnHost = func(script string, timeoutSecs uint64) error {
		out, err := proxmox.SSHExec(session.Host, keyPath)(script, timeoutSecs)
		if err != nil {
			return err
		}
		*captured = append(*captured, out.Stdout)
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("host script failed: %s", strings.TrimSpace(out.Stderr))
		}
		return nil
	}
	session.OnCreated = func(id string) {
		// Persist the handle the instant the create succeeds — a death
		// mid-prepare must not strand a billed instance the tooling can't
		// see. The profile records it; a re-run re-adopts.
		cfg.PangolinHost = &config.PangolinHostSpec{Provider: "vultr", ID: id}
		if err := cfg.Save(config.ConfigPath()); err != nil {
			fmt.Printf("  (the instance %s is LIVE AND BILLING and its handle could not be recorded: %v — note the id by hand)\n", id, err)
		}
	}
	session.Print = func(format string, args ...any) { fmt.Printf(format, args...) }
	return session, captured, cleanup, nil
}

// pangolinKeyJSON is the mint response's shape (the wrapped envelope).
type pangolinKeyJSON struct {
	Data struct {
		APIKeyID string `json:"apiKeyId"`
		APIKey   string `json:"apiKey"`
	} `json:"data"`
}

// parseRootKey pulls the minted key's JSON line out of the install output
// and parses it GO-SIDE (the shell never parses JSON). It also ships the
// key file back to the VPS (root-only) so the next run re-prints instead
// of re-minting.
func parseRootKey(outputs []string) string {
	key := ""
	for _, o := range outputs {
		for _, l := range strings.Split(o, "\n") {
			if !strings.HasPrefix(l, "PANGOLIN_KEY_JSON=") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(l, "PANGOLIN_KEY_JSON="))
			var j pangolinKeyJSON
			if json.Unmarshal([]byte(raw), &j) != nil {
				continue
			}
			if j.Data.APIKeyID != "" && j.Data.APIKey != "" {
				key = j.Data.APIKeyID + "." + j.Data.APIKey
			}
		}
	}
	return key
}

// verifyOrgJSON asserts the org create's response named the world's org.
func verifyOrgJSON(outputs []string) error {
	for _, o := range outputs {
		for _, l := range strings.Split(o, "\n") {
			if !strings.HasPrefix(l, "PANGOLIN_ORG_JSON=") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(l, "PANGOLIN_ORG_JSON="))
			var j struct {
				Data struct {
					OrgID string `json:"orgId"`
				} `json:"data"`
				Error bool `json:"error"`
			}
			if json.Unmarshal([]byte(raw), &j) != nil {
				continue
			}
			if j.Data.OrgID == "freehold" {
				return nil
			}
		}
	}
	return fmt.Errorf("the pangolin org was not created (the install output carried no successful org create)")
}

// The tunnel key lives on the CP guest at this path (the tunnel unit's -i).
const tunnelKeyPath = "/srv/data/cp/pangolin-tunnel.key"

// mintTunnelKey runs ssh-keygen ON the CP guest (through the world host's
// runner): the private half is born there and never transits. Idempotent.
func mintTunnelKey(cfg *config.Config) error {
	vmid := cpVmid(cfg)
	out, err := freeholdExec("exec", "proxmox-box", fmt.Sprintf(
		"pct exec %d -- sh -c '[ -f %s ] || ssh-keygen -t ed25519 -N \"\" -f %s -C pangolin-tunnel >/dev/null && chmod 600 %s && test -f %s'",
		vmid, tunnelKeyPath, tunnelKeyPath, tunnelKeyPath, tunnelKeyPath))
	if err != nil || !strings.Contains(out, "tunnel.key") && !strings.Contains(out, "pangolin-tunnel") {
		// ssh-keygen is quiet on success here; the file test's exit carries
		// the truth — re-check.
		out2, err2 := freeholdExec("exec", "proxmox-box", fmt.Sprintf(
			"pct exec %d -- test -f %s && echo key-present", vmid, tunnelKeyPath))
		if err2 != nil || !strings.Contains(out2, "key-present") {
			return fmt.Errorf("the key did not land: %.160s", out)
		}
	}
	return nil
}

// readTunnelPub reads the minted key's PUBLIC line (non-secret; argv-safe).
func readTunnelPub(cfg *config.Config) (string, error) {
	out, err := freeholdExec("exec", "proxmox-box", fmt.Sprintf(
		"pct exec %d -- cat %s.pub", cpVmid(cfg), tunnelKeyPath))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		return "", fmt.Errorf("not an ssh pub line: %.80s", line)
	}
	return line, nil
}

// authorizeOnVPS appends the tunnel key's pub line to the VPS's
// authorized_keys over the door SSH (the VPS was born with the door's
// line).
func authorizeOnVPS(vpsIP, pubLine string) error {
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return fmt.Errorf("door key: %w", err)
	}
	keyPath, cleanup, err := proxmox.WriteTempKey(pem)
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := proxmox.SSHExec("root@"+vpsIP, keyPath)(
		fmt.Sprintf("mkdir -p ~/.ssh && chmod 700 ~/.ssh && grep -Fqx '%s' ~/.ssh/authorized_keys 2>/dev/null || (echo '%s' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys)", pubLine, pubLine),
		60)
	if err != nil {
		return err
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return fmt.Errorf("authorized_keys append failed: %.160s", out.Stderr)
	}
	return nil
}

func cpVmid(cfg *config.Config) int {
	if cfg.Lxc.Cp.Vmid != nil {
		return int(*cfg.Lxc.Cp.Vmid)
	}
	return 0
}

// freeholdExec runs the freehold CLI's own exec verb in-process.
func freeholdExec(args ...string) (string, error) {
	cmd := exec.Command("freehold", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pointPangolinHost writes the edge's own hostname record
// (pangolin.<world>) — its ACME cert issues on the VPS (HTTP-01) and Newt
// dials TLS through it. The zone credential = the box's own sealed DNS copy
// (the one the build's record management reads); a manual-DNS world returns
// an error the caller surfaces as the by-hand instruction.
func pointPangolinHost(cfg *config.Config, host, ip string) error {
	cc := &certcred.Engine{Out: os.Stdout, Yes: true}
	_, identSecret, _, err := cc.CertIdent()
	if err != nil {
		return err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	provider, env, err := cert.LoadCreds(cc.CertCredPath("relay"), open, identSecret)
	if err != nil {
		return err
	}
	if provider == "manual" {
		return fmt.Errorf("manual DNS mode")
	}
	mgr, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	return mgr.UpsertA(host, ip)
}

func consoleLogin(cfg *config.Config) (*console.Client, error) {
	secStr, err := oplogin.SecretHex()
	if err != nil {
		return nil, fmt.Errorf("no operator identity — run `freehold login` first: %w", err)
	}
	key, err := oplogin.NsecToSecret(secStr)
	if err != nil {
		return nil, fmt.Errorf("stored operator identity is invalid: %w", err)
	}
	client, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return nil, fmt.Errorf("console login at %s failed: %w", cfg.CPURL, err)
	}
	return client, nil
}

func consoleEncPubkey(client *console.Client) ([]byte, error) {
	w, err := client.World()
	if err != nil {
		return nil, err
	}
	if w.ConsoleEncPubkey == "" {
		return nil, fmt.Errorf("the CP reports no console enc pubkey (an older CP? update first)")
	}
	pk, derr := hex.DecodeString(w.ConsoleEncPubkey)
	if derr != nil || len(pk) != 32 {
		return nil, fmt.Errorf("console enc pubkey from /api/world not 32-byte hex: %q", w.ConsoleEncPubkey)
	}
	return pk, nil
}

func init() {
	pangolinCmd.Flags().String("config", config.DefaultPath(), "Config path (the world's profile)")
	pangolinCmd.Flags().String("provider", "vultr", "The host provider for the Pangolin VPS (vultr today)")
	pangolinCmd.Flags().String("email", "", "ACME contact email for the edge's certificates (required)")
	pangolinCmd.Flags().String("region", "", "Vultr region (default "+vultr.DefaultRegion+")")
	pangolinCmd.Flags().String("plan", "", "Vultr plan (default vc2-2c-4gb — the edge is light)")
}

// Command returns the pangolin command for root registration.
func Command() *cobra.Command { return pangolinCmd }
