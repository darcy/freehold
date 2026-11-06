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
	NewtSecret string `json:"secret"` // the one-time Newt secret: Pangolin names it "secret" in the create response
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
// The resource is created SSO-GATED (its `sso` flag set right after create):
// nothing it fronts is publicly open by default — reaching the app means
// logging into the instance, until the freehold member gate's exact shape
// on this path lands (docs/NETWORK.md). The tunnel itself is proven with a
// resource access token (MintAccessToken), which badger accepts in place
// of a login.
func (c *Client) CreateResource(ctx context.Context, orgID string, siteID int, name, subdomain, domainID string) (Resource, error) {
	var r Resource
	if err := c.do(ctx, http.MethodPut, "/v1/org/"+orgID+"/resource", map[string]interface{}{
		"name": name, "subdomain": subdomain, "mode": "http", "domainId": domainID,
	}, &r); err != nil {
		return Resource{}, err
	}
	if err := c.requireSSO(ctx, r.ResourceID); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// requireSSO sets the resource's sso flag — the gate that keeps nothing it
// fronts publicly open by default.
func (c *Client) requireSSO(ctx context.Context, resourceID int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/resource/%d", resourceID), map[string]interface{}{"sso": true}, nil)
}

// MintAccessToken mints a resource access token — the credential badger
// accepts in place of a Pangolin login (the `p_token` query param by
// default). The tunnel-verification path.
func (c *Client) MintAccessToken(ctx context.Context, resourceID int, title string, validForSeconds int) (string, error) {
	var out struct {
		AccessToken string `json:"accessToken"` // Pangolin names it "accessToken" in the create response
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/resource/%d/access-token", resourceID), map[string]interface{}{
		"title": title, "validForSeconds": validForSeconds,
	}, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("pangolin: the access token came back empty")
	}
	return out.AccessToken, nil
}

// SetTarget points the resource at the app's internal address, reachable
// from the gateway (where Newt dials out): freehold-subnet IP + the app's
// port. method is "http" (Pangolin terminates TLS, proxies plain HTTP) or
// "https" (TLS to the home edge — the member-gated path; the resource's
// tlsServerName carries the SNI).
func (c *Client) SetTarget(ctx context.Context, resourceID, siteID int, ip string, port int, method string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/resource/%d/target", resourceID), map[string]interface{}{
		"siteId": siteID, "ip": ip, "port": port, "method": method,
	}, nil)
}

// DeleteResource removes one app's public surface.
func (c *Client) DeleteResource(ctx context.Context, resourceID int) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/v1/resource/%d", resourceID), nil, nil)
}

// ---- idempotent ensure helpers (the expose path runs on every re-apply) ----

// ListSites returns the org's tunnel sites.
func (c *Client) ListSites(ctx context.Context, orgID string) ([]Site, error) {
	var out struct {
		Sites []Site `json:"sites"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/org/"+orgID+"/sites", nil, &out); err != nil {
		return nil, err
	}
	return out.Sites, nil
}

// EnsureSite finds the org's site by name, creating it (a Newt site — the
// gateway's dial-out identity) when absent. The Newt secret is only ever
// returned at creation; the caller must persist it then.
func (c *Client) EnsureSite(ctx context.Context, orgID, name string) (Site, bool, error) {
	sites, err := c.ListSites(ctx, orgID)
	if err != nil {
		return Site{}, false, err
	}
	for _, s := range sites {
		if s.Name == name {
			return s, false, nil
		}
	}
	s, err := c.CreateSite(ctx, orgID, name)
	return s, true, err
}

// ListResources returns the org's resources.
func (c *Client) ListResources(ctx context.Context, orgID string) ([]Resource, error) {
	var out struct {
		Resources []Resource `json:"resources"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/org/"+orgID+"/resources", nil, &out); err != nil {
		return nil, err
	}
	return out.Resources, nil
}

// EnsureDomain resolves the base domain's id (the config-declared namespace
// Pangolin serves), refusing an unverified one — a vhost on it would 500.
func (c *Client) EnsureDomain(ctx context.Context, orgID, baseDomain string) (string, error) {
	domains, err := c.ListDomains(ctx, orgID)
	if err != nil {
		return "", err
	}
	for _, d := range domains {
		if d.BaseDomain == baseDomain {
			if !d.Verified {
				return "", fmt.Errorf("pangolin: base domain %s is not verified on the instance", baseDomain)
			}
			return d.DomainID, nil
		}
	}
	return "", fmt.Errorf("pangolin: base domain %s is not configured on the instance", baseDomain)
}

