// Command freehold-console is the Go port of the Rust control-plane serve:
// the CP's loopback admin/ops web console (NIP-98 operator auth when an admin
// whitelist is seeded). It replaces the Rust control-plane console crate at
// parity — same routes, same security guards (loopback-only until authn, the
// DNS-rebinding Origin guard, HttpOnly+SameSite=Strict session cookies,
// single-use portal tokens).
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"freehold/contract/state"
	"freehold/control-plane/api/console"
)

func main() {
	log.SetFlags(0)
	fs := flag.NewFlagSet("freehold-console", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "CP state dir (state.json + console identity)")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP bind address")
	adminPubkeys := fs.String("admin-pubkeys", "", "comma-separated operator pubkeys (64-hex) — seeds NIP-98 console auth; empty = loopback-only posture")
	relayURL := fs.String("relay-url", "", "relay scope (recorded in state.json)")
	relayPubkey := fs.String("relay-pubkey", "", "relay signing pubkey (roster trust anchor)")
	relayHost := fs.String("relay-host", "", "relay community host")
	publicOrigin := fs.String("public-origin", "", "the console's fronted public origin (DNS-rebinding guard C3.5)")
	agentToolsURL := fs.String("agent-tools-url", "", "agent-tools MCP server URL (served on /api/world)")
	agentToolsPubkey := fs.String("agent-tools-pubkey", "", "agent-tools server pubkey (served on /api/world)")
	fs.Parse(os.Args[1:])

	if *stateDir == "" {
		log.Fatal("serve needs --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		log.Fatalf("open state: %v", err)
	}

	// Admin whitelist: seeded on first serve, persisted in state.json.
	var admins []string
	if *adminPubkeys != "" {
		for _, pk := range strings.Split(*adminPubkeys, ",") {
			pk = strings.TrimSpace(pk)
			if pk == "" {
				continue
			}
			if !isHex64(pk) {
				log.Fatalf("--admin-pubkeys entries must be 64-hex Nostr pubkeys (got %q)", pk)
			}
			admins = append(admins, pk)
		}
		if len(admins) > 0 {
			store.SetAdmins(admins)
			if err := store.Save(); err != nil {
				log.Fatalf("persist admins: %v", err)
			}
		}
	} else {
		admins = store.Admins()
	}

	// Relay / agent-tools scope (recorded, served on /api/world).
	if *relayURL != "" {
		if err := store.SetRelayURL(strPtr(*relayURL)); err != nil {
			log.Fatalf("set relay url: %v", err)
		}
	}
	if *relayHost != "" {
		if err := store.SetRelayHost(strPtr(*relayHost)); err != nil {
			log.Fatalf("set relay host: %v", err)
		}
	}
	if *relayPubkey != "" {
		if !isHex64(*relayPubkey) {
			log.Fatalf("--relay-pubkey must be a 64-hex Nostr pubkey (got %q)", *relayPubkey)
		}
		if err := store.SetRelayPubkey(strPtr(*relayPubkey)); err != nil {
			log.Fatalf("set relay pubkey: %v", err)
		}
	}
	if *agentToolsURL != "" {
		if err := store.SetAgentToolsURL(strPtr(*agentToolsURL)); err != nil {
			log.Fatalf("set agent-tools url: %v", err)
		}
	}
	if *agentToolsPubkey != "" {
		if !isHex64(*agentToolsPubkey) {
			log.Fatalf("--agent-tools-pubkey must be a 64-hex Nostr pubkey (got %q)", *agentToolsPubkey)
		}
		if err := store.SetAgentToolsPubkey(strPtr(*agentToolsPubkey)); err != nil {
			log.Fatalf("set agent-tools pubkey: %v", err)
		}
	}

	// The console identity (minted on the box at first serve, 0600 — a keypair
	// is NEVER shipped; deploy-cp reads the pubkey back to relay-member it).
	secret, consolePK, err := console.EnsureConsoleIdentity(*stateDir)
	if err != nil {
		log.Fatal(err)
	}

	// Bind guard: unauthenticated console stays loopback-only (C3).
	var auth *console.Auth
	if len(admins) > 0 {
		auth = console.NewAuth(admins)
		log.Printf("console auth enabled (NIP-98, %d operators) — non-loopback bind allowed", len(admins))
	} else {
		if err := console.ValidateLoopbackBind(*addr); err != nil {
			log.Fatal(err)
		}
	}

	var pubOrigin *string
	if *publicOrigin != "" {
		pubOrigin = strPtr(*publicOrigin)
	}
	srv := &console.Server{
		Store: store, ConsoleSecret: secret, ConsolePubkey: consolePK,
		Auth: auth, PublicOrigin: pubOrigin, RelayHost: *relayHost,
	}
	log.Printf("freehold-console (Go) serving on %s (console agent %s)", *addr, consolePK)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func strPtr(s string) *string { return &s }