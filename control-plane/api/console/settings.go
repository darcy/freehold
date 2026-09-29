package console

import (
	"encoding/json"
	"net/http"
	"time"

	"freehold/control-plane/state"
)

// The operator settings surface (GET/POST /api/settings): the CP's
// operator-editable configuration — state.Settings — read/written through the
// console so the web UI, the TUI (contract/console client) and the
// `freehold-console settings` CLI verb all edit the same record.

// settingsGet serves the current settings. Read fresh from disk (the same
// re-open discipline as dnsList/overview): an out-of-band `freehold-console
// settings` write is visible without a serve restart.
func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"settings": fresh.Snapshot().Settings})
}

// settingsSet replaces the settings (POST /api/settings). The timezone is
// validated at this trust boundary (a bad IANA name would silently UTC every
// agent pod); empty clears it (pods run UTC).
func (s *Server) settingsSet(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req struct {
		OperatorTZ *string `json:"operator_tz"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad settings body")
		return
	}
	if req.OperatorTZ == nil {
		writeErr(w, http.StatusBadRequest, "settings body carries no operator_tz")
		return
	}
	tz := *req.OperatorTZ
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			writeErr(w, http.StatusBadRequest, "unknown timezone "+tz+" (want an IANA name, e.g. America/Chicago)")
			return
		}
	}
	// Read-modify-write through a FRESH store so another process's settings
	// write (the CLI verb) is never clobbered by this serve's snapshot.
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	set := fresh.Snapshot().Settings
	if set == nil {
		set = &state.Settings{}
	}
	set.OperatorTZ = tz
	if err := fresh.SetSettings(set); err != nil {
		writeErr(w, http.StatusInternalServerError, "save settings: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "settings": set})
}
