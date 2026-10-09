// The apps surface (operator): the launcher feed — every exposed app's
// record, served from the toolset's durable apps registry (the same rows the
// build re-ensures and the edge config renders from). The channel NAME is
// resolved fresh per request (the registry stores the channel ID — names
// change; the record must not).
package console

import (
	"net/http"
	"path/filepath"

	"freehold/contract/relay"
	"freehold/control-plane/api/agenttools"
)

func (s *Server) appsList(w http.ResponseWriter, r *http.Request) {
	// OPERATOR-only: the records carry internal targets + pubkeys — a
	// member session (any relay member) must not enumerate them. The same
	// admin gate every other /api surface holds.
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	apps, err := agenttools.OpenApps(filepath.Join(s.AgentToolsDir, "apps.json"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	records := apps.List()
	// id → display name, one relay read for the whole list (the record holds
	// the channel ID; the name is display-only and resolved fresh).
	names := map[string]string{}
	_ = s.Store.Reload()
	dial, auth := s.relayDialAuth(s.Store.Snapshot())
	if dial != "" {
		if groups, gerr := relay.QueryGroupsAuth(dial, auth, s.ConsoleSecret); gerr == nil {
			for _, g := range groups {
				names[g.ID] = g.Name
			}
		}
	}
	out := make([]map[string]interface{}, 0, len(records))
	for _, rec := range records {
		out = append(out, map[string]interface{}{
			"name": rec.Name, "fqdn": rec.FQDN, "target": rec.Target,
			"visibility": rec.Visibility, "auth": rec.Auth,
			"group": rec.Group, "group_name": names[rec.Group],
			"owner": rec.Owner, "requester": rec.Requester, "created_at": rec.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"apps": out})
}

// The PORTAL (session-aware): the apps THIS session can open — the launcher
// the console renders as tiles. The operator sees all; a member's channel
// roster admits them (the same check the verify gate runs, cache included);
// a device session's static list binds it. Same redaction as the operator
// feed's member boundary: names + URLs only — no targets, no owners.
func (s *Server) myApps(w http.ResponseWriter, r *http.Request) {
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	pk, role, err := s.sessionFor(r)
	device := false
	var bound []string
	if err != nil && s.Members != nil {
		if info, ok := s.Members.MemberIdentity(cookieValue(r, memberCookie)); ok && (info.Pubkey != "" || info.Device) {
			pk, role, device, bound, err = info.Pubkey, RoleMember, info.Device, info.Apps, nil
		}
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if role != RoleOperator && s.Members == nil {
		// A member-role console session on a tier-less console: nothing to
		// admit against — the empty launcher is the honest answer.
		writeJSON(w, http.StatusOK, map[string]interface{}{"apps": []map[string]interface{}{}})
		return
	}
	apps, err := agenttools.OpenApps(filepath.Join(s.AgentToolsDir, "apps.json"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]interface{}{}
	for _, rec := range apps.List() {
		switch {
		case role == RoleOperator:
			// all of them
		case device:
			allowed := false
			for _, n := range bound {
				if n == rec.Name {
					allowed = true
				}
			}
			if !allowed {
				continue // the link opens exactly the apps it was minted for
			}
		default:
			ok, aerr := s.memberChannelAllowed(pk, rec.Group)
			if aerr != nil || !ok {
				continue // fail-closed: a relay outage lists nothing
			}
		}
		out = append(out, map[string]interface{}{"name": rec.Name, "fqdn": rec.FQDN})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"apps": out})
}