// EnsureResource finds the resource whose fullDomain is
// subdomain.<the base domain>, creating it (mode http, on the site) when
// absent. Returns the resource and whether it was created now.
func (c *Client) EnsureResource(ctx context.Context, orgID string, siteID int, subdomain, domainID, baseDomain string) (Resource, bool, error) {
	resources, err := c.ListResources(ctx, orgID)
	if err != nil {
		return Resource{}, false, err
	}
	want := subdomain + "." + baseDomain
	for _, r := range resources {
		if r.FullDomain == want {
			// Re-assert the SSO gate idempotently: a create whose gate
			// POST failed must not stay publicly open on later ensures.
			return r, false, c.requireSSO(ctx, r.ResourceID)
		}
	}
	r, err := c.CreateResource(ctx, orgID, siteID, subdomain, subdomain, domainID)
	return r, true, err
}

// SetTargetIfDiffers points the resource at ip:port:method, skipping the
// write when the target already matches (re-apply convergence, no churn).
func (c *Client) SetTargetIfDiffers(ctx context.Context, resourceID, siteID int, ip string, port int, method string) error {
	var out struct {
		Targets []struct {
			SiteID int    `json:"siteId"`
			IP     string `json:"ip"`
			Port   int    `json:"port"`
			Method string `json:"method"`
		} `json:"targets"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/resource/%d/targets", resourceID), nil, &out); err != nil {
		return err
	}
	for _, t := range out.Targets {
		if t.SiteID == siteID && t.IP == ip && t.Port == port && t.Method == method {
			return nil
		}
	}
	return c.SetTarget(ctx, resourceID, siteID, ip, port, method)
}

// ---- the public path's SSO + per-app policy (docs/NETWORK.md, model D) ----

// IdP is one identity provider (the global OIDC IdP the world's CP provides).
type IdP struct {
	IDPID   int    `json:"idpId"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Variant string `json:"variant"`
}

// ListIDPs returns the instance's global identity providers.
func (c *Client) ListIDPs(ctx context.Context) ([]IdP, error) {
	var out struct {
		IDPs []IdP `json:"idps"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/idp", nil, &out); err != nil {
		return nil, err
	}
	return out.IDPs, nil
}

// CreateOIDCIDP registers the world's CP as a global OIDC identity provider.
// Returns the idp id + Pangolin's callback (the redirect URI our authorize
// endpoint must accept). Pangolin decrypts the client secret for every login
// and token exchange; the CP seals the same pair.
func (c *Client) CreateOIDCIDP(ctx context.Context, name, clientID, clientSecret, authURL, tokenURL string) (int, string, error) {
	var out struct {
		IDPID       int    `json:"idpId"`
		RedirectURL string `json:"redirectUrl"`
	}
	if err := c.do(ctx, http.MethodPut, "/v1/idp/oidc", map[string]interface{}{
		"name": name, "clientId": clientID, "clientSecret": clientSecret,
		"authUrl": authURL, "tokenUrl": tokenURL,
		"identifierPath": "sub", "namePath": "name",
		"scopes": "openid profile email", "autoProvision": true, "variant": "oidc",
	}, &out); err != nil {
		return 0, "", err
	}
	if out.IDPID == 0 {
		return 0, "", fmt.Errorf("pangolin: the OIDC IdP create returned no idpId")
	}
	return out.IDPID, out.RedirectURL, nil
}

// EnsureOIDCIDP finds the global OIDC IdP by name, creating it when absent.
// created reports a fresh create (its callback must be sealed); an existing
// IdP returns "" for the callback (the CP already sealed it).
func (c *Client) EnsureOIDCIDP(ctx context.Context, name, clientID, clientSecret, authURL, tokenURL string) (idpID int, redirectURL string, created bool, err error) {
	idps, ierr := c.ListIDPs(ctx)
	if ierr != nil {
		return 0, "", false, ierr
	}
	for _, i := range idps {
		if i.Name == name && i.Type == "oidc" {
			return i.IDPID, "", false, nil
		}
	}
	id, redirect, cerr := c.CreateOIDCIDP(ctx, name, clientID, clientSecret, authURL, tokenURL)
	return id, redirect, true, cerr
}

// AssignIDPOrg links a global IdP to the org with its auto-provision role
// mapping (a JMESPath expression; "'family'" maps every admitted key to the
// family role). Idempotent (PUT).
func (c *Client) AssignIDPOrg(ctx context.Context, idpID int, orgID, roleMapping string) error {
	path := fmt.Sprintf("/v1/idp/%d/org/%s", idpID, orgID)
	body := map[string]string{"roleMapping": roleMapping}
	err := c.do(ctx, http.MethodPut, path, body, nil)
	if err == nil {
		return nil
	}
	// PUT is create-only: an existing policy is updated with POST (the ensure
	// runs every build).
	if !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Role is one org role (the access unit for resources).
type Role struct {
	RoleID int    `json:"roleId"`
	Name   string `json:"name"`
}

// ListRoles returns the org's roles.
func (c *Client) ListRoles(ctx context.Context, orgID string) ([]Role, error) {
	var out struct {
		Roles []Role `json:"roles"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/org/"+orgID+"/roles", nil, &out); err != nil {
		return nil, err
	}
	return out.Roles, nil
}

// EnsureRole finds the org role by name, creating it when absent.
func (c *Client) EnsureRole(ctx context.Context, orgID, name string) (int, error) {
	roles, err := c.ListRoles(ctx, orgID)
	if err != nil {
		return 0, err
	}
	for _, r := range roles {
		if r.Name == name {
			return r.RoleID, nil
		}
	}
	var out Role
	if err := c.do(ctx, http.MethodPut, "/v1/org/"+orgID+"/role", map[string]interface{}{"name": name}, &out); err != nil {
		return 0, err
	}
	if out.RoleID == 0 {
		return 0, fmt.Errorf("pangolin: the role create returned no roleId")
	}
	return out.RoleID, nil
}

// SetAccessControl sets a resource's auth posture: SSO on/off, the roles
// allowed, and (SSO) the IdP to skip straight to. Idempotent.
func (c *Client) SetAccessControl(ctx context.Context, resourceID int, sso bool, roleIDs []int, skipToIDPID int) error {
	if roleIDs == nil {
		roleIDs = []int{} // the API requires an array, never null
	}
	body := map[string]interface{}{"sso": sso, "userIds": []string{}, "roleIds": roleIDs}
	if skipToIDPID != 0 {
		body["skipToIdpId"] = skipToIDPID
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/resource-policy/%d/access-control", resourceID), body, nil)
}

// SetTLSName sets a resource's upstream TLS SNI (the home edge's vhost name) —
// the tunnel target is https to the edge, which serves the per-app vhost by
// SNI/Host.
func (c *Client) SetTLSName(ctx context.Context, resourceID int, name string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/resource/%d", resourceID), map[string]interface{}{
		"tlsServerName": name,
	}, nil)
}

