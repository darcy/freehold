package main

import "testing"

// TestAuthzRefused pins the classification the channel migration relies on: a
// non-owner refusal is a skip (so it can't wedge the queue), while a transient
// error is not.
func TestAuthzRefused(t *testing.T) {
	refused := []string{
		`event publish returned HTTP 403: actor not authorized for name/about/archived/visibility/ttl changes`,
		"relay: not the channel owner",
		"event publish returned HTTP 403: forbidden",
	}
	for _, m := range refused {
		if !authzRefused(m) {
			t.Errorf("authzRefused(%q) = false, want true", m)
		}
	}
	transient := []string{
		"event publish request failed: dial tcp: connection refused",
		"event publish returned HTTP 500: events blocked",
	}
	for _, m := range transient {
		if authzRefused(m) {
			t.Errorf("authzRefused(%q) = true, want false", m)
		}
	}
}
