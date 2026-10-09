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
	if _, err := s.requireSession(r); err != nil {
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
