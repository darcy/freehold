// The exposure capability: Network's hands for making an agent-built service
// reachable. `expose_app` turns {name, target, group} into a public TLS
// vhost on the edge — DNS pointed, cert issued (the SAME in-process lego
// chain the build uses), the Caddy config rendered + applied — gated by the
// console's member login unless the operator said otherwise. The apps
// registry (apps.json, the toolset's durable dir) is the source of truth:
// the build tail re-ensures every record, so teardown/rebuild restores the
// apps the same way capability doors re-stage.
//
// Two invariants this file exists to keep:
//
//   - CERT BEFORE CONFIG. A vhost whose tls files are missing crash-loops
//     the whole edge (the relay rides it) — an app's vhost enters the
//     rendered Caddyfile only after its cert is installed, checked against
//     the durable mirror at render time. Both appliers (the build's
//     terraform var and the verb's kubectl apply) render identically, so
//     they converge, never fight.
//   - ACCESS NARROWS TO GROUPS THE REQUESTER BELONGS TO. Every app rides a
//     relay channel (its ID — names change, ids don't); Network validates
//     the requester is IN the named channel before recording it. Widening
//     (visibility public / auth none) is refused at the toolset's dispatch —
//     an operator-level act, never an agent's.
package cpbuild

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/relay"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	cert "freehold/platform/services/certificates/letsencrypt"
	dnsman "freehold/platform/services/externaldns/cloudflare"
	"freehold/platform/services/pangolin"
	caddydeploy "freehold/platform/services/webproxy/caddy"
)

// WorldDomain is the world's own domain (config.WorldDomain) — the per-world
// subtree every world name hangs off; kept as a local alias so the exposure
// machinery reads its own vocabulary.
func WorldDomain(cpHost string) string { return config.WorldDomain(cpHost) }

// appDomainIsZone reports whether the cp host IS a zone apex (two labels) —
// the one shape where the apps' shared wildcard (*.host) would cover the
// whole zone rather than one world's subtree.
func appDomainIsZone(cpHost string) bool {
	return len(strings.Split(cpHost, ".")) < 3
}

// portalHost is the PORTAL's public host — the world root itself, the
// member face the edge points at the same backend as the operator console.
// Empty while the cp host IS the world root (the console hasn't moved off it:
// its own vhost serves that name, no portal vhost exists).
func (s *Spec) portalHost() string { return appsPortalHost(s.CpHost, s.WorldDomainOverride) }

// appsPortalHost resolves the portal host from the CP host + the operator's
// explicit world-domain override (EffectiveWorldDomain). Empty while the CP
// host IS the world root.
func appsPortalHost(cpHost, override string) string {
	if root := config.EffectiveWorldDomain(cpHost, override); root != cpHost {
		return root
	}
	return ""
}

// appWildcardHost resolves the apps' shared wildcard — the world domain's
// wildcard under the same override.
func appWildcardHost(cpHost, override string) string {
	return "*." + config.EffectiveWorldDomain(cpHost, override)
}

// appWildcard is the wildcard the apps' shared cert covers — the world
// domain's wildcard (one cert covers every app the world will ever expose;
// its challenge name is unique inside the world).
func (s *Spec) appWildcard() string { return appWildcardHost(s.CpHost, s.WorldDomainOverride) }

// appFQDN is an app's public hostname: a CHILD of the world's own domain
// (litellm.sandbox.freehold.technology, litellm.librem.freehold.technology) —
// never a shared-zone-level name, which is where cross-world record
// confusion comes from. The member cookie scopes to the same world domain,
// so the cookie reaches every child app AND the shared wildcard's challenge
// name (_acme-challenge.<world-domain>) stays inside the world. Both ends of
// the gate agree on this shape.
func appFQDN(cpHost, override, name string) string {
	return name + "." + config.EffectiveWorldDomain(cpHost, override)
}

