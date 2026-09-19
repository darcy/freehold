// Package provisioning hosts the substrate seam: the low-level Provider
// interface platform engines (box, service deployers) call so they stay free
// of provider imports and provider-specific command strings. Concrete
// providers live in the top-level providers/ module and are injected by the
// composition roots (install/, control-plane/).
package provisioning

import "freehold/contract/client"

// Guest is one substrate guest as the provider sees it.
type Guest struct {
	ID   string
	Name string
}

// ExecFunc is a transport-free host-command runner: the caller owns the
// transport (the runner's MCP client today, direct SSH later), the provider
// owns the guest command wrapping.
type ExecFunc func(cmd string, timeoutS uint64) (*client.ExecOutcome, error)

// GuestExecFunc runs a command inside a guest ("" = the substrate host). The
// provider wraps the command in a single-quoted `sh -c` shell, so cmd must be
// single-quote-free.
type GuestExecFunc func(guest string, cmd string, timeoutS uint64) (*client.ExecOutcome, error)

// Provider is the substrate ops seam. It is deliberately low-level: there is
// no lifecycle method — install/build/teardown/uninstall own the sequence.
type Provider interface {
	// GuestExec runs cmd inside guest ("" = host) through the provider's
	// transport; the provider owns the wrapping.
	GuestExec(guest string, cmd string, timeoutS uint64) (*client.ExecOutcome, error)
	// ListGuests returns every guest as (id, name).
	ListGuests() ([]Guest, error)
	// GuestIPv4 returns the guest's first non-loopback IPv4 (CIDR).
	GuestIPv4(guest string) (string, error)
	// GuestMounts returns the guest's mount guest-paths, in order.
	GuestMounts(guest string) ([]string, error)
	// DestroyGuest destroys a guest (and its rootfs), best-effort.
	DestroyGuest(guest string) error
	// LocalLvmStatus returns the pool PVE's local-lvm currently points at and
	// how many LVs ride it (the stranding guard's input).
	LocalLvmStatus() (pool string, riders int, err error)
	// RepointLocalLvm keeps PVE's stock local-lvm storage pointed at thinPool.
	// Idempotent.
	RepointLocalLvm(thinPool string) error
}