// Rule is one access rule on a resource (ACCEPT bypasses auth for matching
// requests; PASS forces auth; DROP rejects).
type Rule struct {
	Action   string `json:"action"`
	Match    string `json:"match"`
	Value    string `json:"value"`
	Priority int    `json:"priority"`
}

// ListRules returns a resource's rules.
func (c *Client) ListRules(ctx context.Context, resourceID int) ([]Rule, error) {
	var out struct {
		Rules []Rule `json:"rules"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/public-resource/%d/rules", resourceID), nil, &out); err != nil {
		return nil, err
	}
	return out.Rules, nil
}

// EnsureAcceptRule creates an ACCEPT rule (bypass auth) for one path pattern
// when no matching rule exists. The exempt paths are the app's own-auth
// surfaces (navidrome /rest, browns /api) — native clients cannot do SSO.
func (c *Client) EnsureAcceptRule(ctx context.Context, resourceID int, path string, priority int) error {
	rules, err := c.ListRules(ctx, resourceID)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Action == "ACCEPT" && r.Match == "PATH" && r.Value == path {
			return nil
		}
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/public-resource/%d/rule", resourceID), map[string]interface{}{
		"action": "ACCEPT", "match": "PATH", "value": path, "priority": priority, "enabled": true,
	}, nil)
}