// looksLikeChannelID reports whether g is already a dashed-uuid channel id
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx) rather than a display name.
func looksLikeChannelID(g string) bool {
	if len(g) != 36 {
		return false
	}
	for i, c := range g {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

// groupID resolves the expose group: a channel NAME resolves through the
// relay (FindChannel); an already-dashed id passes through. EMPTY defaults
// to `general` — the everyone-channel the first-run surface stands up. The
// record stores the ID, never the name — groups get renamed, the ACL must
// not break.
func groupID(dialURL, authURL string, secret []byte, group string) (string, error) {
	g := strings.TrimSpace(group)
	if g == "" {
		g = "general"
	}
	if looksLikeChannelID(g) {
		return g, nil
	}
	id, _, ok, err := relay.FindChannelAuth(dialURL, authURL, secret, g)
	if err != nil {
		return "", fmt.Errorf("group %q: %w", g, err)
	}
	if !ok {
		return "", fmt.Errorf("group %q: no such channel on the relay — create it first (agents do that in conversation)", g)
	}
	return id, nil
}

// requesterInGroup is the expose invariant: the requester is IN the channel
// they are exposing into (IsMemberAuth — the same channel-scoped read the
// runner grants use), and the requester is a KNOWN identity (a registry
// agent or the operator), never an arbitrary pubkey.
func requesterInGroup(reg *agenttools.Registry, operatorPub, requester, dialURL, authURL string, secret []byte, groupID string) error {
	known := operatorPub != "" && requester == operatorPub
	if !known {
		agents, err := reg.Agents()
		if err != nil {
			return fmt.Errorf("requester check: %w", err)
		}
		for _, a := range agents {
			if a.Pubkey == requester {
				known = true
				break
			}
		}
	}
	if !known {
		return fmt.Errorf("requester %s is not a known identity (a registry agent or the operator)", shortHex(requester))
	}
	in, err := relay.IsMemberAuth(dialURL, authURL, secret, groupID, requester)
	if err != nil {
		return fmt.Errorf("requester membership check: %w", err)
	}
	if !in {
		return fmt.Errorf("requester is not in the %q channel — have them added first (access narrows to groups the requester belongs to; it never widens)", groupID)
	}
	return nil
}

func shortHex(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// appsCertSlot is the SHARED cert slot for every app vhost: ONE wildcard
// cert (*.cp-host) issued once — an expose is DNS + config only after that,
// never a per-app LE order (no per-app rate-limit accumulation, no
// propagation wait per app).
const appsCertSlot = "apps"

// portalCertSlot is the PORTAL's cert slot — the world root's own (non-
// wildcard) name, the member face. Its challenge name is the world root's
// (_acme-challenge.<root>), shared with the apps wildcard's — the certPending
// guard covers the overlap.
const portalCertSlot = "portal"

// appRecordFQDN derives the row's hostname FROM ITS NAME at consume time —
// rows written by an older shape re-derive to the current world-domain shape
// the shared wildcard covers, instead of re-serving a name the wildcard's
// one-label coverage misses (a silent TLS mismatch). The stored FQDN is
// informational; appDNSEnsure migrates the public record when the two
// differ.
func (s *Spec) appRecordFQDN(rec agenttools.AppRecord) string {
	return appFQDN(s.CpHost, s.WorldDomainOverride, rec.Name)
}

// appVhost builds one registry row's site block. Gated apps forward through
// the console's member gate on the CP guest (the CP's LAN IP, port 8080);
// every app presents the SHARED wildcard cert.
func (s *Spec) appVhost(rec agenttools.AppRecord) caddydeploy.AppVhost {
	gate := ""
	if rec.Auth == agenttools.AuthGate {
		gate = s.CpIP + ":8080"
	}
	v := caddydeploy.AppVhost{
		FQDN:     s.appRecordFQDN(rec),
		Upstream: rec.Target,
		Slot:     appsCertSlot,
		Gate:     gate,
	}
	// The operator's at-source exemptions ride by NAME (the profile's
	// [edge.apps]) — the registry row still owns fqdn/upstream.
	if ea, ok := s.EdgeApps[rec.Name]; ok {
		v.ExemptPaths = ea.ExemptPaths
		v.ExemptBearer = ea.ExemptBearer
	}
	return v
}

// edgeRiders renders the operator's at-source rider vhosts — [edge.apps]
// rows with NO registry record (the gate-less apps an operator pins at
// source: nextcloud/paperless-class services). The registry owns the gated
// surface; these are the operator's explicit exceptions, and they ride the
// SAME shared wildcard slot (cert-before-config applies — the caller renders
// them only behind a valid wildcard). A row whose name matches a registry
// record is NOT a rider (its exemptions merged in appVhost instead).
func (s *Spec) edgeRiders(registry map[string]bool) []caddydeploy.AppVhost {
	if len(s.EdgeApps) == 0 {
		return nil
	}
	portal := s.portalHost()
	var riders []caddydeploy.AppVhost
	for name, ea := range s.EdgeApps {
		if registry[name] || ea.Upstream == "" {
			continue
		}
		fqdn := appFQDN(s.CpHost, s.WorldDomainOverride, name)
		// Rider keys are operator free-hand; registry names are enforced
		// lowercase DNS labels. A case-variant ("Nextcloud" vs the registry
		// row "nextcloud") would dodge the merge above AND — hosts match
		// case-insensitively — render a second site block for a host that
		// already has one: the unloadable-config crash-loop, reported by
		// nothing. Same refusal the registry path applies, loud skip here.
		if !agenttools.ValidAppName(name) {
			log.Printf("edge rider %q: not a lowercase DNS label — skipped", name)
			continue
		}
		// Two site blocks for one host is an unloadable Caddyfile — the
		// edge crash-loops and kubectl apply still exits 0. The registry
		// path refuses the edge's own hosts (BuildExposeAppFn); the rider
		// path can't fail a render, so it skips loud instead.
		if fqdn == s.RelayHost || fqdn == s.CpHost || fqdn == portal {
			log.Printf("edge rider %q: %s is the edge's own host — skipped (rename the rider)", name, fqdn)
			continue
		}
		gate := ""
		if ea.Gate != nil && *ea.Gate {
			gate = s.CpIP + ":8080"
		}
		riders = append(riders, caddydeploy.AppVhost{
			FQDN:         fqdn,
			Upstream:     ea.Upstream,
			Slot:         appsCertSlot,
			Gate:         gate,
			ExemptPaths:  ea.ExemptPaths,
			ExemptBearer: ea.ExemptBearer,
		})
	}
	return riders
}

// edgeRiderDNSEnsure points a rider vhost's public A record — the same
// surface appDNSEnsure gives registry apps (the proxy's edge IP, or the
// Pangolin VPS when the public path rides the tunnel). Without it the rider
// renders for an NXDOMAIN name: the operator pins the app, the build reports
// success, nothing is reachable.
func (s *Spec) edgeRiderDNSEnsure(name string) (bool, error) {
	fqdn := appFQDN(s.CpHost, s.WorldDomainOverride, name)
	// Riders are operator-pinned vhosts with no registry row, so no Pangolin
	// resource exists for them: their A record stays on the proxy (the
	// allowlist gates REGISTRY apps only).
	ip := s.ProxyIP
	if ip == "" {
		return false, fmt.Errorf("rider dns %s: the proxy IP is not in the world coords", fqdn)
	}
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		return false, fmt.Errorf("rider dns %s: %w", fqdn, err)
	}
	mgr, err := dnsman.For(provider, env)
	if err != nil {
		return false, fmt.Errorf("rider dns %s: %w", fqdn, err)
	}
	if err := mgr.UpsertA(fqdn, ip); err != nil {
		return false, fmt.Errorf("rider dns %s: %w", fqdn, err)
	}
	return true, nil
}

// appsCertEnsure runs the SHARED wildcard cert's chain (*.world-domain): the
// durable-reuse gate (a valid mirror cert seeds the PVC — no LE order), the
// box seed cache, then the in-process resumable DNS-01 issue. Returns
// whether a cert was installed NOW (vs already present). ONE order covers
// every app under the world domain; the zone credential comes from the relay
// slot's sealed store — one token, one zone, every slot.
// certCovers reports whether the fullchain's leaf cert's SANs carry the
// wanted host EXACTLY (the wildcard form included) — the durable-reuse gate
// must not serve a stale shape's cert after the covered domain moves (an
// old-shape wildcard in the mirror would otherwise TLS-mismatch every app).
func certCovers(fullchain []byte, host string) bool {
	block, _ := pem.Decode(fullchain)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	for _, name := range leaf.DNSNames {
		if name == host {
			return true
		}
	}
	return false
}

// certPending reports whether a relay/cp/portal order is still un-resolved:
// the state file exists until its Resolve succeeds (DiscardState). Those
// slots' challenge names include the world root's own (_acme-challenge.<world
// -domain> — the portal cert's, and the cp cert's whenever the cp host IS the
// world root) — the SAME name the apps wildcard challenges at. A fresh apps
// order would purge the pending order's challenge TXT out from under it (the
// blast radius is one failed build that self-heals, but the check turns that
// race into a clean refusal).
func (s *Spec) certPending() bool {
	for _, sl := range []string{"relay", "cp", portalCertSlot} {
		if sl == portalCertSlot && s.portalHost() == "" {
			continue // no portal slot on this world
		}
		if _, err := os.Stat(filepath.Join(s.consoleStateRoot(), "world-secrets", "cert-pending-"+sl+".json")); err == nil {
			return true
		}
	}
	return false
}

func (s *Spec) appsCertEnsure() (bool, error) {
	slot := appsCertSlot
	host := s.appWildcard()
	if s.certPending() {
		return false, fmt.Errorf("cert %s: a relay/cp/portal cert order is still pending (shared challenge name) — the next build re-ensures", slot)
	}
	if fc, err := s.durableFullchain(s.K3sVmid, slot); err == nil && s.durableKeyPresent(s.K3sVmid, slot) {
		if _, ok := cert.ReuseIfValidBytes(fc, time.Now(), 30*24*time.Hour); ok && certCovers(fc, host) {
			if err := s.seedCaddyCertFromDurable(s.K3sVmid, slot); err != nil {
				return false, fmt.Errorf("cert %s durable seed: %w", slot, err)
			}
			return false, nil
		}
	}
	seeded, err := s.certSeedFromCache(s.K3sVmid, slot, host)
	if err != nil {
		return false, fmt.Errorf("cert %s seed: %w", slot, err)
	}
	if seeded {
		return true, nil
	}
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		return false, fmt.Errorf("cert %s: %w", slot, err)
	}
	resume, po, statePath, err := s.openCertOrder(slot, host, provider, env)
	if err != nil {
		return false, fmt.Errorf("cert %s issue: %w", slot, err)
	}
	if err := resume.PropagationWait(po); err != nil {
		// Transient: KEEP the resumable order — the next run resumes the same
		// order + challenge instead of minting a new one.
		return false, fmt.Errorf("cert %s issue: %w", slot, err)
	}
	issued, err := resume.Resolve(po)
	if err != nil {
		if errors.Is(err, cert.ErrAuthInvalid) {
			_ = os.Remove(statePath)
		}
		return false, fmt.Errorf("cert %s issue: %w", slot, err)
	}
	if err := s.installCaddyCertFile(s.K3sVmid, slot, issued.Fullchain, issued.Key); err != nil {
		return false, fmt.Errorf("cert %s install: %w", slot, err)
	}
	// The wildcard's challenge lives at the BASE's name (lego strips the "*").
	if n, perr := cert.PurgeChallengeRecords(host, provider, env); perr == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "cert %s: cleaned %d challenge record(s) after issue\n", slot, n)
	}
	return true, nil
}

