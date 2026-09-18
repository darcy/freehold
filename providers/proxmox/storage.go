package proxmox

import (
	"fmt"

	"freehold/contract/client"
	"freehold/platform/provisioning/bootstrap"
	"freehold/providers/proxmox/drive"
)

// EnsureZpool creates a zpool if absent (idempotent) — the device-clean gate
// (ProbeDevice) is provider-side.
func EnsureZpool(clientConn *client.McpClient, target, pool string, device *string) error {
	exists, err := drive.ZpoolExists(clientConn, target, pool)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if device == nil || *device == "" {
		return fmt.Errorf("creating a zpool needs a physical device — pass --device (e.g. /dev/sdb); or use an existing zpool/LVM backend")
	}
	probe, err := ProbeDevice(clientConn, target, *device)
	if err != nil {
		return err
	}
	if !probe.Clean {
		return fmt.Errorf("refusing to create a zpool on %s — it is not empty: %s. Freehold never erases a device that carries data; clean it yourself (after confirming it holds nothing you need) and re-run", *device, probe.Content)
	}
	_, err = bootstrap.ExecToOK(clientConn, target, "zpool create "+pool+" "+*device, "zpool create", 300)
	return err
}
