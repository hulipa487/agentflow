package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// vultrBase is the Vultr API v2 root.
const vultrBase = "https://api.vultr.com/v2"

// vultrClient is a thin hand-rolled client over the handful of Vultr v2
// endpoints the provider needs: register/list/delete SSH keys, create/get/list/
// delete instances. No OpenAPI codegen — just net/http + encoding/json, in
// keeping with the rest of the repo.
type vultrClient struct {
	apiKey  string
	httpc   *http.Client
	baseURL string // overridable for tests
}

func newVultrClient(apiKey string, baseURL string) *vultrClient {
	if baseURL == "" {
		baseURL = vultrBase
	}
	return &vultrClient{
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: 60 * time.Second},
		baseURL: baseURL,
	}
}

// --- SSH keys ---

type sshKeyCreateReq struct {
	Name   string `json:"name"`
	SSHKey string `json:"ssh_key"`
}

type sshKeyResp struct {
	SSHKey struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		SSHKey     string `json:"ssh_key"`
		DateCreated string `json:"date_created"`
	} `json:"ssh_key"`
}

func (c *vultrClient) CreateSSHKey(ctx context.Context, name, pubKey string) (string, error) {
	body, err := json.Marshal(sshKeyCreateReq{Name: name, SSHKey: pubKey})
	if err != nil {
		return "", err
	}
	var r sshKeyResp
	if err := c.do(ctx, http.MethodPost, "/ssh-keys", nil, body, &r, 201); err != nil {
		return "", err
	}
	return r.SSHKey.ID, nil
}

func (c *vultrClient) DeleteSSHKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/ssh-keys/"+id, nil, nil, nil, 204)
}

// --- Instances ---

type instanceCreateReq struct {
	Region   string   `json:"region"`
	Plan     string   `json:"plan"`
	OsID     int      `json:"os_id,omitempty"`
	Label    string   `json:"label,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	SSHKeyID []string `json:"sshkey_id,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

type instanceResp struct {
	Instance vultrInstance `json:"instance"`
}

type instanceListResp struct {
	Instances []vultrInstance `json:"instances"`
}

// vultrInstance carries only the fields the provider reads. main_ip and the
// status triad drive the ready-check; user_scheme selects the SSH login user.
type vultrInstance struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	MainIP       string `json:"main_ip"`
	Status       string `json:"status"`        // active | pending | suspended | resizing
	PowerStatus  string `json:"power_status"`  // running | stopped
	ServerStatus string `json:"server_status"` // none | locked | installingbooting | ok
	UserScheme   string `json:"user_scheme"`   // root | limited
	Region       string `json:"region"`
	OsID        int    `json:"os_id"`
}

func (c *vultrClient) CreateInstance(ctx context.Context, req instanceCreateReq) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	var r instanceResp
	if err := c.do(ctx, http.MethodPost, "/instances", nil, body, &r, 202); err != nil {
		return "", err
	}
	return r.Instance.ID, nil
}

func (c *vultrClient) GetInstance(ctx context.Context, id string) (vultrInstance, error) {
	var r instanceResp
	if err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, nil, &r, 200); err != nil {
		return vultrInstance{}, err
	}
	return r.Instance, nil
}

// ListInstancesByLabel returns instances whose label matches l. Used to find an
// already-running instance (crash recovery); not on the v1 happy path.
func (c *vultrClient) ListInstancesByLabel(ctx context.Context, label string) ([]vultrInstance, error) {
	q := url.Values{}
	if label != "" {
		q.Set("label", label)
	}
	var r instanceListResp
	if err := c.do(ctx, http.MethodGet, "/instances", q, nil, &r, 200); err != nil {
		return nil, err
	}
	return r.Instances, nil
}

func (c *vultrClient) DeleteInstance(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+id, nil, nil, nil, 204)
}

// --- request plumbing ---

// do issues a request and, if wantCode matches the response status, decodes
// the JSON body into out (when out is non-nil). DELETE returns 204 with no
// body, hence out may be nil. It retries on 429 using Retry-After.
func (c *vultrClient) do(ctx context.Context, method, path string, q url.Values, body []byte, out any, wantCode int) error {
	u := c.baseURL + path
	if q != nil {
		u += "?" + q.Encode()
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = err
			if attempt >= 2 {
				return fmt.Errorf("vultr %s %s: %w", method, path, lastErr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(500*(attempt+1)) * time.Millisecond):
			}
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			ra := parseRetryAfter(resp.Header.Get("Retry-After"), 1*time.Second)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ra):
			}
			continue
		}

		if resp.StatusCode != wantCode {
			return fmt.Errorf("vultr %s %s: status %d (want %d): %s",
				method, path, resp.StatusCode, wantCode, strings.TrimSpace(string(respBody)))
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("vultr %s %s: decode: %w", method, path, err)
			}
		}
		return nil
	}
}

// parseRetryAfter parses the Retry-After header as seconds, falling back to d.
func parseRetryAfter(h string, dflt time.Duration) time.Duration {
	if h == "" {
		return dflt
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return dflt
}

// isReady reports whether a Vultr instance is booted, running, and has a real
// public IP — the precondition for SSH.
func (i vultrInstance) isReady() bool {
	return i.Status == "active" &&
		i.PowerStatus == "running" &&
		i.ServerStatus == "ok" &&
		i.MainIP != "" &&
		i.MainIP != "0.0.0.0"
}
