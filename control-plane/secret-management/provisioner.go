// Package provisioner reproduces control-plane/src/provisioner.rs: provision,
// adopt, rotate a secret — seal TO the runner's enc key, ship the package
// (identity.json + ciphertext-only secrets.json), record pubkeys + ciphertext
// only. NO master key, NO retained private keys, NO plaintext.
package provisioner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/state"
)

// identity.json is the runner's identity file (private keys).
const identityFile = "identity.json"

// IDENTITY_FILE content (matches core::identity::IDENTITY_FILE).
var _ = identityFile

// ProvisionResult is what provision returns.
type ProvisionResult struct {
	Name        string
	NostrPubkey string
	EncPubkey   string
	PackageDir  string
	// Readdressed marks the re-adopt path: a live runner whose substrate
	// address rotated. The record + package moved on disk; the RUNNER loads
	// its package once at boot, so the caller owning the runner's unit must
	// restart it (the console surfaces this itself; a box-side re-adopt
	// ships the corrected package at deploy-cp, whose restart reloads).
	Readdressed bool
	OldAddress  string
	// RunnerRestarted / RestartError are the CONSOLE's bookkeeping: whether
	// the unit serving this runner was restarted after the re-address, and
	// why not when it wasn't (a capability door's own unit, the co-located
	// unit, or a runner that lives elsewhere and owes a manual restart).
	RunnerRestarted bool
	RestartError    string
}

// ProvisionRequest is the input to provision_runner.
type ProvisionRequest struct {
	Name      string
	Kind      string
	Address   string
	Secret    []byte
	RunnerDir string
	Grants    []string
	RiskLevel *string
	// Probe is the parameterized verify arm — "<METHOD> <path> [auth] [want]"
	// (e.g. "GET /user/tokens/verify bearer"). Empty = legacy (the runner's
	// built-in kind match). ProbeBody is an optional literal request body.
	Probe     string
	ProbeBody string
}

// probeAuthStyles are the credential-attachment styles a probe may name.
var probeAuthStyles = map[string]bool{"bearer": true, "basic": true, "json-body": true, "none": true}

// probePathRe constrains the probe path: `-` prefixed, URL-safe charset
// (query strings allowed), no whitespace/shell metacharacters — the runner
// composes the curl from validated parts, so nothing here reaches a shell.
var probePathRe = regexp.MustCompile(`^/[A-Za-z0-9/._~?&=:-]{0,255}$`)

// probeWantRe is the expected HTTP status code (3 digits).
var probeWantRe = regexp.MustCompile(`^\d{3}$`)

// ValidateProbe checks a probe spec string: "<METHOD> <path> [auth] [want]
// [insecure]" — method GET|POST, path per probePathRe, auth one of
// probeAuthStyles (default bearer), want a 3-digit code (default 200), and an
// optional literal "insecure" (curl -k — private-CA targets like a k3s API).
// Returns the normalized spec (upper-cased method, defaults filled). The
// API-boundary guard: a probe that fails this never reaches state, a package,
// or a shell.
func ValidateProbe(probe string) (string, error) {
	probe = strings.TrimSpace(probe)
	fields := strings.Fields(probe)
	if len(fields) < 2 || len(fields) > 5 {
		return "", fmt.Errorf("invalid probe %q: want \"<METHOD> <path> [auth] [want] [insecure]\" (e.g. \"GET /user/tokens/verify bearer\")", probe)
	}
	method := strings.ToUpper(fields[0])
	if method != "GET" && method != "POST" {
		return "", fmt.Errorf("invalid probe method %q: GET or POST", fields[0])
	}
	if !probePathRe.MatchString(fields[1]) {
		return "", fmt.Errorf("invalid probe path %q: must start with '/', URL-safe chars only, max 256", fields[1])
	}
	auth := "bearer"
	if len(fields) > 2 {
		auth = fields[2]
		if !probeAuthStyles[auth] {
			return "", fmt.Errorf("invalid probe auth %q: bearer, basic, json-body or none", fields[2])
		}
	}
	want := "200"
	if len(fields) > 3 {
		want = fields[3]
		if !probeWantRe.MatchString(want) {
			return "", fmt.Errorf("invalid probe want %q: 3-digit HTTP status", fields[3])
		}
	}
	insecure := ""
	if len(fields) > 4 {
		if fields[4] != "insecure" {
			return "", fmt.Errorf("invalid probe flag %q: only \"insecure\" (curl -k for a private-CA target)", fields[4])
		}
		insecure = "insecure"
	}
	out := []string{method, fields[1], auth, want}
	if insecure != "" {
		out = append(out, insecure)
	}
	return strings.Join(out, " "), nil
}

