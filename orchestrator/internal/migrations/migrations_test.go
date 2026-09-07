package migrations

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAppliesVerifyGatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp", "migrations.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	var applied []string
	mig := Migration{
		Name:   "001-abc",
		Apply:  func() error { applied = append(applied, "001"); return nil },
		Verify: func() error { return nil },
	}
	res, err := st.Run([]Migration{mig})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].OK || !res[0].Applied {
		t.Fatalf("expected 1 OK applied result, got %+v", res)
	}
	if !st.Done("001-abc") {
		t.Fatal("migration should be done after apply+verify")
	}

	// Idempotent: a fresh State over the SAME durable file skips it.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Pending([]Migration{mig}); len(got) != 0 {
		t.Fatal("done migration must not be pending on re-open")
	}
}

func TestVerifyFailureRetriesNotDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	mig := Migration{
		Name:   "002-verify-red",
		Apply:  func() error { applyCalls++; return nil },
		Verify: func() error { return fmt.Errorf("not converged yet") },
	}
	res, err := st.Run([]Migration{mig})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].OK || !res[0].Applied {
		t.Fatalf("verify-failed migration must be not-OK but still applied, got %+v", res[0])
	}
	if st.Done("002-verify-red") {
		t.Fatal("verify failure must NOT mark the migration done")
	}
	// Next run RETRIES it (Apply is idempotent) because it's still pending.
	next, err := st.Run([]Migration{mig})
	if err != nil {
		t.Fatal(err)
	}
	if !next[0].Applied {
		t.Fatal("pending-verify migration must be re-applied on the next run")
	}
	if applyCalls != 2 {
		t.Fatalf("expected 2 apply calls (retry), got %d", applyCalls)
	}
}

func TestApplyFailureStopsPendingsAndKeepsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ok := Migration{Name: "000-ok", Apply: func() error { return nil }, Verify: func() error { return nil }}
	boom := Migration{Name: "003-boom", Apply: func() error { return fmt.Errorf("apply exploded") }}
	// Order: ok runs first; then boom fails and stops the run.
	if _, err := st.Run([]Migration{ok, boom}); err == nil {
		t.Fatal("expected an error when a migration's apply fails")
	}
	if !st.Done("000-ok") {
		t.Fatal("the migration before the failure should still be done")
	}
	if st.Done("003-boom") {
		t.Fatal("failed migration must not be done")
	}
	// The failed migration is still pending (retried, not skipped).
	if got := st.Pending([]Migration{boom}); len(got) != 1 {
		t.Fatal("failed migration must remain pending")
	}
	// State file survives (durable).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger not persisted: %v", err)
	}
}
