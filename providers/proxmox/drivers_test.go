package proxmox

import "testing"

// TestFirstFieldBlankLine is the live regression: `pvesm list local` output
// ends in a trailing newline, and the parse loop fed that blank line to
// firstField, which indexed an empty Fields() result and PANICKED the whole
// rebuild. A blank line must yield "" (the caller skips it), never a panic.
func TestFirstFieldBlankLine(t *testing.T) {
	if got := firstField(""); got != "" {
		t.Errorf("blank line = %q, want \"\"", got)
	}
	if got := firstField("   \t "); got != "" {
		t.Errorf("whitespace-only line = %q, want \"\"", got)
	}
	if got := firstField("local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst tzst vztmpl 129954319"); got != "local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst" {
		t.Errorf("volid line = %q", got)
	}
}
