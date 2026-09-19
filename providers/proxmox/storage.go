package proxmox

import (
	"fmt"

	"freehold/providers/proxmox/drive"
)

// EnsureZpool creates a zpool if absent (idempotent) — the device-clean gate
// (ProbeDevice) is provider-side.
func EnsureZpool(exec ExecFunc, pool string, device *string) error {
	exists, err := drive.ZpoolExists(exec, pool)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if device == nil || *device == "" {
		return fmt.Errorf("creating a zpool needs a physical device — pass --device (e.g. /dev/sdb); or use an existing zpool/LVM backend")
	}
	probe, err := ProbeDevice(exec, *device)
	if err != nil {
		return err
	}
	if !probe.Clean {
		return fmt.Errorf("refusing to create a zpool on %s — it is not empty: %s. Freehold never erases a device that carries data; clean it yourself (after confirming it holds nothing you need) and re-run", *device, probe.Content)
	}
	_, err = ExecToOK(exec, "zpool create "+pool+" "+*device, "zpool create", 300)
	return err
}
