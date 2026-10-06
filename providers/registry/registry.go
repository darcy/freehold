// Package registry is the host-provisioner seam's composition table: the
// name an operator types at --provider (or picks in the guided flow) to the
// HostProvider that owns the world's host. Adding a substrate = an entry
// here; the installer never learns a provider's name.
package registry

import (
	"fmt"
	"sort"

	"freehold/platform/provisioning"
	"freehold/providers/proxmox"
	"freehold/providers/vultr"
)

var providers = map[string]provisioning.HostProvider{
	"proxmox": proxmox.HostProvider{},
	"vultr":   vultr.HostProvider{},
}

// ByName resolves a provider; an empty name resolves the default (proxmox —
// the lead substrate). Unknown names fail with the known set.
func ByName(name string) (provisioning.HostProvider, error) {
	if name == "" {
		name = "proxmox"
	}
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %v)", name, Names())
	}
	return p, nil
}

// Names lists the registry keys, default first.
func Names() []string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "proxmox") != (names[j] == "proxmox") {
			return names[i] == "proxmox"
		}
		return names[i] < names[j]
	})
	return names
}
