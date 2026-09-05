package dnsman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cloudflare's DNS-01 challenge provider env carries the API token under
// CLOUDFLARE_DNS_API_TOKEN (the primary, zone-scoped token) or the legacy
// CLOUDFLARE_API_KEY+CLOUDFLARE_EMAIL pair. The zone is resolved from the
// token's visible zones by longest suffix match, so DNS-01 records under any
// host (e.g. _acme-challenge... and the relay/cp A records) land in the apex
// zone Cloudflare serves for this account.
const (
	EnvToken   = "CLOUDFLARE_DNS_API_TOKEN"
	EnvBaseURL = "CLOUDFLARE_BASE_URL"
	defaultURL = "https://api.cloudflare.com/client/v4"
)

func init() {
	Register("cloudflare", NewCloudflare)
}

// cloudflareManager manages A records via the Cloudflare REST API (net/http —
// no SDK dependency; two endpoints suffice).
type cloudflareManager struct {
	client *http.Client
	token  string
	base   string
}

// NewCloudflare builds the Cloudflare manager from the provider env map. The
// bearer token is required.
func NewCloudflare(env map[string]string) (Manager, error) {
	token := env[EnvToken]
	if token == "" {
		return nil, errors.New("cloudflare DNS management needs " + EnvToken)
	}
	base := env[EnvBaseURL]
	if base == "" {
		base = defaultURL
	}
	return &cloudflareManager{
		client: &http.Client{Timeout: 30 * time.Second},
		token:  token,
		base:   strings.TrimSuffix(base, "/"),
	}, nil
}

func (c *cloudflareManager) Provider() string { return "cloudflare" }

// ---- zone resolution ---------------------------------------------------------

type cfZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type cfListZones struct {
	Result []cfZone `json:"result"`
	Info   cfInfo   `json:"result_info"`
}

type cfInfo struct {
	TotalPages int `json:"total_pages"`
}

// zoneFor returns the zone whose name is the longest suffix of host (so
// records under the apex `freehold.technology` are matched by that zone, never
// an unrelated subdomain), scanning every page of zones the token can see.
func (c *cloudflareManager) zoneFor(ctx context.Context, host string) (cfZone, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	page := 1
	for {
		var out cfListZones
		if err := c.api(ctx, "GET", "/zones?per_page=100&page="+fmt.Sprint(page), nil, &out); err != nil {
			return cfZone{}, err
		}
		if z, ok := matchZone(host, out.Result); ok {
			return z, nil
		}
		if page >= out.Info.TotalPages || out.Info.TotalPages == 0 {
			break
		}
		page++
	}
	return cfZone{}, fmt.Errorf("no Cloudflare zone serves %q (token cannot see an apex zone for it — is the zone added and active?)", host)
}

// matchZone picks the zone whose name is the longest suffix of host (a host
// under the apex `example.com` matches that zone, not an unrelated sibling).
func matchZone(host string, zones []cfZone) (cfZone, bool) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	var best *cfZone
	for _, z := range zones {
		name := strings.ToLower(z.Name)
		if name == host || strings.HasSuffix(host, "."+name) {
			if best == nil || len(name) > len(best.Name) {
				z := z
				best = &z
			}
		}
	}
	if best == nil {
		return cfZone{}, false
	}
	return *best, true
}

// ---- record CRUD --------------------------------------------------------------

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type cfListRecords struct {
	Result []cfRecord `json:"result"`
}

type cfMutation struct {
	Result cfRecord `json:"result"`
}

// rawRecord is the API's on-disk shape (has other fields we ignore).
type rawRecord map[string]any

