package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"freehold/orchestrator/internal/config"
)

// strPtr/uint32Ptr are small helpers for the pointer-typed config fields.
func strPtr(s string) *string { return &s }
func u32Ptr(u uint32) *uint32 { return &u }

// testCfg builds a minimal full config for Services/DATA tests.
func testCfg() *config.Config {
	return &config.Config{
		Domain:   "example.test",
		RelayURL: "https://example.test",
		CPURL:    "https://cp.example.test",
		Runner:   config.RunnerRef{Addr: "127.0.0.1:8787", Pubkey: "aa", Target: "box"},
		Lxc: config.LxcSpec{
			Relay: config.LxcGuest{Vmid: u32Ptr(100), Ip: strPtr("192.168.30.8/24")},
			Cp:    config.LxcGuest{Vmid: u32Ptr(101), Ip: strPtr("192.168.30.9/24")},
			K3s:   config.LxcGuest{Vmid: u32Ptr(102), Ip: strPtr("192.168.30.240/24")},
		},
		Managed: []string{"relay", "cp", "k3s"},
	}
}

// TestBuildCerts exercises the Certs view: no cert -> empty; with an expiry +
// issuer -> one row with the status derived from the expiry.
func TestBuildCerts(t *testing.T) {
	cfg := testCfg()
	m := &Model{Mode: ModeRunning, cfg: cfg}
	m.buildCerts(cfg)
	if len(m.Certs) != 0 {
		t.Fatalf("no cert on record should yield 0 rows, got %+v", m.Certs)
	}

	cfg.Caddy = config.CaddySpec{
		URL:        "https://relay.example.test",
		CertExpiry: time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339),
		CertIssuer: "route53",
	}
	m.buildCerts(cfg)
	if len(m.Certs) != 1 {
		t.Fatalf("want 1 cert row, got %d", len(m.Certs))
	}
	c := m.Certs[0]
	if c.URL != "https://relay.example.test" || c.Issuer != "route53" {
		t.Errorf("cert row = %+v", c)
	}
	if !strings.Contains(c.Domain, "example.test") {
		t.Errorf("domain = %q", c.Domain)
	}
	if !strings.Contains(c.Status, "valid") {
		t.Errorf("status = %q, want valid", c.Status)
	}

	// expired -> red status
	cfg.Caddy.CertExpiry = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	m.buildCerts(cfg)
	if !strings.Contains(m.Certs[0].Status, "EXPIRED") {
		t.Errorf("expired status = %q", m.Certs[0].Status)
	}
}

// TestBuildServicesK3sRow confirms the Services view renders the k3s row —
// name, recorded CIDR ip stripped, API URL — with its live status.
func TestBuildServicesK3sRow(t *testing.T) {
	m := &Model{Mode: ModeRunning, RelayLive: true, CPLive: true, K3sLive: true, RunnerReach: true}
	m.buildServices(testCfg())
	if len(m.Services) != 3 {
		t.Fatalf("want 3 service rows (relay/cp/k3s), got %d", len(m.Services))
	}
	var k3s, relay, cp ServiceRow
	for _, s := range m.Services {
		switch s.Name {
		case "k3s (kube)":
			k3s = s
		case "relay":
			relay = s
		case "control plane":
			cp = s
		}
	}
	if k3s.Name == "" {
		t.Fatal("k3s row missing from Services view")
	}
	if k3s.URL != "https://192.168.30.240:6443" {
		t.Errorf("k3s URL must strip the CIDR prefix, got %q", k3s.URL)
	}
	if !strings.Contains(k3s.Status, "live") {
		t.Errorf("k3s status should show live, got %q", k3s.Status)
	}
	if !strings.Contains(k3s.Where, "LXC 102") || !strings.Contains(k3s.Where, "192.168.30.240") {
		t.Errorf("k3s where should carry vmid + ip, got %q", k3s.Where)
	}
	if relay.Where != "LXC 100 · 192.168.30.8" {
		t.Errorf("relay where should strip CIDR, got %q", relay.Where)
	}
	if !strings.Contains(cp.Status, "live") {
		t.Errorf("cp status should reflect CPLive, got %q", cp.Status)
	}
}

// TestBuildServicesK3sDown flips the probe off: the row must show down.
func TestBuildServicesK3sDown(t *testing.T) {
	m := &Model{Mode: ModeRunning}
	m.buildServices(testCfg())
	for _, s := range m.Services {
		if s.Name == "k3s (kube)" && !strings.Contains(s.Status, "down") {
			t.Errorf("k3s down probe should render down, got %q", s.Status)
		}
	}
}