// ValidateProbeBody checks a literal probe request body: must parse as JSON
// and stay shell-safe (the runner injects it single-quoted) — no single
// quotes, backticks, dollar signs, or newlines.
func ValidateProbeBody(body string) error {
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("invalid probe body: must be JSON")
	}
	if strings.ContainsAny(body, "'`$\n\r") {
		return fmt.Errorf("invalid probe body: single quotes, backticks, $ and newlines are refused")
	}
	return nil
}

// defaultRisk is the kind-based risk default.
func defaultRisk(kind string) *string {
	var v string
	switch kind {
	case "vultr", "b2", "hetzner", "github", "websearch", "litellm", "kubernetes", "cloudflare", "unifi", "browser":
		v = "safe"
	case "ssh":
		v = "risky-install"
	case "local":
		v = "safe"
	default:
		return nil
	}
	return &v
}

// IsPubkey reports whether s is a 64-char lowercase/mixed hex string (a
// pubkey presented by an enrolling runner or an agent grant).
func IsPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// doorNameRe constrains runner names used to DERIVE a system username: the
// name flows into `useradd` and `chown` as shell-free exec argv, and the
// charset is pinned so a username can never smuggle option flags or
// separators. Shape only — NO length cap: derived DNS-zone door names are
// unbounded (cloudflareRunners), and length is irrelevant to the injection
// guard. Same character shape as the provision flow's kebab-case rule.
var doorNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// DoorUser derives the per-door unprivileged system user an ssh/api
// capability runner's unit runs as. A `local` door returns "" — a local door
// IS root on its own host (the documented contract: its exec is the point),
// so it keeps the root unit. An unparsable name is an error, never a silent
// fall-back to root.
//
// The derivation is INJECTIVE by construction: verbatim (`fhd-<name>`) only
// when the whole name body fits the truncation width (≤19), otherwise
// `fhd-<name[:19]>-<crc32(name)>` (32 chars). A crafted short name can
// therefore never re-derive a long name's truncated user — its own body
// exceeds 19 chars and takes the truncated branch, where its crc32 differs —
// so two doors can share a uid only on a crc32 collision over a shared
// 19-char prefix. That matters because EnsureRunnerUser chowns the door's
// package dir to this user: a shared uid would let one door's runner read
// another door's identity.json (its Nostr/enc private keys + sealed
// credential).
func DoorUser(kind, name string) (string, error) {
	if kind == "local" {
		return "", nil
	}
	if !doorNameRe.MatchString(name) {
		return "", fmt.Errorf("door user: name %q must be kebab-case [a-z0-9-]", name)
	}
	if len(name) <= 19 {
		return "fhd-" + name, nil
	}
	return fmt.Sprintf("fhd-%s-%08x", name[:19], crc32.ChecksumIEEE([]byte(name))), nil
}

