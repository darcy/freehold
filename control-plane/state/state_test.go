package state

import (
	"path/filepath"
	"testing"
)

// TestSettingsRoundTrip: SetSettings persists, Open + LoadReadOnly read it
// back, and a state file written WITHOUT the settings group reads as zero
// values (nil Settings) — old worlds unmarshal cleanly, no migration.
func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.SetSettings(&Settings{OperatorTZ: "America/Chicago"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if s := reopened.Settings(); s == nil || s.OperatorTZ != "America/Chicago" {
		t.Fatalf("reopened settings = %+v, want operator_tz America/Chicago", s)
	}

	ro, err := LoadReadOnly(dir)
	if err != nil {
		t.Fatalf("load read-only: %v", err)
	}
	if ro.Settings == nil || ro.Settings.OperatorTZ != "America/Chicago" {
		t.Fatalf("read-only settings = %+v, want operator_tz America/Chicago", ro.Settings)
	}
	if _, err := LoadReadOnly(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("LoadReadOnly on a missing dir must error")
	}
}

// TestValidOperatorTZ: empty (unset) is valid; real IANA names are valid;
// Go's "Local" special case is refused (no pod resolves it — it would fake a
// setting while reading as UTC); garbage is invalid.
func TestValidOperatorTZ(t *testing.T) {
	for tz, want := range map[string]bool{
		"":                true,
		"America/Chicago": true,
		"Europe/Berlin":   true,
		"UTC":             true,
		"Local":           false,
		"Mars/Olympus":    false,
	} {
		if got := ValidOperatorTZ(tz); got != want {
			t.Errorf("ValidOperatorTZ(%q) = %v, want %v", tz, got, want)
		}
	}
}

// TestSettingsNilReadsZero: a pre-settings state file (no settings key) must
// read as nil/zero — the pods-run-UTC default — never an error.
func TestSettingsNilReadsZero(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	ro, err := LoadReadOnly(dir)
	if err != nil {
		t.Fatalf("load read-only: %v", err)
	}
	if ro.Settings != nil && ro.Settings.OperatorTZ != "" {
		t.Fatalf("pre-settings state read %+v, want nil/empty", ro.Settings)
	}
}

// TestMCPCapabilityRoundTrip: InsertMCPCapability persists, Open reads it
// back, re-inserting clears a retirement guard, and Remove drops it.
func TestMCPCapabilityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rec := MCPCapabilityRecord{
		ServerName: "google_sheets_a_ro", Namespace: "mcp", ServicePort: 8000,
		Transport: "http", URL: "http://google-sheets-a-ro.mcp.svc.cluster.local:8000/mcp",
		Scope: "spreadsheet:abc123:readonly", Tools: []string{"read_thing"},
		Grantees: []string{"helper"}, Origin: OriginAgent,
		SecretRef: "google-sheets-a-ro-token", CreatedAt: 7,
	}
	if err := store.InsertMCPCapability("google_sheets_a_ro", rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.InsertRetiredMCP("google_sheets_a_ro", RetiredCapability{RevokedAt: 9, RetiredBy: "agent"}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, retired := store.GetRetiredMCP("google_sheets_a_ro"); !retired {
		t.Fatal("retirement guard must read back")
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.GetMCPCapability("google_sheets_a_ro")
	if !ok || got.ServerName != "google_sheets_a_ro" || got.Transport != "http" ||
		len(got.Grantees) != 1 || got.Grantees[0] != "helper" || !got.AgentProvisioned() {
		t.Fatalf("reopened record = %+v", got)
	}
	if got.MCPSlug() != "google_sheets_a_ro" {
		t.Fatalf("slug = %q", got.MCPSlug())
	}
	// Re-inserting the name clears its retirement guard (the re-enable rule).
	if err := reopened.InsertMCPCapability("google_sheets_a_ro", rec); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if _, retired := reopened.GetRetiredMCP("google_sheets_a_ro"); retired {
		t.Fatal("a re-provisioned MCP capability must clear its retirement guard")
	}
	if err := reopened.RemoveMCPCapability("google_sheets_a_ro"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := reopened.GetMCPCapability("google_sheets_a_ro"); ok {
		t.Fatal("record must be gone after remove")
	}
}

// TestMCPCapabilitiesBackwardCompat: a state file written BEFORE the MCP
// tables existed must load cleanly (nil maps read as empty), never error —
// old worlds need no migration.
func TestMCPCapabilitiesBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.InsertCapability("some-runner", CapabilityRecord{
		Kind: "ssh", Address: "root@h", Port: 8800, Rosters: []string{"compute"},
		CreatedAt: 3,
	}); err != nil {
		t.Fatalf("seed runner capability: %v", err)
	}
	ro, err := LoadReadOnly(dir)
	if err != nil {
		t.Fatalf("load read-only: %v", err)
	}
	if len(ro.MCPCapabilities) != 0 || len(ro.RetiredMCPCapabilities) != 0 {
		t.Fatalf("pre-MCP state must read zero MCP capabilities, got %+v / %+v", ro.MCPCapabilities, ro.RetiredMCPCapabilities)
	}
	// And the same file opens into a live store whose maps are usable.
	live, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := live.InsertMCPCapability("sheet-b-ro", MCPCapabilityRecord{
		ServerName: "sheet-b-ro", Transport: "sse", URL: "http://x/mcp", Origin: OriginOperator,
	}); err != nil {
		t.Fatalf("insert on pre-MCP state: %v", err)
	}
	if _, ok := live.GetMCPCapability("sheet-b-ro"); !ok {
		t.Fatal("inserted MCP record must read back")
	}
	if _, ok := live.GetCapability("some-runner"); !ok {
		t.Fatal("runner capability must survive alongside")
	}
}

// TestCapabilityViewsUnified: the unified listing covers both planes, keeps
// runner and MCP records distinct, and sorts by name then plane.
func TestCapabilityViewsUnified(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.InsertCapability("zz-runner", CapabilityRecord{
		Kind: "local", Address: "local", Port: 8800, Rosters: []string{"data"},
		Origin: OriginOperator, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert runner: %v", err)
	}
	if err := store.InsertMCPCapability("aa-sheet", MCPCapabilityRecord{
		ServerName: "aa-sheet", Transport: "http", URL: "http://aa/mcp",
		Scope: "spreadsheet:x:readonly", Grantees: []string{"helper"},
		Origin: OriginAgent, CreatedAt: 2,
	}); err != nil {
		t.Fatalf("insert mcp: %v", err)
	}
	views := store.CapabilityViews()
	if len(views) != 2 {
		t.Fatalf("want 2 views, got %d: %+v", len(views), views)
	}
	if views[0].Name != "aa-sheet" || views[0].Plane != "mcp" || views[0].Scope == "" {
		t.Fatalf("first view (mcp, name-sorted) = %+v", views[0])
	}
	if views[1].Name != "zz-runner" || views[1].Plane != "runner" || views[1].Scope != "" {
		t.Fatalf("second view (runner) = %+v", views[1])
	}
	// A same-name capability on both planes sorts mcp first (plane tiebreak
	// is plain alphabetical).
	if err := store.InsertMCPCapability("zz-runner", MCPCapabilityRecord{
		ServerName: "zz-runner", Transport: "http", URL: "http://z/mcp", CreatedAt: 4,
	}); err != nil {
		t.Fatalf("insert same-name mcp: %v", err)
	}
	views = store.CapabilityViews()
	if views[1].Name != "zz-runner" || views[1].Plane != "mcp" || views[2].Plane != "runner" {
		t.Fatalf("same-name tiebreak must sort alphabetically by plane: %+v", views)
	}
}