// appDNSEnsure points the app's public A record at the proxy. LAN visibility
// skips it: no public record — the name resolves only inside the world.
func (s *Spec) appDNSEnsure(rec agenttools.AppRecord) (bool, error) {
	// LAN visibility: NO public record — the whole point of the mode.
	if rec.Visibility == agenttools.VisibilityLAN {
		return false, nil
	}
	fqdn := s.appRecordFQDN(rec)
	// The pangolin public path: the app's A record points at the VPS (the
	// home IP appears in no public DNS). Empty DNSIP keeps the proxy IP — a
	// dev/test world exercises the tunnel without moving the public DNS.
	ip := s.ProxyIP
	if s.Pangolin != nil && s.Pangolin.DNSIP != "" && s.pangolinRides(rec.Name) {
		ip = s.Pangolin.DNSIP
	}
	if ip == "" {
		return false, fmt.Errorf("app dns %s: the proxy IP is not in the world coords", fqdn)
	}
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		return false, fmt.Errorf("app dns %s: %w", fqdn, err)
	}
	mgr, err := dnsman.For(provider, env)
	if err != nil {
		return false, fmt.Errorf("app dns %s: %w", fqdn, err)
	}
	// Shape migration: a row written by an older naming shape (its stored
	// FQDN differs from the derived one — e.g. a shared-zone-level name from
	// before apps hung off the world domain) repoints to the derived name and
	// best-effort removes the STALE public record, so the old name never
	// lingers pointing at the edge. DeleteA of a missing record is a no-op,
	// so this stays idempotent across builds.
	if rec.FQDN != "" && rec.FQDN != fqdn {
		_ = mgr.DeleteA(rec.FQDN)
	}
	if err := mgr.UpsertA(fqdn, ip); err != nil {
		return false, fmt.Errorf("app dns %s: %w", fqdn, err)
	}
	return true, nil
}

