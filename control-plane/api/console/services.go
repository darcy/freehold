package console

import (
	"crypto/tls"
	"io"
	"net/http"
	"sort"
	"time"

	"freehold/control-plane/state"
)

// WorldServiceJSON is the /api/world services row: the recorded coords plus the
// co-located live-probe result, so any logged-in management box renders the
// live world instead of only the box that deployed it.
type WorldServiceJSON struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Up     bool   `json:"up"`
	Detail string `json:"detail"`
}

// worldServiceRows builds the sorted /api/world services list, probing each
// recorded service co-located from the CP's own LXC (the CP sits beside k3s,
// litellm and the Caddy edge on the world network).
func worldServiceRows(snap state.ControlPlaneState) []WorldServiceJSON {
	rows := make([]WorldServiceJSON, 0, len(snap.Services))
	names := make([]string, 0, len(snap.Services))
	for name := range snap.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := snap.Services[name]
		up, detail := probeWorldService(svc)
		rows = append(rows, WorldServiceJSON{Name: name, Kind: svc.Kind, URL: svc.URL, Up: up, Detail: detail})
	}
	return rows
}

// probeWorldService dispatches on the service kind to a short-round-trip,
// never-holds-the-box-up probe. Public coords only — nothing secret here.
func probeWorldService(s state.WorldService) (bool, string) {
	switch s.Kind {
	case "k3s":
		// Self-signed https control plane; any HTTP round-trip (a 401 for the
		// unauth client is normal) means the API is listening.
		return answered(s.URL, true)
	case "litellm":
		// Gateway health endpoint must answer 2xx.
		return healthy(s.URL)
	case "caddy":
		return answered(s.URL, false)
	default:
		return false, "unknown kind"
	}
}

// answered reports whether an HTTP GET completes a round-trip (any status,
// including 4xx/5xx = the server is up). k3s/caddy probes use this.
func answered(url string, insecure bool) (bool, string) {
	c := &http.Client{Timeout: 4 * time.Second}
	if insecure {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // self-signed k3s API probe
	}
	resp, err := c.Get(url)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return true, resp.Status
}

// healthy reports whether an HTTP GET answers 2xx (litellm /health).
func healthy(url string) (bool, string) {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, resp.Status
	}
	return false, resp.Status
}
