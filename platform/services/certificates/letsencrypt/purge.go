package cert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// purgeChallengeRecords removes every TXT record at `_acme-challenge.<host>`
// from the DNS provider (Cloudflare), returning how many were deleted.
//
// Why: lego's DNS-01 Present ADDS a value to that name and nothing in the
// issuance path ever removes it — Resume only discards the resumable-order
// state file, not the placed record. Every future issuance (each a NEW ACME
// order, and each contributes to rate-limit exposure) then STACKS another TXT
// value onto the same name, so the zone quietly accumulates orphaned
// challenge records (exactly the redundant records surfaced on librem /
// relay.migrate). Purging before placing and again after success keeps the
// name clean — and this is API-based, so it works even when the record isn't
// resolvable yet (the provider sees it in its own API).
//
// Only the cloudflare challenge provider is wired here; other providers are a
// no-op (their lego implementation manages cleanup internally).
func purgeChallengeRecords(host, providerName string, env map[string]string) (int, error) {
	if providerName != "cloudflare" {
		return 0, nil
	}
	token := env["CLOUDFLARE_DNS_API_TOKEN"]
	apiKey := env["CLOUDFLARE_API_KEY"]
	email := env["CLOUDFLARE_EMAIL"]
	if token == "" && apiKey == "" {
		return 0, errors.New("cloudflare purge: no CLOUDFLARE_DNS_API_TOKEN / CLOUDFLARE_API_KEY in cred")
	}
	base := env["CLOUDFLARE_BASE_URL"]
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}

	name := "_acme-challenge." + strings.TrimSuffix(host, ".") + "."
	client := &http.Client{Timeout: 20 * time.Second}

	// Find the covering zone (longest zone whose name the record is under).
	zoneID, err := findCoveringZone(client, base, token, apiKey, email, name)
	if err != nil {
		return 0, err
	}
	if zoneID == "" {
		return 0, errors.New("cloudflare purge: no zone covers " + name)
	}

	// List the current TXT records at that name, then delete each.
	ids, err := listTXTRecordIds(client, base, token, apiKey, email, zoneID, name)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		req, err := http.NewRequest(http.MethodDelete, base+"/zones/"+zoneID+"/dns_records/"+id, nil)
		if err != nil {
			return n, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("X-Auth-Key", apiKey)
			req.Header.Set("X-Auth-Email", email)
		}
		resp, err := client.Do(req)
		if err != nil {
			return n, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		n++
	}
	return n, nil
}

// findCoveringZone lists the account's zones and returns the id of the one
// whose name is the longest suffix of the record name (the covering zone).
func findCoveringZone(client *http.Client, base, token, apiKey, email, name string) (string, error) {
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	page := 1
	for {
		url := fmt.Sprintf("%s/zones?per_page=50&page=%d", base, page)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("X-Auth-Key", apiKey)
			req.Header.Set("X-Auth-Email", email)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("cloudflare purge list zones: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("cloudflare purge list zones: HTTP %d: %s", resp.StatusCode, trunc(body))
		}
		var out struct {
			Success  bool `json:"success"`
			Result   []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", err
		}
		if !out.Success {
			return "", errors.New("cloudflare purge list zones: API not success")
		}
		zones = append(zones, out.Result...)
		if out.ResultInfo.Page >= out.ResultInfo.TotalPages || out.ResultInfo.TotalPages == 0 {
			break
		}
		page++
	}
	// Longest zone name that suffixes the record name (dot-normalized).
	best, bestName := "", ""
	match := strings.TrimSuffix(name, ".")
	for _, z := range zones {
		zn := strings.TrimPrefix(strings.TrimSuffix(z.Name, "."), "*.")
		suffix := "." + zn
		if strings.HasSuffix(match, suffix) && len(zn) > len(bestName) {
			best, bestName = z.ID, zn
		}
	}
	return best, nil
}

// listTXTRecordIds lists the DNS record ids of type TXT at the given name.
// Cloudflare's `name=` filter is literal (no trailing dot — it does not match
// "_acme-challenge.relay.x." against the record "_acme-challenge.relay.x"),
// so the fqdn's trailing dot is trimmed here or the purge silently lists 0.
func listTXTRecordIds(client *http.Client, base, token, apiKey, email, zoneID, name string) ([]string, error) {
	name = strings.TrimSuffix(name, ".")
	url := fmt.Sprintf("%s/zones/%s/dns_records?type=TXT&name=%s&per_page=100", base, zoneID, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-Auth-Key", apiKey)
		req.Header.Set("X-Auth-Email", email)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare purge list records: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare purge list records: HTTP %d: %s", resp.StatusCode, trunc(body))
	}
	var out struct {
		Success bool `json:"success"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, errors.New("cloudflare purge list records: API not success")
	}
	var ids []string
	for _, r := range out.Result {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// PurgeChallengeRecords is the exported wrapper: removes any pre-existing /
// leftover TXT challenge records for a host, so new placements never stack on
// stale ones (the redundant-record accumulation that bloats the zone and the
// ACME-order load).
func PurgeChallengeRecords(host, providerName string, env map[string]string) (int, error) {
	return purgeChallengeRecords(host, providerName, env)
}

// trunc bounds an API error body to a sane log size.
func trunc(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