// TestRenderProbesHasK3s confirms the probe strip lists the k3s API.
func TestRenderProbesHasK3s(t *testing.T) {
	m := &Model{RelayLive: true, CPLive: true, K3sLive: true, RunnerReach: true}
	out := renderProbes(m)
	if !strings.Contains(out, "k3s") {
		t.Errorf("probe strip must include k3s, got %q", out)
	}
}

// TestDataViewRendersCapacityAndFill verifies the DATA view renders the
// capacity line + fill column with the traffic-light thresholds, and the
// empty state when no snapshot exists.
func TestDataViewRendersCapacityAndFill(t *testing.T) {
	m := &Model{
		Mode: ModeRunning, Converged: true, ActiveView: ViewData,
		DataCap: "vg pve · 120.0G of 931.5G · 811.5G free", DataAt: time.Now(),
		Storage: []DataRow{
			{Role: "cp", Mount: "/srv/data/cp", Size: "10.0G", Used: "9.5G", Fill: "95%", Source: "/freehold/x/cp", Live: "mounted"},
		},
	}
	out := m.View()
	if !strings.Contains(out, "DATA · vg pve") {
		t.Errorf("DATA title must carry the capacity line, got:\n%s", out)
	}
	if !strings.Contains(out, "mount point") || !strings.Contains(out, "fill") || !strings.Contains(out, "source (host)") {
		t.Errorf("DATA header must match the Rust columns, got:\n%s", out)
	}
	if !strings.Contains(out, "95%") || !strings.Contains(out, "/srv/data/cp") {
		t.Errorf("DATA row missing fill/mount, got:\n%s", out)
	}

	// empty state
	m2 := &Model{Mode: ModeRunning, Converged: true, ActiveView: ViewData}
	if !strings.Contains(m2.View(), "no durable plane on record") {
		t.Errorf("empty DATA must show the no-plane notice, got:\n%s", m2.View())
	}
}

// TestFillStyle checks the traffic-light boundaries (Rust fill_color).
// Styles hold funcs, so equality is asserted on rendered sentinels.
func TestFillStyle(t *testing.T) {
	rendered := func(pct uint64) string { return fillStyle(pct).Render("x") }
	green, yellow, red := styleGreen.Render("x"), styleYellow.Render("x"), styleRed.Render("x")
	if rendered(69) != green {
		t.Error("69% must be green")
	}
	if rendered(70) != yellow || rendered(89) != yellow {
		t.Error("70-89% must be yellow")
	}
	if rendered(90) != red || rendered(100) != red {
		t.Error(">=90% must be red")
	}
}

// TestWKeyNeedsLogin confirms `w` without a console session yields the
// press-l notice instead of a no-op.
func TestWKeyNeedsLogin(t *testing.T) {
	m := &Model{Mode: ModeRunning, Converged: true}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	if !strings.Contains(m.Msg, "not logged in") {
		t.Errorf("w without login must say press l first, got %q", m.Msg)
	}
}

// TestRKeyIsRealRefresh confirms `r` is NOT the old "data refresh
// requested" stub — it re-runs load (with a missing config it lands in
// bootstrap mode rather than printing a placeholder).
func TestRKeyIsRealRefresh(t *testing.T) {
	m := &Model{Mode: ModeRunning, Converged: true, CfgPath: "/nonexistent/config.toml", LastRef: time.Now()}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.Msg == "data refresh requested" {
		t.Fatal("r must not be the stub string anymore")
	}
	if m.Mode != ModeBootstrap || m.HasConfig {
		t.Errorf("r must re-run load; missing config should land in bootstrap, got mode=%v hasConfig=%v", m.Mode, m.HasConfig)
	}
}

// TestGuestLocation covers the location column's four combinations.
func TestGuestLocation(t *testing.T) {
	if got := guestLocation(u32Ptr(100), strPtr("1.2.3.4/24")); got != "LXC 100 · 1.2.3.4" {
		t.Errorf("vmid+cidr: %q", got)
	}
	if got := guestLocation(u32Ptr(100), nil); got != "LXC 100" {
		t.Errorf("vmid only: %q", got)
	}
	if got := guestLocation(nil, strPtr("1.2.3.4")); got != "1.2.3.4" {
		t.Errorf("ip only: %q", got)
	}
	if got := guestLocation(nil, nil); got != "—" {
		t.Errorf("neither: %q", got)
	}
}

