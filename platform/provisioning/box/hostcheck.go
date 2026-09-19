package box

import (
	"fmt"

	"freehold/platform/provisioning"
)

// CPGuestLive reports whether the world's `<name>-cp` guest exists on the
// substrate. This is the host-side, profile-independent fail-if-live probe:
// a profile-less box pointed at a host that already runs a live CP would
// otherwise fall to mint and re-deploy over it. The provider's guest list is
// the authoritative source (a console probe only covers a known CP URL).
func CPGuestLive(p provisioning.Provider, name, domain string) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("no provisioning provider wired — cannot list guests")
	}
	exact, err := lxcName(name, domain, "cp")
	if err != nil {
		return false, err
	}
	guests, err := p.ListGuests()
	if err != nil {
		return false, err
	}
	for _, g := range guests {
		if g.Name == exact {
			return true, nil
		}
	}
	return false, nil
}
