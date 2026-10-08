package proxmox

import (
	"errors"
	"strings"
	"testing"

	"freehold/platform/provisioning"
)

func TestProxmoxNeedsAreReachedHostShape(t *testing.T) {
	p := HostProvider{}
	if p.HostsGateway() {
		t.Error("a reached LAN host runs the gateway as a GUEST")
	}
	if p.AccessMode() != "ssh-root-proxmox" {
		t.Errorf("access mode: %q", p.AccessMode())
	}
	var host, proxy, consent bool
	for _, n := range p.Needs() {
		switch n.Name {
		case provisioning.NeedHost:
			host = true
			if n.Secret || n.Bool || n.Default == "" {
				t.Errorf("the host need is a plain answered ask: %+v", n)
			}
		case provisioning.NeedProxyIP:
			proxy = true
		case provisioning.NeedConfirmStorage:
			consent = n.Bool
		}
	}
	if !host || !proxy || !consent {
		t.Fatalf("proxmox needs incomplete (host/proxy_ip/consent): %+v", p.Needs())
	}
}

func TestProxmoxInstallDoorKeyPasteGate(t *testing.T) {
	p := HostProvider{}
	s := &provisioning.HostSession{
		Host:        "root@192.0.2.10",
		Interactive: true,
	}
	printed := 0
	s.Print = func(string, ...any) { printed++ }
	answers := []string{"", "q"} // ENTER (done), then q (never reached)
	s.Prompt = func(string) (string, error) {
		if len(answers) == 0 {
			return "", errors.New("EOF")
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	if err := p.InstallDoorKey(s, "ssh-ed25519 AAA door"); err != nil {
		t.Fatalf("paste gate: %v", err)
	}
	if printed == 0 {
		t.Fatal("the door line must be printed for the operator to paste")
	}
	// 'q' aborts — the door is NOT installed.
	s2 := &provisioning.HostSession{Host: "root@h", Interactive: true}
	s2.Print = func(string, ...any) {}
	s2.Prompt = func(string) (string, error) { return "", errors.New("aborted") }
	if err := p.InstallDoorKey(s2, "ssh-ed25519 AAA door"); err == nil {
		t.Fatal("a quit prompt must abort the install (the door is not in place)")
	}
	// Headless cannot prompt — the actionable bail, never a silent pass.
	s3 := &provisioning.HostSession{Host: "root@h", Interactive: false}
	if err := p.InstallDoorKey(s3, "ssh-ed25519 AAA door"); err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("headless must bail with the key in the message: %v", err)
	}
	// An empty key (a reused package the caller could not recover) is a
	// no-op — the verify surfaces access problems.
	s4 := &provisioning.HostSession{Host: "root@h", Interactive: false}
	if err := p.InstallDoorKey(s4, ""); err != nil {
		t.Fatalf("empty key no-op: %v", err)
	}
}
