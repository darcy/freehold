package box

import (
	"os"
	"testing"
)

func TestDumpDoorKey(t *testing.T) {
	pem, err := DoorKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/opencode/doorcheck/door.pem", pem, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("ops dir:", OpsDir())
}
