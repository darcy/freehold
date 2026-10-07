// Package vultr is the Vultr cloud provider: the API client that creates and
// destroys the world's HOST (a plain Debian instance) and the PVE-on-Debian
// install it needs before the Proxmox provider can drive it. Everything above
// the host is unchanged — the locked "VPS = Proxmox-on-Cloud-Compute,
// LXC-only" decision (the apt-route spike, both providers, live).
//
// The client is deliberately minimal: only the calls the lifecycle verbs need
// (ensure sshkey, create, poll, destroy). The agent-facing Vultr API door —
// the runner's connector — is a separate, already-shipped surface.
package vultr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOsID is Debian 12 x64 (the driver's historical default). The PVE
// install reads the codename off the host, so any Debian release works;
// --vultr-os-id overrides for a newer one.
const DefaultOsID = 1743

// DefaultBaseURL is the Vultr API v2 root. Overridable for tests.
const DefaultBaseURL = "https://api.vultr.com"

// Client is a minimal Vultr API v2 client.
type Client struct {
	Token string
	// BaseURL defaults to DefaultBaseURL; tests point it at an httptest server.
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// do performs one authenticated call and decodes the JSON body into out (nil
// skips decode). An unexpected status is an error carrying the body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vultr %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("vultr %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// EnsureSSHKey makes the door's public key an account SSH key (idempotent:
// an existing key with the same key body is reused). Returns the key id.
func (c *Client) EnsureSSHKey(ctx context.Context, name, pubkey string) (string, error) {
	var listed struct {
		SSHKeys []struct {
			ID  string `json:"id"`
			Key string `json:"ssh_key"`
		} `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/ssh-keys?per_page=100", nil, &listed); err != nil {
		return "", err
	}
	key := strings.TrimSpace(pubkey)
	for _, k := range listed.SSHKeys {
		if strings.TrimSpace(k.Key) == key {
			return k.ID, nil
		}
	}
	var created struct {
		SSHKey struct {
			ID string `json:"id"`
		} `json:"ssh_key"`
	}
	// The body field is `ssh_key` (the same name the list decode reads).
	if err := c.do(ctx, http.MethodPost, "/v2/ssh-keys", map[string]string{"name": name, "ssh_key": key}, &created); err != nil {
		return "", err
	}
	if created.SSHKey.ID == "" {
		return "", fmt.Errorf("vultr ssh-key create returned no id")
	}
	return created.SSHKey.ID, nil
}

// CreateInstance boots a Debian instance with the SSH key authorized at
// birth, so the door works before any interactive step. body carries the
// create fields (region/plan/os_id/label…; a 0/absent os_id defaults);
// returns the id.
func (c *Client) CreateInstance(ctx context.Context, body map[string]any, sshKeyID string) (string, error) {
	if v, ok := body["os_id"].(uint32); !ok || v == 0 {
		body["os_id"] = DefaultOsID
	}
	if sshKeyID != "" {
		body["sshkey_id"] = []string{sshKeyID}
	}
	var created struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/instances", body, &created); err != nil {
		return "", err
	}
	if created.Instance.ID == "" {
		return "", fmt.Errorf("vultr create returned no instance id")
	}
	return created.Instance.ID, nil
}

// Instance fetches one instance's status + main IP.
func (c *Client) Instance(ctx context.Context, id string) (status, ip string, err error) {
	var got struct {
		Instance struct {
			Status  string `json:"status"`
			MainIP  string `json:"main_ip"`
			PowerOn string `json:"power_status"`
		} `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/instances/"+id, nil, &got); err != nil {
		return "", "", err
	}
	return got.Instance.Status, got.Instance.MainIP, nil
}

// Regions lists the valid datacenter ids (lowercase: ewr, lax, …). The
// region ask is free-text; the create 400s ("Invalid datacenter.") on a
// continent group name — the API's own list is the authority.
func (c *Client) Regions(ctx context.Context) ([]string, error) {
	var got struct {
		Regions []struct {
			ID   string `json:"id"`
			City string `json:"city"`
		} `json:"regions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/regions?per_page=500", nil, &got); err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range got.Regions {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// Plans lists the valid plan ids (vc2-…, voc-…, …).
func (c *Client) Plans(ctx context.Context) ([]string, error) {
	var got struct {
		Plans []struct {
			ID string `json:"id"`
		} `json:"plans"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/plans?per_page=500", nil, &got); err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range got.Plans {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// DebianOsID picks the NEWEST Debian x64 image from the API's own OS
// catalog: the host is about to become PVE via the apt route, so the image
// must be Debian — and an os_id guess is exactly how an Ubuntu 22.04
// arrives with a Debian name on it (the catalog drifts). Sorted by the
// release number in the name ("Debian 12 x64").
func (c *Client) DebianOsID(ctx context.Context) (uint32, error) {
	var got struct {
		OS []struct {
			ID     uint32 `json:"id"`
			Name   string `json:"name"`
			Arch   string `json:"arch"`
			Family string `json:"family"`
		} `json:"os"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/os?per_page=500", nil, &got); err != nil {
		return 0, err
	}
	bestID, bestVer := uint32(0), 0
	for _, o := range got.OS {
		if !strings.EqualFold(o.Family, "debian") && !strings.Contains(strings.ToLower(o.Name), "debian") {
			continue
		}
		if o.Arch != "" && o.Arch != "x86_64" {
			continue
		}
		ver := 0
		if _, err := fmt.Sscanf(o.Name, "Debian %d", &ver); err != nil && ver == 0 {
			continue
		}
		if ver > bestVer {
			bestVer, bestID = ver, o.ID
		}
	}
	if bestID == 0 {
		return 0, fmt.Errorf("the Vultr OS catalog lists no Debian x64 image — pick one by id with --host-answer os_id=<id>")
	}
	return bestID, nil
}

// WaitActive polls until the instance is active AND its IPv4 is assigned
// (Vultr reports 0.0.0.0 until the IP lands). Returns the main IP.
func (c *Client) WaitActive(ctx context.Context, id string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, ip, err := c.Instance(ctx, id)
		if err == nil && status == "active" && ip != "" && ip != "0.0.0.0" {
			return ip, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("instance %s did not become active with an IP within %s (last: status=%q ip=%q)", id, timeout, status, ip)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Destroy deletes an instance (404 counts as destroyed — idempotent).
// A MISSED destroy keeps billing, so transient 409s retry.
func (c *Client) Destroy(ctx context.Context, id string) error {
	for attempt := 0; ; attempt++ {
		err := c.do(ctx, http.MethodDelete, "/v2/instances/"+id, nil, nil)
		if err == nil {
			return nil
		}
		// A 404-style answer is the API's way of saying it is already gone.
		// Any other error (409/412 included — a still-settling or locked
		// instance EXISTS and bills) retries, then fails loudly.
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil
		}
		if attempt >= 4 || ctx.Err() != nil {
			return fmt.Errorf("destroy of instance %s never confirmed — the instance may still be running and billing: %w", id, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
