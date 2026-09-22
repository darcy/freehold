package acceptance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freehold/contract/relay"
	"freehold/contract/wire"
	provisioner "freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// insertRunner seeds a deterministic runner + secret record (per-name d-tag).
func insertRunner(t *testing.T, store *state.StateStore, name string, status state.RunnerStatus) {
	t.Helper()
	store.InsertRunner(name, state.RunnerRecord{
		NostrPubkey: hex64(name[0]),
		EncPubkey:   hex64('b'),
		Status:      status,
		PackageDir:  "/tmp/pkg",
		CreatedAt:   5,
	})
	rotated := uint64(7)
	store.InsertSecret(name, state.SecretRecord{
		Runner: name, Kind: "ssh", Address: "host:22",
		CiphertextHex: "aa", CreatedAt: 5, RotatedAt: &rotated,
	})
}

func profileMsgCount(rs *RelayState) int {
	n := 0
	for _, e := range rs.Events() {
		if e.Kind == wire.ChannelMessage && containsStr(e.tagAll("t"), relay.ProfileMessageTag()) {
			n++
		}
	}
	return n
}

func TestChannelSyncAndQueryMetaRoundtrip(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	consoleSecret, consolePub := ensureConsoleIdentity(t, cpDir)
	relayURL, _ := spawnRelay(t)

	insertRunner(t, store, "relaybox", state.RunnerActive)
	if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}

	profiles, err := relay.QueryRunnerMetas(relayURL, consolePub, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d: %v", len(profiles), profiles)
	}
	p := profiles[0]
	if p.Name != "relaybox" || p.Kind != "ssh" || p.Address != "host:22" || p.Status != "active" {
		t.Fatalf("profile: %+v", p)
	}
	if p.NostrPubkey != hex64('r') || p.EncPubkey != hex64('b') {
		t.Fatalf("profile pubkeys: %+v", p)
	}
	if p.Secret != "relaybox" {
		t.Fatalf("secret name only, got %q", p.Secret)
	}
	if p.RotatedAt == nil || *p.RotatedAt != 7 {
		t.Fatalf("rotated_at: %v", p.RotatedAt)
	}

	roster, err := relay.QueryChannelRoster(relayURL, RelayPubkey(), hex64('r'), consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(roster, hex64('r')) {
		t.Fatal("the runner must be a member of its own channel")
	}
	if !containsStr(roster, consolePub) {
		t.Fatal("the console owner is a member of the channel it created")
	}
}

func TestRosterReadRequiresMembership(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	ensureConsoleIdentity(t, cpDir)
	relayURL, _ := spawnRelay(t)

	insertRunner(t, store, "relaybox", state.RunnerActive)
	if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}

	// A caller who is not a member (and not the owner) must be refused.
	outsider := make([]byte, 32)
	for i := range outsider {
		outsider[i] = 0x7f
	}
	if _, err := relay.QueryChannelRoster(relayURL, RelayPubkey(), hex64('r'), outsider); err == nil {
		t.Fatal("non-member roster read must fail closed")
	} else if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "membership") {
		t.Fatalf("expected a membership refusal, got %v", err)
	}
}