// TestParseDnsList extracts records from `dns list` output (marker + metadata
// lines ignored) — the live DNS panel's pure parser.
func TestParseDnsList(t *testing.T) {
	out := `cp 192.168.30.9
  source: record_lxc cp · created: 123
relay 192.168.30.8
  source: record_lxc relay · created: 124
--- addn-hosts ---
192.168.30.9 cp
192.168.30.9 cp.darcydev.net
192.168.30.8 relay
192.168.30.8 relay.darcydev.net
`
	rows := parseDnsList(out)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Name != "cp" || rows[0].IP != "192.168.30.9" || rows[0].Source != "resolver (live)" {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].Name != "relay" || rows[1].IP != "192.168.30.8" {
		t.Errorf("row1 = %+v", rows[1])
	}
	// empty output -> no rows (caller falls back to the mirror)
	if got := parseDnsList("(no dns records — the resolver forwards everything upstream)\n"); len(got) != 0 {
		t.Errorf("empty list should parse to 0 rows, got %+v", got)
	}
	// the wildcard apex line surfaces as *.<apex>
	wc := parseDnsList("wildcard  *.freehold-test.darcydev.net\t192.168.30.7\n  source: record-caddy · created: 123\n--- addn-hosts ---\n192.168.30.7\n")
	if len(wc) != 1 || wc[0].Name != "*.freehold-test.darcydev.net" || wc[0].IP != "192.168.30.7" || wc[0].Source != "wildcard (live)" {
		t.Errorf("wildcard row = %+v", wc)
	}
	// `(none)` wildcard is not a row
	if got := parseDnsList("wildcard  (none)\n"); len(got) != 0 {
		t.Errorf("(none) should yield 0 rows, got %+v", got)
	}
}

// TestRunnerSourceDefaultsToCpAndToggles covers the Runners view source:
// new models default to the CP (console) source, and `s` toggles to local
// and back, re-filling the view each time.
func TestRunnerSourceDefaultsToCpAndToggles(t *testing.T) {
	cfg := testCfg()
	if cfg == nil {
		t.Fatal("testCfg returned nil")
	}
	m := &Model{Mode: ModeRunning, cfg: cfg}
	if m.RunnerSource == "" {
		m.RunnerSource = RunnerSourceCP
	}
	m.refreshRunners(cfg)
	if m.RunnerSource != RunnerSourceCP {
		t.Fatalf("default source = %q, want cp", m.RunnerSource)
	}
	// not logged into a console -> the CP source shows the login hint row.
	if len(m.Runners) == 0 || !strings.Contains(m.Runners[0].Name, "not logged") {
		t.Fatalf("cp source without login should hint, got %+v", m.Runners)
	}
	// toggle to local via the same switch the `s` key drives.
	toggle := func() {
		if m.RunnerSource == RunnerSourceLocal {
			m.RunnerSource = RunnerSourceCP
		} else {
			m.RunnerSource = RunnerSourceLocal
		}
		m.refreshRunners(cfg)
	}
	toggle()
	if m.RunnerSource != RunnerSourceLocal {
		t.Fatalf("after toggle source = %q, want local", m.RunnerSource)
	}
	if got := m.runnerSourceLabel(); !strings.Contains(got, "loopback") {
		t.Errorf("local label = %q", got)
	}
	toggle()
	if m.RunnerSource != RunnerSourceCP {
		t.Fatalf("second toggle source = %q, want cp", m.RunnerSource)
	}
	// the Runners view title carries the source label + toggle hint.
	m.ActiveView = ViewRunners
	out := m.View()
	if !strings.Contains(out, "Runners · ") || !strings.Contains(out, "s toggles") {
		t.Errorf("runners view should show the source toggle hint:\n%s", out)
	}
}

// TestVmidForRole covers the plane mount role -> vmid map.
func TestVmidForRole(t *testing.T) {
	cfg := testCfg()
	for role, want := range map[string]uint32{"relay": 100, "cp": 101, "k3s": 102} {
		got := vmidForRole(cfg, role)
		if got == nil || *got != want {
			t.Errorf("vmidForRole(%s) = %v, want %d", role, got, want)
		}
	}
	if vmidForRole(cfg, "other") != nil {
		t.Error("unknown role must map to nil")
	}
}
