package drive

import (
	"fmt"
	"strings"

	"freehold/platform/provisioning/planebase"
)

// PVE `storage.cfg` facts shared by the detector (bootstrap) and the driver
// (drive): the stock `local-lvm` storage's thinpool pointer is a whitespace-
// formatted block, and `pvesm set local-lvm --thinpool` is NOT supported by
// PVE ("Unknown option: thinpool" — live-verified), so the pointer is read and
// written with scoped awk. Living here keeps ONE copy for both callers and
// avoids a bootstrap<->drive import cycle.

// LocalLvmProbeScript prints the pool PVE's stock local-lvm storage currently
// points at. Empty output = no local-lvm block (or no thinpool line).
const LocalLvmProbeScript = "grep -A2 '^lvmthin: local-lvm$' /etc/pve/storage.cfg | awk '/^[[:space:]]*thinpool[[:space:]]/{print $2; exit}'"

// LocalLvmRepointScript rewrites ONLY the thinpool line inside the local-lvm
// block of /etc/pve/storage.cfg (scoped awk: the in-block flag sets on the
// header, clears on a blank line), replacing the file through a tmp copy.
func LocalLvmRepointScript(pool string) string {
	return fmt.Sprintf(`awk -v tp=%s '/^lvmthin: local-lvm$/{inb=1; print; next} /^[[:space:]]*$/{inb=0} inb && /^[[:space:]]*thinpool[[:space:]]/{print "\tthinpool " tp; next} {print}' /etc/pve/storage.cfg > /tmp/fh-storage.cfg && cat /tmp/fh-storage.cfg > /etc/pve/storage.cfg && rm -f /tmp/fh-storage.cfg`, pool)
}

// LocalLvmRidersScript prints the LV names riding the pool local-lvm currently
// points at, one per line (empty when there is no pointer or no riders). Used
// to decide whether re-pointing local-lvm would strand live guest disks.
const LocalLvmRidersScript = "pool=$(grep -A2 '^lvmthin: local-lvm$' /etc/pve/storage.cfg | awk '/^[[:space:]]*thinpool[[:space:]]/{print $2; exit}'); if [ -n \"$pool\" ]; then lvs --noheadings -o pool_lv,lv_name 2>/dev/null | awk -v p=\"$pool\" '$1==p && $2 !~ /_(t|c)data$/ && $2 !~ /_(t|c)meta$/ && $2 !~ /_pmspare$/ {print $2}'; fi"

// RepointLocalLvm points PVE's stock local-lvm storage at `pool`: probe the
// current pointer, skip when already correct, scoped awk edit, then READ BACK.
// Idempotent; a missing local-lvm block is an error (callers run only where
// storage.cfg is known to carry one). `run` executes one script on the target
// and returns its stdout.
func RepointLocalLvm(run func(script string, timeoutS uint64) (string, error), pool string) error {
	if !planebase.ValidStorageName(pool) {
		return fmt.Errorf("refusing to re-point local-lvm at an unsafe pool name %q (allowed: [A-Za-z0-9._-])", pool)
	}
	current, err := run(LocalLvmProbeScript, 30)
	if err != nil {
		return fmt.Errorf("local-lvm probe failed: %w", err)
	}
	if current = strings.TrimSpace(current); current == "" {
		return fmt.Errorf("no `lvmthin: local-lvm` storage block in /etc/pve/storage.cfg — cannot re-point it at %s", pool)
	}
	if current == pool {
		return nil // already pointing there
	}
	if _, err := run(LocalLvmRepointScript(pool), 60); err != nil {
		return fmt.Errorf("re-pointing PVE local-lvm to %s: %w", pool, err)
	}
	rb, err := run(LocalLvmProbeScript, 30)
	if err != nil {
		return fmt.Errorf("local-lvm readback failed: %w", err)
	}
	if got := strings.TrimSpace(rb); got != pool {
		return fmt.Errorf("local-lvm still points at %q after the edit (want %q)", got, pool)
	}
	return nil
}
