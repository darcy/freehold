package cli

import (
	"reflect"
	"testing"
)

func TestStaleRosterMembers(t *testing.T) {
	server := "fa0000"
	op := "1d0000"
	agent := "6f0000"
	cur := []string{server, op, agent, "96699c6b..."} // 96699... is a rotated-out prior CPA
	got := staleRosterMembers(cur, server, op, []string{agent})
	want := []string{"96699c6b..."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}

	// No stale members when every roster member is in the desired set.
	if got := staleRosterMembers(cur, server, op, []string{agent, "96699c6b..."}); len(got) != 0 {
		t.Fatalf("expected no stale members, got %v", got)
	}
}
