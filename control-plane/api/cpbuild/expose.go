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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"freehold/contract/relay"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	cert "freehold/platform/services/certificates/letsencrypt"
	dnsman "freehold/platform/services/externaldns/cloudflare"
	caddydeploy "freehold/platform/services/webproxy/caddy"
)

// appDomainBase strips the CP host's own label: cp.librem.freehold.technology
// → librem.freehold.technology — the base app fqdns compose onto.
func appDomainBase(cpHost string) string {
	return strings.TrimPrefix(cpHost, "cp.")
}

// appFQDN is an app's public hostname.
func appFQDN(cpHost, name string) string {
	return name + "." + appDomainBase(cpHost)
}

// appSlot is an app cert's slot name (the PVC + durable-mirror dir).
func appSlot(name string) string { return "app-" + name }

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

// appVhost builds one registry row's site block. Gated apps forward through
// the console's member gate on the CP guest (the CP's LAN IP, port 8080).
func (s *Spec) appVhost(rec agenttools.AppRecord) caddydeploy.AppVhost {
	gate := ""
	if rec.Auth == agenttools.AuthGate {
		gate = s.CpIP + ":8080"
	}
	return caddydeploy.AppVhost{
		FQDN:     rec.FQDN,
		Upstream: rec.Target,
		Slot:     appSlot(rec.Name),
		Gate:     gate,
	}
}

// appCertEnsure runs an app slot's cert chain: the durable-reuse gate (a
// valid mirror cert seeds the PVC — no LE order), then the box seed cache,
// then the in-process resumable DNS-01 issue. Returns whether a cert was
// installed NOW (vs already present). The zone credential comes from the
// relay slot's sealed store — one token, one zone, every slot.
func (s *Spec) appCertEnsure(slot, host string) (bool, error) {
	if fc, err := s.durableFullchain(s.K3sVmid, slot); err == nil && s.durableKeyPresent(s.K3sVmid, slot) {
		if _, ok := cert.ReuseIfValidBytes(fc, time.Now(), 30*24*time.Hour); ok {
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
	if n, perr := cert.PurgeChallengeRecords(host, provider, env); perr == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "cert %s: cleaned %d challenge record(s) after issue\n", slot, n)
	}
	return true, nil
}

// appDNSEnsure points the app's public A record at the proxy. LAN visibility
// skips it: no public record — the name resolves only inside the world.
func (s *Spec) appDNSEnsure(rec agenttools.AppRecord) (bool, error) {
	if rec.Visibility == agenttools.VisibilityLAN {
		return false, nil
	}
	if s.ProxyIP == "" {
		return false, fmt.Errorf("app dns %s: the proxy IP is not in the world coords", rec.FQDN)
	}
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		return false, fmt.Errorf("app dns %s: %w", rec.FQDN, err)
	}
	mgr, err := dnsman.For(provider, env)
	if err != nil {
		return false, fmt.Errorf("app dns %s: %w", rec.FQDN, err)
	}
	if err := mgr.UpsertA(rec.FQDN, s.ProxyIP); err != nil {
		return false, fmt.Errorf("app dns %s: %w", rec.FQDN, err)
	}
	return true, nil
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
		for _, rec := range s.Apps.List() {
			fc, err := s.durableFullchain(s.K3sVmid, appSlot(rec.Name))
			if err != nil {
				continue
			}
			if _, ok := cert.ReuseIfValidBytes(fc, time.Now(), 0); ok {
				apps = append(apps, s.appVhost(rec))
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
	return caddydeploy.RenderCaddyfileApps(
		s.RelayHost, s.RelayIP+":3000", pair,
		s.CpHost, cpUpstream, cpMcpUpstream,
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
	if s.Apps == nil {
		return "", nil
	}
	if err := s.Apps.Reload(); err != nil {
		return "", fmt.Errorf("apps registry reload: %w", err)
	}
	records := s.Apps.List()
	if len(records) == 0 {
		return "", nil
	}
	var failed []string
	for _, rec := range records {
		if _, err := s.appDNSEnsure(rec); err != nil {
			failed = append(failed, rec.Name+" dns: "+err.Error())
			continue
		}
		if _, err := s.appCertEnsure(appSlot(rec.Name), rec.FQDN); err != nil {
			failed = append(failed, rec.Name+" cert: "+err.Error())
		}
	}
	if err := s.applyCaddyfile(); err != nil {
		return "", fmt.Errorf("edge config: %w", err)
	}
	if len(failed) > 0 {
		return fmt.Sprintf("apps reconciled (%d of %d; FAILED: %s)", len(records)-len(failed), len(records), strings.Join(failed, "; ")), nil
	}
	return fmt.Sprintf("apps reconciled (%d)", len(records)), nil
}

// BuildExposeAppFn binds expose_app to the Spec (the world coords + the
// co-located runner). The registry write is the INTENT: it lands first, the
// apply follows; a failed apply leaves the row for the next build to
// re-ensure, and the report names which legs landed — never a silent half.
func BuildExposeAppFn(spec *Spec, apps *agenttools.AppsStore, consoleSecret []byte) func(agent.ExposeArgs) (string, error) {
	return func(args agent.ExposeArgs) (string, error) {
		if spec.CpHost == "" || spec.CpIP == "" || spec.RelayHost == "" || spec.RelayIP == "" || spec.ProxyIP == "" || spec.K3sVmid == 0 {
			return "", fmt.Errorf("expose: the world coords are missing (relay/cp host+ip, proxy ip, k3s vmid) — exposure is world machinery")
		}
		if apps == nil {
			return "", fmt.Errorf("expose: the apps registry is not bound")
		}
		fqdn := appFQDN(spec.CpHost, args.Name)
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
		if _, err := spec.appDNSEnsure(rec); err != nil {
			return report + " — RECORDED; the DNS step failed and the next build re-ensures it: " + err.Error(), nil
		}
		if _, err := spec.appCertEnsure(appSlot(rec.Name), rec.FQDN); err != nil {
			return report + " — RECORDED and DNS pointed; the cert step failed and the next build re-ensures it: " + err.Error(), nil
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
		if err := spec.applyCaddyfile(); err != nil {
			return report + " — RECORDED; the edge config apply failed (the vhost may still serve until the next build): " + err.Error(), nil
		}
		if rec.Visibility != agenttools.VisibilityLAN {
			if provider, env, err := spec.dnsCredFromStore("relay"); err == nil {
				if mgr, merr := dnsman.For(provider, env); merr == nil {
					_ = mgr.DeleteA(rec.FQDN) // best-effort
				}
			}
		}
		return report + " — live", nil
	}
}