// EnsureRunnerUser idempotently creates a CP-guest capability door's
// unprivileged system user and gives it the door's package dir. Every
// re-seal (rotate, fill, the build's re-ship) writes the package root-owned
// again, so callers run this before every (re)start of a non-root unit.
// No-op for local doors (root contract) and for empty kind (the co-located
// CP lifecycle runner, which is root by design).
func EnsureRunnerUser(kind, name, pkgDir string) (string, error) {
	user, err := DoorUser(kind, name)
	if err != nil || user == "" {
		return user, err
	}
	script := fmt.Sprintf("id -u %s >/dev/null 2>&1 || useradd -r -M -s /usr/sbin/nologin %s", user, user)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		return "", fmt.Errorf("create door user %s: %v: %s", user, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("chown", "-R", user+":"+user, pkgDir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("chown %s package: %v: %s", user, err, strings.TrimSpace(string(out)))
	}
	return user, nil
}

// DoorSandbox renders the systemd sandbox lines for a non-root door unit:
// the whole filesystem read-only except the door's own package dir (its
// state, sealed package, audit spool), no privilege escalation, its own
// /tmp. The ExecStartPre (the `+` prefix runs it with full privileges,
// bypassing User= and the sandbox) re-asserts package ownership at EVERY
// start: any re-ship (rotate, fill, grant, add-secret, re-address) lands
// root-owned, and the next start — by the build, the console, the CLI, or a
// host reboot — heals it. Nothing else needs to know. An empty user (local
// doors, the co-located runner) renders "" — those run as root by contract.
func DoorSandbox(user, pkgDir string) string {
	if user == "" {
		return ""
	}
	// The package dir sits inside the CP state dir (0700, re-tightened by
	// state.Open on every open): the door user must be able to TRAVERSE the
	// ancestors to read its own identity.json, or the unit crash-loops
	// "no identity found" with the file right there. o+x only (traverse,
	// never list); the state dir's contents stay 0600/0700. Healed at every
	// start like the ownership line, so a later Open or re-ship cannot
	// strand it. systemd needs $$ for a literal $.
	return fmt.Sprintf(`User=%s
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=%s
PrivateTmp=yes
ProtectHome=yes
RestrictSUIDSGID=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
ProtectClock=yes
ExecStartPre=+/usr/bin/chown -R %s %s
ExecStartPre=+/bin/sh -c 'd=$$(/usr/bin/dirname %s); while [ "$$d" != "/" ]; do /usr/bin/chmod o+x "$$d" 2>/dev/null || true; d=$$(/usr/bin/dirname "$$d"); done'`, user, pkgDir, user, pkgDir, pkgDir)
}

func hexToArr(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("pubkey must be 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ProvisionRunner is the B1 happy path: existing service + credential ->
// runner with the secret encrypted to its key.
func ProvisionRunner(store *state.StateStore, req *ProvisionRequest) (*ProvisionResult, error) {
	// Keep the name a bare name — it flows into package dir paths.
	if req.Name == "" || strings.Contains(req.Name, "/") || strings.HasPrefix(req.Name, ".") {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name (no '/', no leading '.')", req.Name)
	}
	if req.Name == "local" {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name (no '/', no leading '.')", req.Name)
	}
	for _, g := range req.Grants {
		if !IsPubkey(g) {
			return nil, fmt.Errorf("invalid agent pubkey %q: must be 64 hex chars", g)
		}
	}
	// The address IS the door's endpoint: an empty one re-addresses the
	// runner to nowhere (a re-run that omitted --address would wipe a live
	// door). Only a local exec target runs without one.
	if req.Kind != "local" && strings.TrimSpace(req.Address) == "" {
		return nil, fmt.Errorf("runner %s: an empty address would wipe the door's endpoint (kind %q dials its address)", req.Name, req.Kind)
	}
	probe := ""
	probeBody := ""
	if p := strings.TrimSpace(req.Probe); p != "" {
		if req.Kind == "ssh" || req.Kind == "local" {
			return nil, fmt.Errorf("probe is an api-class field — kind %q verifies itself", req.Kind)
		}
		var err error
		if probe, err = ValidateProbe(p); err != nil {
			return nil, fmt.Errorf("runner %s: %w", req.Name, err)
		}
		if req.ProbeBody != "" {
			if err := ValidateProbeBody(req.ProbeBody); err != nil {
				return nil, fmt.Errorf("runner %s: %w", req.Name, err)
			}
			probeBody = req.ProbeBody
		}
	} else if req.ProbeBody != "" {
		return nil, fmt.Errorf("runner %s: probe_body requires probe", req.Name)
	}
	if r, ok := store.GetRunner(req.Name); ok {
		if r.Status == state.RunnerRevoked {
			return nil, fmt.Errorf("runner %s is revoked — provision a new service or restore it", req.Name)
		}
		// A LIVE runner re-provisioned: the identity + sealed credential
		// persist; the substrate ADDRESS may rotate (a re-adopt whose host
		// was destroyed and recreated — the old address 404s the moment the
		// CP's world-build dials it). Re-address the record + package when
		// the request moves the runner; an unchanged address stays the
		// caller's reuse error.
		return readdressRunner(store, r, req)
	}
	// Refuse to clobber a dir already holding a package.
	if _, err := os.Stat(filepath.Join(req.RunnerDir, identityFile)); err == nil {
		return nil, fmt.Errorf("package dir %s already holds a runner — refusing to clobber", req.RunnerDir)
	}
	for name, rec := range store.Snapshot().Runners {
		if rec.PackageDir == req.RunnerDir {
			return nil, fmt.Errorf("package dir %s already holds a runner — refusing to clobber (runner %s)", req.RunnerDir, name)
		}
	}

	// Generate identity + seal the credential TO the runner's enc key.
	_, err := identityGenerate(req.RunnerDir)
	if err != nil {
		return nil, err
	}
	// (identity file written by identityGenerate)

	// load back the pubkeys/secrets for the record.
	nostrPubkey, encPubkeyHex, _, err := loadIdentity(req.RunnerDir)
	if err != nil {
		return nil, err
	}
	encPub, err := hexToArr(encPubkeyHex)
	if err != nil {
		return nil, err
	}
	// aad = secret NAME — pins the blob to the entry it's filed under.
	sealed, err := crypto.Seal(encPub[:], []byte(req.Name), req.Secret)
	if err != nil {
		return nil, err
	}
	ciphertextHex := hex.EncodeToString(sealed)

	// Ship the package: secrets.json holds ciphertext only.
	pkg := wire.New(
		map[string]string{req.Name: ciphertextHex},
		map[string]wire.TargetMeta{req.Name: {Kind: req.Kind, Address: req.Address, Secret: req.Name,
			Probe: probe, ProbeBody: probeBody}},
		req.Grants,
	)
	if err := pkg.WriteToDir(req.RunnerDir); err != nil {
		// identity.json already landed — remove so PackageDirInUse doesn't
		// lock the dir on retry.
		os.Remove(filepath.Join(req.RunnerDir, identityFile))
		return nil, err
	}

	now := uint64(time.Now().Unix())
	risk := req.RiskLevel
	if risk == nil {
		risk = defaultRisk(req.Kind)
	}
	store.InsertRunner(req.Name, state.RunnerRecord{
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkeyHex,
		Status:      state.RunnerActive,
		PackageDir:  req.RunnerDir,
		CreatedAt:   now,
		RiskLevel:   risk,
	})
	store.InsertSecret(req.Name, state.SecretRecord{
		Runner:        req.Name,
		Kind:          req.Kind,
		Address:       req.Address,
		Probe:         probe,
		ProbeBody:     probeBody,
		CiphertextHex: ciphertextHex,
		CreatedAt:     now,
	})
	if err := store.Save(); err != nil {
		os.Remove(filepath.Join(req.RunnerDir, identityFile))
		os.Remove(filepath.Join(req.RunnerDir, wire.SECRETS_FILE))
		store.RemoveRunner(req.Name)
		store.RemoveSecret(req.Name)
		return nil, err
	}
	return &ProvisionResult{
		Name:        req.Name,
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkeyHex,
		PackageDir:  req.RunnerDir,
	}, nil
}

// readdressRunner updates a LIVE runner's substrate address — the re-adopt
// path, where the runner identity and its sealed credential survive but the
// host underneath rotated. The state's secret row and the package's targets
// entry move together (the row is the CP's dial record, the package the
// runner's own); the sealed blob is untouched. The runner's TOFU known_hosts
// is keyed by address, so the new host is a fresh trust-on-first-use — no
// stale entry rejects it.
func readdressRunner(store *state.StateStore, r state.RunnerRecord, req *ProvisionRequest) (*ProvisionResult, error) {
	sec, ok := store.GetSecret(req.Name)
	if !ok {
		return nil, fmt.Errorf("runner %s already exists but holds no secret record — revoke and re-provision it", req.Name)
	}
	// The request's Kind is ignored on this path (the runner keeps its own) —
	// so the empty-address guard keys on the kind the runner actually IS,
	// not the caller's declared one.
	if sec.Kind != "local" && strings.TrimSpace(req.Address) == "" {
		return nil, fmt.Errorf("runner %s: an empty address would wipe the door's endpoint (the runner is a %q door)", req.Name, sec.Kind)
	}
	if sec.Address == req.Address {
		return nil, fmt.Errorf("runner %s already exists", req.Name)
	}
	pkg, err := wire.Load(r.PackageDir)
	if err != nil {
		return nil, fmt.Errorf("runner %s package: %w", req.Name, err)
	}
	t, ok := pkg.Targets[req.Name]
	if !ok {
		return nil, fmt.Errorf("runner %s package holds no target %q", req.Name, req.Name)
	}
	old := sec.Address
	t.Address = req.Address
	pkg.Targets[req.Name] = t
	if err := pkg.WriteToDir(r.PackageDir); err != nil {
		return nil, err
	}
	sec.Address = req.Address
	store.InsertSecret(req.Name, sec)
	// The capability record is the REBUILD source for a door: the build's
	// re-assert feeds rec.Address back into the package (reSealRotating →
	// setTargetAddress) — a record left on the old address silently reverts
	// this re-address on the next build. Move it with the rest.
	if cap, ok := store.GetCapability(req.Name); ok && cap.Address != "" {
		cap.Address = req.Address
		if err := store.InsertCapability(req.Name, cap); err != nil {
			// Roll the rows back — they pre-existed. The capability may have
			// moved before the error; restore it like the secret row.
			sec.Address = old
			store.InsertSecret(req.Name, sec)
			if cap.Address == req.Address {
				cap.Address = old
				_ = store.InsertCapability(req.Name, cap)
			}
			_ = store.Save()
			t.Address = old
			pkg.Targets[req.Name] = t
			_ = pkg.WriteToDir(r.PackageDir)
			return nil, fmt.Errorf("move capability record: %w", err)
		}
	}
	if err := store.Save(); err != nil {
		// Roll the package + records back — they pre-existed, so restore
		// their old addresses rather than removing anything.
		t.Address = old
		pkg.Targets[req.Name] = t
		if p2, e2 := wire.Load(r.PackageDir); e2 == nil {
			p2.Targets[req.Name] = t
			_ = p2.WriteToDir(r.PackageDir)
		}
		sec.Address = old
		store.InsertSecret(req.Name, sec)
		if cap, ok := store.GetCapability(req.Name); ok && cap.Address == req.Address {
			cap.Address = old
			_ = store.InsertCapability(req.Name, cap)
		}
		_ = store.Save()
		return nil, err
	}
	fmt.Printf("runner %s already exists — re-addressed %s -> %s (identity + credential unchanged)\n", req.Name, old, req.Address)
	return &ProvisionResult{
		Name:        req.Name,
		NostrPubkey: r.NostrPubkey,
		EncPubkey:   r.EncPubkey,
		PackageDir:  r.PackageDir,
		Readdressed: true,
		OldAddress:  old,
	}, nil
}

// EnrollRunner registers a SELF-HOSTED runner — one whose identity was minted
// ON its own host (the runner-client's `runner enroll`) and whose pubkeys were
// presented through an audited surface (provision_runner hosted=self). The CP
// records the pubkeys and seals credentials TO them; it never sees a private
// key, ships no package (no PackageDir), and starts no unit — the target runs
// its own process. mcpAddr (host:port, optional) feeds the console's readiness
// probe across the LAN.
func EnrollRunner(store *state.StateStore, name, kind, address, nostrPubkey, encPubkey, mcpAddr string) (*state.RunnerRecord, error) {
	if req := name; req == "" || strings.Contains(req, "/") || strings.HasPrefix(req, ".") || req == "local" {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name (no '/', no leading '.')", req)
	}
	if !IsPubkey(nostrPubkey) {
		return nil, fmt.Errorf("invalid nostr pubkey %q: must be 64 hex chars", nostrPubkey)
	}
	if !IsPubkey(encPubkey) {
		return nil, fmt.Errorf("invalid enc pubkey %q: must be 64 hex chars", encPubkey)
	}
	if r, ok := store.GetRunner(name); ok {
		if r.Status == state.RunnerRevoked {
			return nil, fmt.Errorf("runner %s is revoked — enroll under a new name", name)
		}
		return nil, fmt.Errorf("runner %s already exists", name)
	}
	// A "pending" placeholder sealed to the PRESENTED key — the same empty
	// shell an api door ships; the console's fill replaces it with the real
	// credential (RotateSecretSelfHosted).
	encPub, err := hexToArr(encPubkey)
	if err != nil {
		return nil, err
	}
	sealed, err := crypto.Seal(encPub[:], []byte(name), []byte("pending"))
	if err != nil {
		return nil, err
	}
	ciphertextHex := hex.EncodeToString(sealed)
	now := uint64(time.Now().Unix())
	rec := state.RunnerRecord{
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkey,
		Status:      state.RunnerActive,
		CreatedAt:   now,
		RiskLevel:   defaultRisk(kind),
	}
	if mcpAddr != "" {
		a := mcpAddr
		rec.McpAddr = &a
	}
	store.InsertRunner(name, rec)
	store.InsertSecret(name, state.SecretRecord{
		Runner: name, Kind: kind, Address: address,
		CiphertextHex: ciphertextHex, CreatedAt: now,
	})
	if err := store.Save(); err != nil {
		store.RemoveRunner(name)
		store.RemoveSecret(name)
		return nil, err
	}
	return &rec, nil
}

// identityGenerate writes a fresh identity.json into dir and returns the
// enc pubkey hex. Reproduces Identity::generate + write_to_dir.
func identityGenerate(dir string) (string, error) {
	if err := wire.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	// Use the wire SecretPackage write discipline for consistency (the
	// identity file too is secret material, written 0600 atomically).
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	fillRand(nostrSecret)
	fillRand(encSecret)
	nn, err := crypto.PubkeyFromSecret(nostrSecret)
	if err != nil {
		return "", err
	}
	// X25519 enc pubkey from the enc secret.
	encPub, err := x25519Pub(encSecret)
	if err != nil {
		return "", err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	_ = nn
	return writeIdentityJSON(dir, doc, encPub)
}

func fillRand(b []byte) {
	_, _ = cryptoRandRead(b)
}

// writeIdentityJSON writes identity.json (0600 atomic) and returns enc pubkey.
func writeIdentityJSON(dir string, doc map[string]string, encPub string) (string, error) {
	return encPub, wire.WriteJSON0600(filepath.Join(dir, identityFile), doc)
}

// x25519Pub derives the X25519 public key hex from a 32-byte secret.
func x25519Pub(secret []byte) (string, error) {
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(pub), nil
}

// loadIdentity reads the pubkeys/secrets from an identity.json.
func loadIdentity(dir string) (nostrPubkey, encPubkeyHex string, encSecret []byte, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return
	}
	var doc struct {
		NostrSecretHex string `json:"nostr_secret_hex"`
		EncSecretHex   string `json:"enc_secret_hex"`
	}
	if err = jsonUnmarshal(raw, &doc); err != nil {
		return
	}
	ns, err := hex.DecodeString(doc.NostrSecretHex)
	if err != nil {
		return
	}
	nostrPubkey, err = crypto.PubkeyFromSecret(ns)
	if err != nil {
		return
	}
	encSecret, err = hex.DecodeString(doc.EncSecretHex)
	if err != nil {
		return
	}
	encPubBytes, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		return
	}
	encPubkeyHex = hex.EncodeToString(encPubBytes)
	return
}

// SecretCreatorRequest is the second Kind of request (step 13):
// name + nostr_pubkey/enc_pubkey + package_dir. Kept for completeness with
// the Rust provisioner surface; provision_runner covers the main path.
type SecretCreatorRequest struct {
	Name        string
	NostrPubkey string
	EncPubkey   string
	PackageDir  string
}

// cryptoRandRead is the CSPRNG backing fillRand.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// jsonUnmarshal wraps encoding/json.Unmarshal.
func jsonUnmarshal(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
