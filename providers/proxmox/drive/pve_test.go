package drive

import (
	"strings"
	"testing"
)

// repointRun simulates PVE's storage.cfg for the local-lvm probe/edit scripts.
func repointRun(cfg *string) func(script string, timeoutS uint64) (string, error) {
	return func(script string, timeoutS uint64) (string, error) {
		switch {
		case strings.Contains(script, "awk -v tp="):
			*cfg = strings.Replace(*cfg, "thinpool data", "thinpool freehold-thin", 1)
			return "", nil
		case strings.Contains(script, "grep -A2"):
			for _, l := range strings.Split(*cfg, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "thinpool ") {
					return strings.TrimPrefix(strings.TrimSpace(l), "thinpool "), nil
				}
			}
			return "", nil
		}
		return "", nil
	}
}

func TestRepointLocalLvmCarvesAndReadsBack(t *testing.T) {
	cfg := "dir: local\n\tpath /var/lib/vz\n\nlvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n"
	if err := RepointLocalLvm(repointRun(&cfg), "freehold-thin"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "thinpool freehold-thin") || strings.Contains(cfg, "thinpool data") {
		t.Errorf("local-lvm must point at freehold-thin:\n%s", cfg)
	}
	if !strings.Contains(cfg, "dir: local") || !strings.Contains(cfg, "vgname pve") {
		t.Errorf("the edit must be scoped:\n%s", cfg)
	}
}

func TestRepointLocalLvmAlreadyCorrect(t *testing.T) {
	cfg := "lvmthin: local-lvm\n\tthinpool freehold-thin\n\tvgname pve\n"
	if err := RepointLocalLvm(repointRun(&cfg), "freehold-thin"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "thinpool freehold-thin") {
		t.Errorf("already-correct pointer must be left untouched:\n%s", cfg)
	}
}

func TestRepointLocalLvmNoBlockErrors(t *testing.T) {
	cfg := "dir: local\n\tpath /var/lib/vz\n"
	err := RepointLocalLvm(repointRun(&cfg), "freehold-thin")
	if err == nil || !strings.Contains(err.Error(), "no `lvmthin: local-lvm`") {
		t.Errorf("missing local-lvm block must be a hard error, got %v", err)
	}
}