func TestRevokeFlipsMetaAndCutsOffRunner(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	consoleSecret, consolePub := ensureConsoleIdentity(t, cpDir)
	relayURL, rs := spawnRelay(t)

	insertRunner(t, store, "relaybox", state.RunnerActive)
	if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}
	afterActive := profileMsgCount(rs)

	if err := store.SetRunnerStatus("relaybox", state.RunnerRevoked); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.RevokeRunnerChannel(store, relayURL, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}
	if got := profileMsgCount(rs); got != afterActive+1 {
		t.Fatalf("revoke must re-publish the profile once more: %d -> %d", afterActive, got)
	}

	profiles, err := relay.QueryRunnerMetas(relayURL, consolePub, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Status != "revoked" {
		t.Fatalf("fold sees one revoked meta: %+v", profiles)
	}

	roster, err := relay.QueryChannelRoster(relayURL, RelayPubkey(), hex64('r'), consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(roster, hex64('r')) {
		t.Fatal("revoked runner must exit its own channel")
	}
}

func TestRogueAuthorCannotMintOrClobberMetas(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	consoleSecret, consolePub := ensureConsoleIdentity(t, cpDir)
	relayURL, rs := spawnRelay(t)

	insertRunner(t, store, "relaybox", state.RunnerActive)
	if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}

	h := relay.RunnerChannelID(hex64('r'))
	rogueSecret := make([]byte, 32)
	for i := range rogueSecret {
		rogueSecret[i] = 3
	}
	rogue := &relay.RunnerProfile{
		Name: "relaybox", Kind: "ssh", Address: "evil:22", Status: "revoked",
		NostrPubkey: hex64('r'), EncPubkey: hex64('b'), Secret: "relaybox", CreatedAt: 5,
	}
	content, _ := json.Marshal(rogue)
	ts := time.Now().Unix() + 100
	tags := [][]string{{"h", h}, {"d", h}, {"t", relay.ProfileMessageTag()}}
	pk, id, sig, err := wire.SignEvent(rogueSecret, wire.ChannelMessage, ts, tags, string(content))
	if err != nil {
		t.Fatal(err)
	}
	rs.AppendEvent(relayEvent{ID: id, Pubkey: pk, CreatedAt: ts, Kind: wire.ChannelMessage, Tags: tags, Content: string(content), Sig: sig})

	profiles, err := relay.QueryRunnerMetas(relayURL, consolePub, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Status != "active" || profiles[0].Address != "host:22" {
		t.Fatalf("rogue meta must be ignored: %+v", profiles)
	}
}

// fold queries the relay and rebuilds a fresh store from the current metas.
func fold(t *testing.T, relayURL, consolePub string, consoleSecret []byte, dir string) (*state.StateStore, []*relay.RunnerProfile) {
	t.Helper()
	store := openStore(t, dir)
	profiles, err := relay.QueryRunnerMetas(relayURL, consolePub, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	runners := map[string]state.RunnerRecord{}
	secrets := map[string]state.SecretRecord{}
	for _, p := range profiles {
		status := state.RunnerActive
		if p.Status == "revoked" {
			status = state.RunnerRevoked
		}
		runners[p.Name] = state.RunnerRecord{
			NostrPubkey: p.NostrPubkey, EncPubkey: p.EncPubkey, Status: status,
			PackageDir: "", CreatedAt: p.CreatedAt, RiskLevel: p.Risk,
		}
		secrets[p.Name] = state.SecretRecord{
			Runner: p.Name, Kind: p.Kind, Address: p.Address,
			CiphertextHex: "", CreatedAt: p.CreatedAt, RotatedAt: p.RotatedAt,
		}
	}
	if err := store.RebuildFrom(runners, secrets); err != nil {
		t.Fatal(err)
	}
	return store, profiles
}

func TestRebuildFoldsSnapshotsIdempotently(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	consoleSecret, consolePub := ensureConsoleIdentity(t, cpDir)
	relayURL, _ := spawnRelay(t)

	for i, tc := range []struct {
		name   string
		status state.RunnerStatus
	}{{"alpha", state.RunnerActive}, {"beta", state.RunnerRevoked}} {
		insertRunner(t, store, tc.name, tc.status)
		if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, tc.name, cpDir); err != nil {
			t.Fatal(err)
		}
		rec, _ := store.GetRunner(tc.name)
		sec, _ := store.GetSecret(tc.name)
		status := "active"
		var rotated *uint64
		if tc.status == state.RunnerRevoked {
			status = "revoked"
			r := uint64(9)
			rotated = &r
		}
		created := uint64(10 + i)
		profile := &relay.RunnerProfile{
			Name: tc.name, Kind: sec.Kind, Address: "host" + string(rune('1'+i)) + ":22",
			Status: status, NostrPubkey: rec.NostrPubkey, EncPubkey: rec.EncPubkey,
			Secret: tc.name, CreatedAt: created, RotatedAt: rotated,
		}
		if err := relay.PublishRunnerMeta(relayURL, consoleSecret, profile); err != nil {
			t.Fatal(err)
		}
	}

	store1, view1 := fold(t, relayURL, consolePub, consoleSecret, filepath.Join(base, "fold1"))
	store2, view2 := fold(t, relayURL, consolePub, consoleSecret, filepath.Join(base, "fold2"))

	if len(view1) != 2 {
		t.Fatalf("expected 2 runners, got %d", len(view1))
	}
	snap1 := store1.Snapshot()
	snap2 := store2.Snapshot()
	if len(snap1.Runners) != len(snap2.Runners) {
		t.Fatal("re-run must converge to the same runner set")
	}
	for name, r1 := range snap1.Runners {
		r2, ok := snap2.Runners[name]
		if !ok || r1.Status != r2.Status || r1.CreatedAt != r2.CreatedAt {
			t.Fatalf("fold not idempotent for %s: %+v vs %+v", name, r1, r2)
		}
	}
	if snap1.Runners["alpha"].Status != state.RunnerActive {
		t.Fatal("alpha must be active")
	}
	if snap1.Runners["beta"].Status != state.RunnerRevoked {
		t.Fatal("beta must be revoked")
	}
	if snap1.Secrets["beta"].RotatedAt == nil || *snap1.Secrets["beta"].RotatedAt != 9 {
		t.Fatal("beta must carry rotated_at 9")
	}
	for _, s := range snap1.Secrets {
		if s.CiphertextHex != "" {
			t.Fatal("restored records carry no ciphertext")
		}
	}
	for _, r := range snap1.Runners {
		if r.PackageDir != "" {
			t.Fatal("restored records carry no package path")
		}
	}
	_ = view2
}

func TestRebuildCarriesRiskLabel(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	consoleSecret, consolePub := ensureConsoleIdentity(t, cpDir)
	relayURL, _ := spawnRelay(t)

	insertRunner(t, store, "gamma", state.RunnerActive)
	if err := provisioner.SyncRunnerChannel(store, relayURL, relayURL, "gamma", cpDir); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.GetRunner("gamma")
	sec, _ := store.GetSecret("gamma")
	created := uint64(time.Now().Unix()) + 100
	risk := "risky-host"
	profile := &relay.RunnerProfile{
		Name: "gamma", Kind: sec.Kind, Address: sec.Address, Status: "active",
		NostrPubkey: rec.NostrPubkey, EncPubkey: rec.EncPubkey, Secret: "gamma",
		CreatedAt: created, Risk: &risk,
	}
	if err := relay.PublishRunnerMeta(relayURL, consoleSecret, profile); err != nil {
		t.Fatal(err)
	}

	folded, _ := fold(t, relayURL, consolePub, consoleSecret, filepath.Join(base, "fold"))
	got, ok := folded.GetRunner("gamma")
	if !ok {
		t.Fatal("gamma must fold")
	}
	if got.RiskLevel == nil || *got.RiskLevel != "risky-host" {
		t.Fatalf("risk level = %v", got.RiskLevel)
	}
	if got.CreatedAt != created {
		t.Fatalf("created_at = %d want %d", got.CreatedAt, created)
	}
}
