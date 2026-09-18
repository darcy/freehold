package bootstrap

import (
	"fmt"
	"strings"

	"freehold/platform/provisioning/planebase"
)

// ParseMount parses a `<dataset>:<guest-path>` mount reference.
func ParseMount(s string) (planebase.MountSpec, error) {
	src, guest, ok := strings.Cut(s, ":")
	if !ok || src == "" || guest == "" {
		return planebase.MountSpec{}, fmt.Errorf("mount %q must be <dataset>:<guest-path>", s)
	}
	if err := PlainPath(src); err != nil {
		return planebase.MountSpec{}, err
	}
	if err := PlainPath(guest); err != nil {
		return planebase.MountSpec{}, err
	}
	return planebase.MountSpec{Source: src, GuestPath: guest}, nil
}
