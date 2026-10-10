// Package pangolin drives a Pangolin instance's integration API — the write
// path for apps exposed through a Pangolin VPS (the public path where the
// home IP appears in no public DNS and nothing at home accepts unsolicited
// traffic). Verified against Pangolin 1.24.0: the integration server mounts
// the REST surface at /v1 with a Bearer key (`apiKeyId.secret`, a root key
// granted the actions per call), while the dashboard's session API lives on
// the external port and the internal API serves the generated Traefik
// config. Pangolin generates its Traefik config from its own database (an
// HTTP provider), so resources/targets are created through the REST API —
// never by writing Traefik config.
package pangolin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client talks to one Pangolin integration API. Key = the full root key
// (`apiKeyId.secret`); Base = the integration server origin
// (http://<vps>:<integration_port>, default 3003).
type Client struct {
	Base string
	Key  string
	HTTP *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Success bool            `json:"success"`
		Error   bool            `json:"error"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("pangolin: %s %s: HTTP %d: %s", method, path, res.StatusCode, truncate(raw))
	}
	if !env.Success || res.StatusCode >= 400 {
		return fmt.Errorf("pangolin: %s %s: HTTP %d: %s", method, path, res.StatusCode, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// Site is the world's tunnel site: the Newt identity the gateway dials out
// with. The secret is shown ONCE at creation — the caller stores it (a CP
// secret, injected into the gateway's Newt stage).
type Site struct {
	SiteID     int    `json:"siteId"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	NewtID     string `json:"newtId"`
	NewtSecret string `json:"newtSecret"`
	Subnet     string `json:"address"`
	Online     bool   `json:"online"`
}

// Resource is one exposed app's public surface: the fqdn Pangolin routes
// (fullDomain) and the auth posture (sso/pincode/whitelist ride the
// resource's policy — the member gate decision lives there).
type Resource struct {
	ResourceID int    `json:"resourceId"`
	Name       string `json:"name"`
	Subdomain  string `json:"subdomain"`
	FullDomain string `json:"fullDomain"`
	DomainID   string `json:"domainId"`
	SSL        bool   `json:"ssl"`
	Enabled    bool   `json:"enabled"`
}

// Domain is one base domain Pangolin serves (the config-declared namespace):
// resources attach to it by DomainID, and its cert resolver decides where
// the per-host certificates come from.
type Domain struct {
	DomainID   string `json:"domainId"`
	BaseDomain string `json:"baseDomain"`
	Verified   bool   `json:"verified"`
	Type       string `json:"type"`
}

// ListDomains returns the base domains the instance serves.
func (c *Client) ListDomains(ctx context.Context, orgID string) ([]Domain, error) {
	var out struct {
		Domains []Domain `json:"domains"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/org/"+orgID+"/domains", nil, &out); err != nil {
		return nil, err
	}
	return out.Domains, nil
}

// CreateOrg registers the world's organization. The subnets are Pangolin's
// WireGuard space (the site tunnel addresses come out of it) — freehold's
// subnet is independent; pick Pangolin space that does not collide
// (100.90.0.0/16 + a /24 utility slice is fine).
func (c *Client) CreateOrg(ctx context.Context, orgID, name, subnet, utilitySubnet string) error {
	return c.do(ctx, http.MethodPut, "/v1/org", map[string]string{
		"orgId": orgID, "name": name, "subnet": subnet, "utilitySubnet": utilitySubnet,
	}, nil)
}

// CreateSite mints the world's Newt site (the gateway dials out with this
// identity). The secret rides the response only — store it immediately.
func (c *Client) CreateSite(ctx context.Context, orgID, name string) (Site, error) {
	var s Site
	if err := c.do(ctx, http.MethodPut, "/v1/org/"+orgID+"/site", map[string]interface{}{
		"name": name, "type": "newt",
	}, &s); err != nil {
		return Site{}, err
	}
	return s, nil
}

// CreateResource exposes one app: subdomain + the base domain form the
// fqdn Pangolin routes and certifies (per-host HTTP-01 through its Traefik).
func (c *Client) CreateResource(ctx context.Context, orgID string, siteID int, name, subdomain, domainID string) (Resource, error) {
	var r Resource
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/org/%s/site/%d/resource", orgID, siteID), map[string]interface{}{
		"name": name, "subdomain": subdomain, "mode": "http", "domainId": domainID,
	}, &r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// SetTarget points the resource at the app's internal address, reachable
// from the gateway (where Newt dials out): freehold-subnet IP + the app's
// port (a NodePort, a guest's port). Method "http" — Pangolin terminates
// TLS and proxies plain HTTP to it.
func (c *Client) SetTarget(ctx context.Context, resourceID, siteID int, ip string, port int) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/resource/%d/target", resourceID), map[string]interface{}{
		"siteId": siteID, "ip": ip, "port": port, "method": "http",
	}, nil)
}

// DeleteResource removes one app's public surface.
func (c *Client) DeleteResource(ctx context.Context, resourceID int) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/v1/resource/%d", resourceID), nil, nil)
}