// recordsByName lists all A records named host (exact) in the zone.
func (c *cloudflareManager) listRecords(ctx context.Context, zone, name, typ string) ([]cfRecord, error) {
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", name)
	var out struct {
		Result []rawRecord `json:"result"`
	}
	if err := c.api(ctx, "GET", "/zones/"+zone+"/dns_records?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	recs := make([]cfRecord, 0, len(out.Result))
	for _, r := range out.Result {
		var rec cfRecord
		if id, _ := r["id"].(string); id != "" {
			rec.ID = id
		}
		if content, _ := r["content"].(string); content != "" {
			rec.Content = content
		}
		if proxied, _ := r["proxied"].(bool); proxied {
			rec.Proxied = true
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func (c *cloudflareManager) UpsertA(name, ip string) error {
	ctx := context.Background()
	zone, err := c.zoneFor(ctx, name)
	if err != nil {
		return err
	}
	name = strings.TrimSuffix(name, ".")
	// A DNS-only (unproxied) A so the operator's edge (freehold's Caddy)
	// serves its own LE wildcard, rather than the CDN edge.
	body := cfRecord{Type: "A", Name: name, Content: ip, TTL: 1, Proxied: false}
	recs, err := c.listRecords(ctx, zone.ID, name, "A")
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		var out cfMutation
		if err := c.api(ctx, "POST", "/zones/"+zone.ID+"/dns_records", body, &out); err != nil {
			return err
		}
		fmt.Printf("  ✓ Cloudflare A %s -> %s (new)\n", name, ip)
		return nil
	}
	// Update the FIRST match if it differs (dedupe: remove the rest).
	var mutated bool
	for i, r := range recs {
		if r.Content != ip || r.Proxied {
			if err := c.api(ctx, "PUT", fmt.Sprintf("/zones/%s/dns_records/%s", zone.ID, r.ID), body, &cfMutation{}); err != nil {
				return err
			}
			mutated = true
		}
		if i > 0 {
			_ = c.api(ctx, "DELETE", fmt.Sprintf("/zones/%s/dns_records/%s", zone.ID, r.ID), nil, nil)
		}
	}
	if !mutated {
		fmt.Printf("  ✓ Cloudflare A %s already -> %s\n", name, ip)
	} else {
		fmt.Printf("  ✓ Cloudflare A %s -> %s (updated)\n", name, ip)
	}
	return nil
}

func (c *cloudflareManager) DeleteA(name string) error {
	return c.deleteRecords(name, "A")
}

// DeleteTXT removes every TXT record at name (clears a leftover DNS-01 challenge
// record ahead of issuing a fresh one).
func (c *cloudflareManager) DeleteTXT(name string) error {
	return c.deleteRecords(name, "TXT")
}

// deleteRecords deletes all records of type typ at name across the matching zone.
func (c *cloudflareManager) deleteRecords(name, typ string) error {
	ctx := context.Background()
	zone, err := c.zoneFor(ctx, name)
	if err != nil {
		return err
	}
	recs, err := c.listRecords(ctx, zone.ID, strings.TrimSuffix(name, "."), typ)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := c.api(ctx, "DELETE", fmt.Sprintf("/zones/%s/dns_records/%s", zone.ID, r.ID), nil, nil); err != nil {
			return err
		}
		fmt.Printf("  ✓ Cloudflare %s %s removed\n", typ, name)
	}
	return nil
}

// ---- transport ----------------------------------------------------------------

type cfAPIError struct {
	Code   int               `json:"code"`
	Msg    string            `json:"message"`
	Errors []cfAPIError      `json:"errors"`
	Raw    map[string]string `json:"-"`
}

func (e *cfAPIError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Errors) > 0 {
		parts := make([]string, 0, len(e.Errors))
		for _, er := range e.Errors {
			parts = append(parts, fmt.Sprintf("%d: %s", er.Code, er.Msg))
		}
		return strings.Join(parts, "; ")
	}
	return fmt.Sprintf("%d: %s", e.Code, e.Msg)
}

type cfEnvelope struct {
	Success bool   `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// api performs one Cloudflare request with the bearer token, unmarshalling a
// generic success/envelope check + optionally the result body.
func (c *cloudflareManager) api(ctx context.Context, method, path string, in, out any) error {
	var reqBody io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		var env cfEnvelope
		if err := json.Unmarshal(raw, &env); err == nil && !env.Success {
			if len(env.Errors) > 0 {
				parts := make([]string, 0, len(env.Errors))
				for _, e := range env.Errors {
					parts = append(parts, fmt.Sprintf("%d: %s", e.Code, e.Message))
				}
				return fmt.Errorf("cloudflare %s %s: %s", method, path, strings.Join(parts, "; "))
			}
			return fmt.Errorf("cloudflare %s %s: request failed", method, path)
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("cloudflare %s %s: parse: %w", method, path, err)
			}
		}
	}
	return nil
}