// pangolinCreds opens the world's Pangolin credential — the instance's
// integration address + the root key, sealed to the console identity at
// world-secrets/pangolin.json (the DNS-cred record shape; the coords carry
// no credential). Written by the box hand-off (or the operator, via the
// console's enc pubkey).
func (s *Spec) pangolinCreds() (base, key, endpoint string, err error) {
	root := s.consoleStateRoot()
	path := filepath.Join(root, "world-secrets", "pangolin.json")
	if !cert.CredExists(path) {
		return "", "", "", fmt.Errorf("no pangolin credential on the CP at %s — seal PANGOLIN_BASE + PANGOLIN_KEY + PANGOLIN_ENDPOINT to the console identity (the DNS-cred record shape)", path)
	}
	secret, err := s.consoleEncSecretAt(root)
	if err != nil {
		return "", "", "", err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(path, open, secret)
	if err != nil {
		return "", "", "", fmt.Errorf("pangolin credential: %w", err)
	}
	return env["PANGOLIN_BASE"], env["PANGOLIN_KEY"], env["PANGOLIN_ENDPOINT"], nil
}

// pangolinEnsure puts one app behind the world's Pangolin public path
// (docs/NETWORK.md): the gateway's Newt site ensured, the resource found or
// created (subdomain on the base domain, mode http — the per-host cert is
// Pangolin's Traefik's business, HTTP-01), and the target pointed at the
// app's freehold-subnet address, reachable from the gateway where Newt
// dials out. LAN-visibility records are skipped — their point is that the
// name resolves only inside the world; a public Pangolin resource would
// contradict it. A freshly created site's one-time Newt secret is sealed to
// the console identity immediately (world-secrets/pangolin-newt.json — the
// gateway stage opens it; losing it burns the identity). Idempotent — the
// expose verb and the build tail both call it.
func (s *Spec) pangolinEnsure(rec agenttools.AppRecord) error {
	p := s.Pangolin
	if p == nil || p.Org == "" || p.Site == "" || p.Domain == "" {
		return fmt.Errorf("pangolin: the world coords lack the pangolin public path (org/site/domain)")
	}
	// The Pangolin base domain and the world's own domain MUST agree: the app
	// A record (appDNSEnsure) points the effective-derived name at the VPS,
	// and the resource must route that same name — a divergence is silent
	// breakage (the visited name has no route on Pangolin's edge).
	if base := config.EffectiveWorldDomain(s.CpHost, s.WorldDomainOverride); p.Domain != base {
		return fmt.Errorf("pangolin: the recorded base domain %q does not match the world's own domain %q — update the pangolin coords to the override", p.Domain, base)
	}
	if rec.Visibility == agenttools.VisibilityLAN {
		return nil
	}
	if !s.pangolinRides(rec.Name) {
		return nil
	}
	if err := s.pangolinHostDNSEnsure(); err != nil {
		return err
	}
	base, key, endpoint, err := s.pangolinCreds()
	if err != nil {
		return err
	}
	if s.GatewayLxc == 0 && !s.HostedGateway {
		return fmt.Errorf("pangolin: the world has no gateway — the Newt tunnel client has nowhere to run (the pangolin public path needs one)")
	}
	// The tunnel FIRST: the client's base is the CP's loopback — the
	// forward unit is what makes that address mean the VPS. Dailing
	// before the unit exists deadlocks every retry.
	if err := s.ensurePangolinTunnel(); err != nil {
		return fmt.Errorf("pangolin: the CP tunnel stage: %w", err)
	}
	cl := &pangolin.Client{Base: base, Key: key}
	site, created, err := cl.EnsureSite(context.Background(), p.Org, p.Site)
	if err != nil {
		return fmt.Errorf("pangolin: site %q: %w", p.Site, err)
	}
	if created {
		if err := s.sealNewtIdentity(site); err != nil {
			return fmt.Errorf("pangolin: site %q was CREATED and its one-time Newt secret could not be sealed — the identity is unusable until it is (remove the site on the instance and re-run): %w", p.Site, err)
		}
	}
	if err := s.ensureNewtGateway(endpoint); err != nil {
		return fmt.Errorf("pangolin: the gateway's newt stage: %w", err)
	}
	if err := s.pangolinPublicPathEnsure(cl, site.SiteID); err != nil {
		return err
	}
	domainID, err := cl.EnsureDomain(context.Background(), p.Org, p.Domain)
	if err != nil {
		return err
	}
	res, created, err := cl.EnsureResource(context.Background(), p.Org, site.SiteID, rec.Name, domainID, p.Domain)
	if err != nil {
		return fmt.Errorf("pangolin: resource %s: %w", rec.Name, err)
	}
	// Model D (docs/NETWORK.md): the edge authenticates via the CP's OIDC IdP
	// and forwards the member's pubkey; the home gate enforces the app's
	// channel per request. The target is the HOME EDGE (https, SNI = the app
	// vhost) — not the raw app — so the gate runs. Native clients bypass the
	// SSO on the operator-pinned exempt paths (ACCEPT rules) and reach the
	// app's own auth.
	if err := cl.SetTLSName(context.Background(), res.ResourceID, s.appRecordFQDN(rec)); err != nil {
		return fmt.Errorf("pangolin: tls name %s: %w", rec.Name, err)
	}
	if err := cl.SetAccessControl(context.Background(), res.ResourceID, true, []int{s.familyRoleID}, s.idpID); err != nil {
		return fmt.Errorf("pangolin: access control %s: %w", rec.Name, err)
	}
	for i, path := range s.edgeAppExemptPaths(rec.Name) {
		if err := cl.EnsureAcceptRule(context.Background(), res.ResourceID, path, i+1); err != nil {
			return fmt.Errorf("pangolin: exempt rule %s %s: %w", rec.Name, path, err)
		}
	}
	// https to the home edge; Newt (on the gateway) forwards to the k3s node.
	// The vhost's SNI/Host is the app's own name (tlsServerName above).
	if err := cl.SetTargetIfDiffers(context.Background(), res.ResourceID, site.SiteID, s.k3sIP(), 443, "https"); err != nil {
		return fmt.Errorf("pangolin: target %s: %w", rec.Name, err)
	}
	if created {
		fmt.Fprintf(os.Stderr, "pangolin: resource %s created (%s)\n", rec.Name, res.FullDomain)
	}
	return nil
}

// pangolinPublicPathEnsure asserts the WORLD-LEVEL half of the public path
// once per build (the per-app ensure calls it): the CP's OIDC client record
// (sealed to the console identity), the Pangolin global OIDC IdP + the org's
// family role + auto-provision policy, and — when the coords ask — the relay
// host's UNGATED resource (Buzz clients authenticate with their own keys; a
// browser SSO would break every websocket).
func (s *Spec) pangolinPublicPathEnsure(cl *pangolin.Client, siteID int) error {
	if s.publicPathAsserted {
		return nil
	}
	p := s.Pangolin
	ctx := context.Background()
	clientID, clientSecret, redirectURI, err := s.oidcCreds()
	if err != nil || clientID == "" || clientSecret == "" {
		clientID, err = randomHex(24)
		if err != nil {
			return err
		}
		clientSecret, err = randomHex(32)
		if err != nil {
			return err
		}
		redirectURI = ""
		if err := s.sealOIDCCreds(clientID, clientSecret, ""); err != nil {
			return fmt.Errorf("pangolin: seal the OIDC client record: %w", err)
		}
	}
	roleID, err := cl.EnsureRole(ctx, p.Org, "family")
	if err != nil {
		return fmt.Errorf("pangolin: the family role: %w", err)
	}
	idpID, newRedirect, created, err := cl.EnsureOIDCIDP(ctx, "freehold", clientID, clientSecret,
		"https://"+s.CpHost+"/api/oidc/authorize", "https://"+s.CpHost+"/api/oidc/token")
	if err != nil {
		return fmt.Errorf("pangolin: the OIDC IdP: %w", err)
	}
	if created && newRedirect != "" {
		redirectURI = newRedirect
	}
	if redirectURI == "" {
		// Deterministic callback (the dashboard path is fixed once the idp
		// exists): what our authorize endpoint must accept.
		redirectURI = "https://pangolin." + p.Domain + "/auth/idp/" + strconv.Itoa(idpID) + "/oidc/callback"
	}
	if _, _, stored, _ := s.oidcCreds(); stored != redirectURI {
		if err := s.sealOIDCCreds(clientID, clientSecret, redirectURI); err != nil {
			return fmt.Errorf("pangolin: seal the OIDC callback: %w", err)
		}
	}
	if err := cl.AssignIDPOrg(ctx, idpID, p.Org, "'family'"); err != nil {
		return fmt.Errorf("pangolin: the IdP org policy: %w", err)
	}
	s.idpID, s.familyRoleID = idpID, roleID
	s.publicPathAsserted = true
	if !p.Relay {
		return nil
	}
	sub := relaySubdomain(s.RelayHost, p.Domain)
	if sub == "" {
		return fmt.Errorf("pangolin: the relay host %q is not a child of the world domain %q — cannot expose it", s.RelayHost, p.Domain)
	}
	// The relay host's public A record moves to the VPS (the resource routes
	// it there); the CP's own relay DIAL is the raw guest IP, so this changes
	// nothing about how the world reaches its relay.
	if p.DNSIP != "" {
		provider, env, derr := s.dnsCredFromStore("relay")
		if derr != nil {
			return fmt.Errorf("pangolin: relay dns %s: %w", s.RelayHost, derr)
		}
		mgr, merr := dnsman.For(provider, env)
		if merr != nil {
			return fmt.Errorf("pangolin: relay dns %s: %w", s.RelayHost, merr)
		}
		if uerr := mgr.UpsertA(s.RelayHost, p.DNSIP); uerr != nil {
			return fmt.Errorf("pangolin: relay dns %s: %w", s.RelayHost, uerr)
		}
	}
	domainID, err := cl.EnsureDomain(ctx, p.Org, p.Domain)
	if err != nil {
		return err
	}
	res, _, err := cl.EnsureResource(ctx, p.Org, siteID, sub, domainID, p.Domain)
	if err != nil {
		return fmt.Errorf("pangolin: relay resource: %w", err)
	}
	if err := cl.SetAccessControl(ctx, res.ResourceID, false, nil, 0); err != nil {
		return fmt.Errorf("pangolin: relay access control: %w", err)
	}
	if err := cl.SetTargetIfDiffers(ctx, res.ResourceID, siteID, s.RelayIP, 3000, "http"); err != nil {
		return fmt.Errorf("pangolin: relay target: %w", err)
	}
	// The CP's own public hostname rides the public path too. On a NATed world
	// this is the ONLY way Pangolin's server-side OIDC token exchange (it
	// calls our token endpoint back) and the member login reach the CP; on a
	// publicly-reachable world it simply consolidates ingress. UNGATED — the
	// console's own NIP-98 auth and the member login protect it.
	if cpSub := relaySubdomain(s.CpHost, p.Domain); cpSub != "" {
		if p.DNSIP != "" {
			provider, env, derr := s.dnsCredFromStore("relay")
			if derr != nil {
				return fmt.Errorf("pangolin: cp dns %s: %w", s.CpHost, derr)
			}
			mgr, merr := dnsman.For(provider, env)
			if merr != nil {
				return fmt.Errorf("pangolin: cp dns %s: %w", s.CpHost, merr)
			}
			if uerr := mgr.UpsertA(s.CpHost, p.DNSIP); uerr != nil {
				return fmt.Errorf("pangolin: cp dns %s: %w", s.CpHost, uerr)
			}
		}
		cpRes, _, rerr := cl.EnsureResource(ctx, p.Org, siteID, cpSub, domainID, p.Domain)
		if rerr != nil {
			return fmt.Errorf("pangolin: cp resource: %w", rerr)
		}
		if err := cl.SetAccessControl(ctx, cpRes.ResourceID, false, nil, 0); err != nil {
			return fmt.Errorf("pangolin: cp access control: %w", err)
		}
		if err := cl.SetTLSName(ctx, cpRes.ResourceID, s.CpHost); err != nil {
			return fmt.Errorf("pangolin: cp tls name: %w", err)
		}
		if err := cl.SetTargetIfDiffers(ctx, cpRes.ResourceID, siteID, s.k3sIP(), 443, "https"); err != nil {
			return fmt.Errorf("pangolin: cp target: %w", err)
		}
	}
	return nil
}

// relaySubdomain is the relay host's label under the world domain
// (relay.<domain> → "relay"); "" when the host is not a child of it.
func relaySubdomain(relayHost, domain string) string {
	if relayHost == "" || domain == "" || relayHost == domain {
		return ""
	}
	sub := strings.TrimSuffix(relayHost, "."+domain)
	if sub == relayHost || sub == "" {
		return ""
	}
	return sub
}

// edgeAppExemptPaths is the operator's at-source exempt paths for one app
// (the [edge.apps] rider pins) — mirrored onto the Pangolin resource as ACCEPT
// rules: native clients (navidrome /rest, browns /api) authenticate with the
// app, never a browser SSO.
func (s *Spec) edgeAppExemptPaths(name string) []string {
	ea, ok := s.EdgeApps[name]
	if !ok || len(ea.ExemptPaths) == 0 {
		return nil
	}
	out := make([]string, 0, len(ea.ExemptPaths))
	for _, p := range ea.ExemptPaths {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// oidcCreds opens the world's OIDC client record (world-secrets/oidc.json,
// sealed to the console identity): the client id/secret the Pangolin IdP was
// created with, and the callback our authorize endpoint accepts.
func (s *Spec) oidcCreds() (clientID, clientSecret, redirectURI string, err error) {
	root := s.consoleStateRoot()
	path := filepath.Join(root, "world-secrets", "oidc.json")
	if !cert.CredExists(path) {
		return "", "", "", fmt.Errorf("no OIDC client record on the CP at %s", path)
	}
	secret, err := s.consoleEncSecretAt(root)
	if err != nil {
		return "", "", "", err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(path, open, secret)
	if err != nil {
		return "", "", "", fmt.Errorf("OIDC client record: %w", err)
	}
	return env["OIDC_CLIENT_ID"], env["OIDC_CLIENT_SECRET"], env["OIDC_REDIRECT_URI"], nil
}

// sealOIDCCreds seals the OIDC client record to the console identity (the
// same shape the DNS/pangolin creds use).
func (s *Spec) sealOIDCCreds(clientID, clientSecret, redirectURI string) error {
	root := s.consoleStateRoot()
	secret, err := s.consoleEncSecretAt(root)
	if err != nil {
		return err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return err
	}
	return cert.SaveCreds(
		filepath.Join(root, "world-secrets", "oidc.json"),
		"oidc",
		map[string]string{"OIDC_CLIENT_ID": clientID, "OIDC_CLIENT_SECRET": clientSecret, "OIDC_REDIRECT_URI": redirectURI},
		func(recipientPub, aad, plain []byte) ([]byte, error) { return crypto.Seal(recipientPub, aad, plain) },
		pub, "oidc",
	)
}

// EdgeTunnelToken derives the shared secret the edge stamps on every
// forward_auth call; the CP trusts the tunnel's Remote-User identity ONLY when
// it matches (and only from the gateway's address). Deterministic from the
// console identity — the console and agent-tools render the same Caddyfile.
func (s *Spec) EdgeTunnelToken() string {
	secret, err := s.consoleEncSecretAt(s.consoleStateRoot())
	if err != nil || len(secret) != 32 {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("freehold-edge-tunnel-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// randomHex returns n random bytes as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensurePangolinTunnel keeps the CP's side of the public path up: the VPS's
// integration API binds LOOPBACK (the Bearer key never rides cleartext
// internet), so the CP reaches it through an SSH forward to the VPS using
// the CP-minted tunnel key (the VPS's authorized_keys carries its pub —
// written by `freehold pangolin`) — a systemd unit on
// the CP guest, restarted on every assert like every other stage. The
// client's sealed base stays http://127.0.0.1:3003 — this unit IS what
// makes that address mean the VPS.
func (s *Spec) ensurePangolinTunnel() error {
	if s.Pangolin == nil || s.Pangolin.DNSIP == "" {
		return nil // no VPS address on record — nothing to forward to
	}
	vps := s.Pangolin.DNSIP
	// The tunnel key: minted ON the CP guest (its private half never
	// transits — the door key never leaves the box), the pub line the
	// operator's `freehold pangolin` authorized on the VPS.
	if _, err := s.runOut("pct exec "+fmt.Sprint(s.CpLxc)+" -- test -f /srv/data/cp/pangolin-tunnel.key", 30); err != nil {
		return fmt.Errorf("the tunnel key is not on the CP guest (/srv/data/cp/pangolin-tunnel.key) — run `freehold pangolin` (it mints the key and authorizes it on the VPS)")
	}
	// The unit lives ON THE CP GUEST — the stage's execs run on the world
	// host (the runner's target), so the script rides pct exec (the
	// deployAgentTools pattern). Single quotes only OUTSIDE the script.
	unit := fmt.Sprintf("pct exec %d -- sh -c 'set -e\ncat > /etc/systemd/system/pangolin-tunnel.service <<UNIT\n[Unit]\nDescription=Pangolin integration tunnel (freehold public path)\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nExecStart=/usr/bin/ssh -N -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=accept-new -i /srv/data/cp/pangolin-tunnel.key -L 127.0.0.1:3003:127.0.0.1:3003 root@%s\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\nUNIT\nsystemctl daemon-reload\nsystemctl enable pangolin-tunnel >/dev/null 2>&1\nsystemctl restart pangolin-tunnel\nfor i in $(seq 1 30); do\n  curl -sf http://127.0.0.1:3003/v1/ >/dev/null 2>&1 && break\n  sleep 2\ndone\ncurl -sf http://127.0.0.1:3003/v1/ >/dev/null 2>&1 || { echo pangolin-tunnel-never-answered; exit 1; }'", s.CpLxc, vps)
	return s.run(unit, 120)
}

// newtVersion pins the Newt release the gateway stage installs; newtURL is
// its linux-amd64 asset (the guests and the VPS worlds are amd64 — a
// different arch's stage needs the asset name split out).
const (
	newtVersion = "1.18.1"
	newtURL     = "https://github.com/fosrl/newt/releases/download/" + newtVersion + "/newt_linux_amd64"
)

// newtFieldRe bounds the identity fields (generateId's alphabet) and
// newtEndpointRe the endpoint — the only shapes that may reach the gateway
// script (see the interpolation guard in ensureNewtGateway).
var (
	newtFieldRe    = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)
	newtEndpointRe = regexp.MustCompile(`^https?://[A-Za-z0-9.:\-_/@]+$`)
)

// ensureNewtGateway asserts the gateway's Newt tunnel client — the public
// path's dial-out leg, re-asserted by every build like the nftables
// ruleset (docs/NETWORK.md). The binary downloads on the host (best-effort
// when a copy is already installed — an offline host keeps the old one) and
// pushes to the gateway guest; the identity rides an env file sealed to
// root (600) and the unit restarts on every assert, so a re-minted secret
// lands live. On a hosted gateway (api-vultr) the same script runs bare on
// the host. endpoint is the Pangolin instance's external API origin (what
// Newt dials).
func (s *Spec) ensureNewtGateway(endpoint string) error {
	if s.newtAsserted {
		return nil // once per build — N apps re-ensure the stage N times otherwise
	}
	ident, err := s.newtIdentity()
	if err != nil {
		return err
	}
	if endpoint == "" {
		return fmt.Errorf("the pangolin credential lacks PANGOLIN_ENDPOINT (the instance origin Newt dials, e.g. http://vps:3000)")
	}
	// The identity and endpoint interpolate into a ROOT shell script on the
	// gateway — a strict allowlist FIRST, so the instance (or a fat-fingered
	// paste) can never carry a shell metacharacter onto the box.
	if !newtFieldRe.MatchString(ident.ID) {
		return fmt.Errorf("the Newt id %q is not a plain token (^[a-zA-Z0-9-]{1,64}$) — refusing to interpolate it into the gateway script", ident.ID)
	}
	if !newtFieldRe.MatchString(ident.Secret) {
		return fmt.Errorf("the Newt secret is not a plain token (^[a-zA-Z0-9-]{1,128}$) — refusing to interpolate it into the gateway script")
	}
	if !newtEndpointRe.MatchString(endpoint) {
		return fmt.Errorf("the pangolin endpoint %q is not a plain http(s) origin — refusing to interpolate it into the gateway script", endpoint)
	}
	// The binary: present-or-download. The download rides the HOST (the
	// runner's own box) — the guests' outbound varies; the host's is the
	// install path itself.
	var binEnsure string
	if s.HostedGateway {
		binEnsure = fmt.Sprintf(`set -e
if [ ! -x /usr/local/bin/newt ]; then
  curl -fsSL -o /usr/local/bin/newt %s
  chmod 755 /usr/local/bin/newt
fi
[ -x /usr/local/bin/newt ] || { echo "newt download failed and no copy installed"; exit 1; }
`, newtURL)
	} else {
		binEnsure = fmt.Sprintf(`set -e
if ! pct exec %d -- test -x /usr/local/bin/newt; then
  curl -fsSL -o /tmp/fh-newt %s
  pct push %d /tmp/fh-newt /usr/local/bin/newt
  pct exec %d -- chmod 755 /usr/local/bin/newt
fi
pct exec %d -- test -x /usr/local/bin/newt
`, s.GatewayLxc, newtURL, s.GatewayLxc, s.GatewayLxc, s.GatewayLxc)
	}
	gw := func(script string) string {
		if s.HostedGateway {
			return script
		}
		return fmt.Sprintf("pct exec %d -- sh -c '%s'", s.GatewayLxc, script)
	}
	unit := fmt.Sprintf(`set -e
cat > /etc/freehold-newt.env <<ENV
NEWT_ID=%s
NEWT_SECRET=%s
NEWT_ENDPOINT=%s
ENV
chmod 600 /etc/freehold-newt.env
cat > /etc/systemd/system/newt.service <<UNIT
[Unit]
Description=Newt tunnel client (freehold public path)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/freehold-newt.env
ExecStart=/usr/local/bin/newt --id \${NEWT_ID} --secret \${NEWT_SECRET} --endpoint \${NEWT_ENDPOINT}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable newt >/dev/null 2>&1
systemctl restart newt
`, ident.ID, ident.Secret, endpoint)
	if err := s.run(binEnsure, 300); err != nil {
		return fmt.Errorf("the newt binary: %w", err)
	}
	if err := s.run(gw(unit), 120); err != nil {
		return fmt.Errorf("the newt unit: %w", err)
	}
	s.newtAsserted = true
	return nil
}

// newtIdentity opens the sealed Newt identity (world-secrets/
// pangolin-newt.json — written when the ensure created the site; the
// operator can also seal one by hand for a pre-existing site). Absent =
// fail loud with the recovery: the site was minted outside this world's
// first ensure, so its one-time secret was never handed to the CP.
func (s *Spec) newtIdentity() (struct{ ID, Secret string }, error) {
	var out struct{ ID, Secret string }
	root := s.consoleStateRoot()
	path := filepath.Join(root, "world-secrets", "pangolin-newt.json")
	if !cert.CredExists(path) {
		return out, fmt.Errorf("no Newt identity on the CP at %s — the pangolin site predates this world's first ensure: remove the site on the instance and re-run the build (the ensure re-creates it and seals the one-time secret)", path)
	}
	secret, err := s.consoleEncSecretAt(root)
	if err != nil {
		return out, err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(path, open, secret)
	if err != nil {
		return out, fmt.Errorf("the Newt identity: %w", err)
	}
	out.ID, out.Secret = env["NEWT_ID"], env["NEWT_SECRET"]
	if out.ID == "" || out.Secret == "" {
		return out, fmt.Errorf("the Newt identity record lacks NEWT_ID/NEWT_SECRET")
	}
	return out, nil
}

// pangolinRides reports whether a registry app rides the world's public path:
// the coords' allowlist decides (empty = every registry app). "We have a
// public path" is not "expose everything" — an app not listed keeps its A
// record on the proxy and gets no resource.
func (s *Spec) pangolinRides(name string) bool {
	if s.Pangolin == nil {
		return false
	}
	if len(s.Pangolin.Apps) == 0 {
		return true
	}
	for _, n := range s.Pangolin.Apps {
		if n == name {
			return true
		}
	}
	return false
}

// pangolinHostDNSEnsure points the edge's own hostname (pangolin.<world>)
// at the VPS: its ACME cert issues there (HTTP-01, on-demand) and Newt dials
// TLS through it. The CP owns the zone credential — a box installed without
// one has no local copy, so the record is asserted here, BEFORE the Newt
// stage dials. The dev/test posture (no DNSIP) moves no public DNS.
func (s *Spec) pangolinHostDNSEnsure() error {
	p := s.Pangolin
	if p == nil || p.Domain == "" || p.DNSIP == "" {
		return nil
	}
	host := "pangolin." + p.Domain
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		return fmt.Errorf("pangolin host dns %s: %w", host, err)
	}
	mgr, err := dnsman.For(provider, env)
	if err != nil {
		return fmt.Errorf("pangolin host dns %s: %w", host, err)
	}
	if err := mgr.UpsertA(host, p.DNSIP); err != nil {
		return fmt.Errorf("pangolin host dns %s: %w", host, err)
	}
	return nil
}

// pangolinRemove takes one app off the Pangolin public path: resolve the
// resource by its fullDomain and delete it (the vhost + its target die with
// it). Best-effort — the report names the failure, the next unexpose
// converges.
func (s *Spec) pangolinRemove(rec agenttools.AppRecord) error {
	p := s.Pangolin
	if p == nil || rec.Visibility == agenttools.VisibilityLAN {
		return nil
	}
	base, key, _, err := s.pangolinCreds()
	if err != nil {
		return err
	}
	cl := &pangolin.Client{Base: base, Key: key}
	resources, err := cl.ListResources(context.Background(), p.Org)
	if err != nil {
		return err
	}
	want := rec.Name + "." + p.Domain
	// The override may have moved the tree since the resource was created:
	// remove BOTH shapes (the recorded base and the effective root), so a
	// renamed world's old resource doesn't linger on the edge.
	wants := []string{want}
	if base := config.EffectiveWorldDomain(s.CpHost, s.WorldDomainOverride); base != p.Domain {
		wants = append(wants, rec.Name+"."+base)
	}
	for _, r := range resources {
		for _, w := range wants {
			if r.FullDomain == w {
				if err := cl.DeleteResource(context.Background(), r.ResourceID); err != nil {
					return err
				}
			}
		}
	}
	return nil // nothing on the path — already gone
}

// sealNewtIdentity writes a created site's one-time Newt identity sealed to
// the console identity — the SAME audience the DNS creds use, so the build
// tail (console-side, in either process) can open it for the gateway stage.
func (s *Spec) sealNewtIdentity(site pangolin.Site) error {
	root := s.consoleStateRoot()
	secret, err := s.consoleEncSecretAt(root)
	if err != nil {
		return err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return err
	}
	return cert.SaveCreds(
		filepath.Join(root, "world-secrets", "pangolin-newt.json"),
		"pangolin-newt",
		map[string]string{"NEWT_ID": site.NewtID, "NEWT_SECRET": site.NewtSecret},
		func(recipientPub, aad, plain []byte) ([]byte, error) { return crypto.Seal(recipientPub, aad, plain) },
		pub, "pangolin-newt",
	)
}

// renderCaddyfile renders the CURRENT edge config from the world coords +
// the apps registry — the ONE render both appliers share: the build's
// terraform var and the verb's kubectl apply (identical output, so they
// converge, never fight). Missing the relay/cp hosts → "" (the caller
// skips). An app's site block rides ONLY when its cert is on the durable
// mirror (cert-before-config is the edge-safety invariant).
func (s *Spec) renderCaddyfile() string {
	if s.RelayHost == "" || s.RelayIP == "" || s.CpHost == "" {
		return ""
	}
	var apps []caddydeploy.AppVhost
	if s.Apps != nil {
		// Cert-before-config: the apps' blocks ride only when the SHARED
		// wildcard cert exists AND covers the world domain's wildcard — the
		// same gate appsCertEnsure's reuse path applies. A mirror cert for a
		// stale shape (the world domain moved; the fresh issue failed) is
		// VALID but covers the wrong name — rendering vhosts behind it would
		// TLS-mismatch every app the DNS step already repointed.
		wc, err := s.durableFullchain(s.K3sVmid, appsCertSlot)
		if err == nil {
			if _, ok := cert.ReuseIfValidBytes(wc, time.Now(), 0); ok && certCovers(wc, s.appWildcard()) {
				registry := map[string]bool{}
				for _, rec := range s.Apps.List() {
					registry[rec.Name] = true
					apps = append(apps, s.appVhost(rec))
				}
				apps = append(apps, s.edgeRiders(registry)...)
			}
		}
	}
	// The public path's gate assertion: the shared token lets the CP trust the
	// tunnel's Remote-User identity (the Pangolin SSO's member pubkey) when the
	// original client is the gateway. Only a world WITH the public path renders
	// it (a direct-edge world has no tunneled requests); absent the console
	// identity it is "" and the cookie gate alone applies.
	if s.Pangolin != nil {
		if token := s.EdgeTunnelToken(); token != "" {
			for i := range apps {
				apps[i].TunnelToken = token
			}
		}
	}
	pair := ""
	if s.K3sIP != "" {
		pair = s.K3sIP + ":5000"
	}
	cpUpstream, cpMcpUpstream := "", ""
	if s.CpIP != "" {
		cpUpstream = s.CpIP + ":8080"
		cpMcpUpstream = s.CpIP + ":8089"
	}
	// Cert-before-config applies to the PORTAL vhost too: it renders only
	// when its cert is on the durable mirror AND covers the world root — on
	// the first build after a rename the mirror is empty, the block is
	// skipped, and the next build renders it (worldCert issued the cert by
	// then). An ungated render would crash-loop the whole edge on the missing
	// tls files.
	portalHost := s.portalHost()
	if portalHost != "" {
		if pc, err := s.durableFullchain(s.K3sVmid, portalCertSlot); err != nil {
			portalHost = ""
		} else if _, ok := cert.ReuseIfValidBytes(pc, time.Now(), 0); !ok || !certCovers(pc, portalHost) {
			portalHost = ""
		}
	}
	return caddydeploy.RenderCaddyfileApps(
		s.RelayHost, s.RelayIP+":3000", pair,
		s.CpHost, cpUpstream, cpMcpUpstream,
		portalHost,
		apps)
}

// applyCaddyfile applies the rendered edge config through the co-located
// runner: file-transit the rendered Caddyfile to the k3s guest, then
// `kubectl create --dry-run | apply` (no JSON quoting of a large config) and
// a rollout restart (subPath configMaps re-read only on restart; the Recreate
// strategy is already the deployment's). The next terraform apply renders the
// identical config from the same renderer — the hand-apply converges.
func (s *Spec) applyCaddyfile() error {
	body := s.renderCaddyfile()
	if body == "" {
		return fmt.Errorf("edge config: the world coords lack the relay/cp hosts to render from")
	}
	tmp, err := os.CreateTemp("", "fh-caddyfile-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if s.uploadHook != nil {
		if err := s.uploadHook(s.RunnerTarget, tmp.Name(), "/tmp/fh-caddyfile", 60); err != nil {
			return fmt.Errorf("edge config upload: %w", err)
		}
	} else {
		mc, err := s.client()
		if err != nil {
			return err
		}
		if _, err := mc.Upload(s.RunnerTarget, tmp.Name(), "/tmp/fh-caddyfile", 60); err != nil {
			return fmt.Errorf("edge config upload: %w", err)
		}
	}
	cmd := `set -e
pct push %[1]d /tmp/fh-caddyfile /tmp/fh-caddyfile
pct exec %[1]d -- sh -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
$K -n caddy create configmap caddy-caddyfile --from-file=Caddyfile=/tmp/fh-caddyfile --dry-run=client -o yaml | $K apply -f -
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
rm -f /tmp/fh-caddyfile
'`
	return s.run(fmt.Sprintf(cmd, s.K3sVmid), 180)
}

// worldApps is the build tail's apps ensure: every registered app gets its
// DNS pointed and its cert issued/seeded, and the edge config applies ONCE
// at the end. Idempotent — same records, same state. The registry is
// RE-READ fresh (expose writes land in the separate agent-tools process; the
// console executor's Spec was built at serve start). One record's failure
// (a revoked cred, an unreachable provider) is REPORTED and the tail moves
// on — the other apps still come up, and the failure is loud in the report,
// never silent.
func (s *Spec) worldApps() (string, error) {
	s.newtAsserted = false       // a fresh build re-asserts the dial-out leg
	s.publicPathAsserted = false // ... and the world-level SSO stages
	if s.Apps == nil {
		return "", nil
	}
	if err := s.Apps.Reload(); err != nil {
		return "", fmt.Errorf("apps registry reload: %w", err)
	}
	records := s.Apps.List()
	// Riders alone carry the tail on a registry-less world: their DNS, the
	// shared wildcard, and the edge render must still run, or the operator's
	// pins are fully inert while the build reports success.
	if len(records) == 0 && len(s.EdgeApps) == 0 {
		return "", nil
	}
	var failed []string
	failedNames := map[string]bool{}
	for _, rec := range records {
		recordFailed := false
		if s.Pangolin != nil {
			if err := s.pangolinEnsure(rec); err != nil {
				// The DNS leg SKIPS: pointing the A record at the VPS
				// while no resource exists there is exactly the public
				// name aimed at an edge with no route. The record keeps
				// riding the direct path (or its last good state) until
				// the next build re-ensures.
				failed = append(failed, rec.Name+" pangolin: "+err.Error())
				failedNames[rec.Name] = true
				continue
			}
		}
		if _, err := s.appDNSEnsure(rec); err != nil {
			failed = append(failed, rec.Name+" dns: "+err.Error())
			recordFailed = true
		}
		if recordFailed {
			failedNames[rec.Name] = true
		}
	}
	// The riders' DNS: a rider vhost whose name is NXDOMAIN is the
	// operator's pin silently dead — the same surface the registry apps get.
	for name, ea := range s.EdgeApps {
		if ea.Upstream == "" || failedNames[name] {
			continue
		}
		inRegistry := false
		for _, rec := range records {
			if rec.Name == name {
				inRegistry = true
				break
			}
		}
		if inRegistry {
			continue
		}
		if _, err := s.edgeRiderDNSEnsure(name); err != nil {
			failed = append(failed, name+" rider dns: "+err.Error())
		}
	}
	if _, err := s.appsCertEnsure(); err != nil {
		failed = append(failed, "shared wildcard cert: "+err.Error())
	}
	if err := s.applyCaddyfile(); err != nil {
		return "", fmt.Errorf("edge config: %w", err)
	}
	if len(failed) > 0 {
		return fmt.Sprintf("apps reconciled (%d of %d; FAILED: %s)", len(records)-len(failedNames), len(records), strings.Join(failed, "; ")), nil
	}
	return fmt.Sprintf("apps reconciled (%d)", len(records)), nil
}

// BuildExposeAppFn binds expose_app to the Spec (the world coords + the
// co-located runner). The registry write is the INTENT: it lands first, the
// apply follows; a failed apply leaves the row for the next build to
// re-ensure, and the report names which legs landed — never a silent half.
func BuildExposeAppFn(spec *Spec, apps *agenttools.AppsStore, consoleSecret []byte) func(agent.ExposeArgs) (string, error) {
	return func(args agent.ExposeArgs) (string, error) {
		spec.newtAsserted = false // a fresh expose re-asserts the dial-out leg
		if spec.CpHost == "" || spec.CpIP == "" || spec.RelayHost == "" || spec.RelayIP == "" || spec.ProxyIP == "" || spec.K3sVmid == 0 {
			return "", fmt.Errorf("expose: the world coords are missing (relay/cp host+ip, proxy ip, k3s vmid) — exposure is world machinery")
		}
		if apps == nil {
			return "", fmt.Errorf("expose: the apps registry is not bound")
		}
		if appDomainIsZone(spec.CpHost) {
			return "", fmt.Errorf("expose: the cp host %q is the zone itself — its app wildcard would cover the whole shared zone (and collide with the cp cert's challenge)", spec.CpHost)
		}
		fqdn := appFQDN(spec.CpHost, spec.WorldDomainOverride, args.Name)
		if fqdn == spec.RelayHost || fqdn == spec.CpHost {
			return "", fmt.Errorf("expose %q: %s is the edge's own host — pick another name", args.Name, fqdn)
		}
		dial, auth := spec.RelayURL, spec.RelayAuthURL
		if auth == "" {
			auth = dial
		}
		gid, err := groupID(dial, auth, consoleSecret, args.Group)
		if err != nil {
			return "", err
		}
		requester := args.Requester
		if requester == "" {
			return "", fmt.Errorf("expose: no requester (the pubkey of whoever asked — their group membership bounds the grant)")
		}
		if err := requesterInGroup(spec.AgentRegistry, spec.OwnerPub, requester, dial, auth, consoleSecret, gid); err != nil {
			return "", err
		}
		visibility, auth := args.Visibility, args.Auth
		if visibility == "" {
			visibility = agenttools.VisibilityFamily
		}
		if auth == "" {
			auth = agenttools.AuthGate
		}
		rec := agenttools.AppRecord{
			Name: args.Name, FQDN: fqdn, Target: args.Target,
			Visibility: visibility, Auth: auth,
			Group: gid, Owner: spec.Audience, Requester: requester,
		}
		if err := apps.Expose(rec); err != nil {
			return "", err
		}
		report := fmt.Sprintf("%s → https://%s (group %s, visibility %s, auth %s)",
			rec.Name, rec.FQDN, gid, rec.Visibility, rec.Auth)
		if spec.Pangolin != nil {
			// The resource exists BEFORE the A record points at the VPS —
			// a die-between-steps build never leaves the public name aimed
			// at an edge that has no route for it.
			if err := spec.pangolinEnsure(rec); err != nil {
				return report + " — RECORDED; the pangolin step failed and the next build re-ensures it: " + err.Error(), nil
			}
		}
		if _, err := spec.appDNSEnsure(rec); err != nil {
			return report + " — RECORDED; the DNS step failed and the next build re-ensures it: " + err.Error(), nil
		}
		if _, err := spec.appsCertEnsure(); err != nil {
			return report + " — RECORDED and DNS pointed; the wildcard cert step failed and the next build re-ensures it: " + err.Error(), nil
		}
		if err := spec.applyCaddyfile(); err != nil {
			return report + " — RECORDED, DNS pointed, cert installed; the edge config apply failed and the next build re-ensures it: " + err.Error(), nil
		}
		return report + " — live", nil
	}
}

// BuildUnexposeAppFn binds unexpose_app: remove the record, re-apply the
// edge config (the vhost drops out), best-effort DNS cleanup. The cert stays
// on the durable mirror — harmless, and the zone record outlives it either
// way.
func BuildUnexposeAppFn(spec *Spec, apps *agenttools.AppsStore) func(name string) (string, error) {
	return func(name string) (string, error) {
		rec, err := apps.Unexpose(name)
		if err != nil {
			return "", err
		}
		report := fmt.Sprintf("unexposed %s (%s)", name, rec.FQDN)
		edgeNote := ""
		if err := spec.applyCaddyfile(); err != nil {
			edgeNote = "; the edge config apply failed (the local vhost may still serve until the next build): " + err.Error()
		}
		removal := ""
		if err := spec.pangolinRemove(rec); err != nil {
			// The record is GONE (unexpose is record-first) — "the next
			// unexpose" can never run. The real recovery: re-expose then
			// unexpose again, which re-ensures the row and retries the
			// removal.
			removal = "; the pangolin resource could not be removed (it may still serve on the VPS) — re-expose " + rec.Name + " then unexpose it again to retry: " + err.Error()
		}
		dnsNote := ""
		if rec.Visibility != agenttools.VisibilityLAN {
			if provider, env, err := spec.dnsCredFromStore("relay"); err == nil {
				if mgr, merr := dnsman.For(provider, env); merr == nil {
					if derr := mgr.DeleteA(spec.appRecordFQDN(rec)); derr != nil {
						dnsNote = "; the A record delete failed: " + derr.Error()
					}
				}
			}
		}
		if edgeNote != "" || removal != "" || dnsNote != "" {
			return report + " — RECORDED" + edgeNote + removal + dnsNote, nil
		}
		return report + " — live", nil
	}
}
