package main

import (
	"strings"
	"testing"

	"freehold/contract/config"
)

// TestTeardownFailsClosedNoProfiles guards the destructive-path guardrail: with
// zero registered profiles, `freehold teardown` must fail closed.
func TestTeardownFailsClosedNoProfiles(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })
	if code := run([]string{"teardown"}); code == 0 {
		t.Fatal("teardown with zero profiles must fail closed (return non-zero)")
	}
}

// TestCommandsRegisteredOnce is the guardrail that each command name is
// registered exactly once — the duplicate-exec failure mode.
func TestCommandsRegisteredOnce(t *testing.T) {
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	seen := map[string]int{}
	for _, c := range rootCmd.Commands() {
		name := strings.Fields(c.Use)[0]
		seen[name]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("command %q registered %d times, want exactly once", name, n)
		}
	}
}

// TestVerbSurface pins the post-refactor command surface: the renames exist,
// the cut commands and the bootstrap alias are gone.
func TestVerbSurface(t *testing.T) {
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	have := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		have[strings.Fields(c.Use)[0]] = true
	}
	for _, want := range []string{
		"install", "build", "teardown", "uninstall", "exec", "profiles",
		"door", "dns-cred", "status", "update", "add-relay-member",
	} {
		if !have[want] {
			t.Errorf("command %q must be registered", want)
		}
	}
	for _, gone := range []string{
		"bootstrap", "world", "relay-member", "console-login", "relay-profile",
		"relay-join", "relay-setup", "delegate", "delegate-peer", "memory",
		"demo", "readiness", "channel",
	} {
		if have[gone] {
			t.Errorf("command %q must be removed", gone)
		}
	}
}
