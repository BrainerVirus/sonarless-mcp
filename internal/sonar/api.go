// Package sonar manages the shared local SonarQube server: its container,
// readiness, admin API and the token sonarless uses for scans and MCP.
package sonar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the SonarQube Web API.
type Client struct {
	BaseURL string
	User    string // basic auth user, or a token with empty Pass
	Pass    string
	HTTP    *http.Client
}

// NewAdmin returns a client authenticated as the local admin.
func NewAdmin(baseURL, user, pass string) *Client {
	return &Client{BaseURL: baseURL, User: user, Pass: pass, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// NewToken returns a client authenticated with a user token.
func NewToken(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, User: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sonarqube API %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// Do performs a request; out (if non-nil) receives the decoded JSON body.
func (c *Client) Do(ctx context.Context, method, path string, params url.Values, out any) error {
	u := c.BaseURL + path
	var body io.Reader
	if method == http.MethodGet {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Pass)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Status returns /api/system/status (STARTING, UP, DB_MIGRATION_NEEDED, ...).
func (c *Client) Status(ctx context.Context) (string, error) {
	var r struct{ Status string }
	err := c.Do(ctx, http.MethodGet, "/api/system/status", nil, &r)
	return r.Status, err
}

// Valid reports whether the client's credentials authenticate.
func (c *Client) Valid(ctx context.Context) bool {
	var r struct{ Valid bool }
	return c.Do(ctx, http.MethodGet, "/api/authentication/validate", nil, &r) == nil && r.Valid
}

// MigrateDB triggers the database upgrade after a server version bump.
func (c *Client) MigrateDB(ctx context.Context) error {
	var r struct{ State, Message string }
	if err := c.Do(ctx, http.MethodPost, "/api/system/migrate_db", url.Values{}, &r); err != nil {
		return err
	}
	if r.State == "NOT_SUPPORTED" || r.State == "MIGRATION_FAILED" {
		return fmt.Errorf("%s: %s", r.State, r.Message)
	}
	return nil
}

// GenerateToken creates a user token with the given name.
func (c *Client) GenerateToken(ctx context.Context, name string) (string, error) {
	var r struct{ Token string }
	err := c.Do(ctx, http.MethodPost, "/api/user_tokens/generate", url.Values{"name": {name}}, &r)
	return r.Token, err
}

// RevokeToken revokes a token by name (missing is fine).
func (c *Client) RevokeToken(ctx context.Context, name string) error {
	err := c.Do(ctx, http.MethodPost, "/api/user_tokens/revoke", url.Values{"name": {name}}, nil)
	if e, ok := err.(*APIError); ok && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// EnsureProject creates the project if it doesn't exist.
func (c *Client) EnsureProject(ctx context.Context, key, name string) error {
	var r struct {
		Components []struct{ Key string }
	}
	if err := c.Do(ctx, http.MethodGet, "/api/projects/search", url.Values{"projects": {key}}, &r); err == nil && len(r.Components) > 0 {
		return nil
	}
	if name == "" {
		name = key
	}
	return c.Do(ctx, http.MethodPost, "/api/projects/create", url.Values{"project": {key}, "name": {name}}, nil)
}

// QualityGate returns the project's quality gate status (OK, ERROR, NONE).
func (c *Client) QualityGate(ctx context.Context, key string) (string, error) {
	var r struct {
		ProjectStatus struct{ Status string }
	}
	err := c.Do(ctx, http.MethodGet, "/api/qualitygates/project_status", url.Values{"projectKey": {key}}, &r)
	return r.ProjectStatus.Status, err
}

// PendingTasks returns the number of queued or running analysis reports.
func (c *Client) PendingTasks(ctx context.Context, key string) (int, error) {
	var q struct {
		Queue []json.RawMessage `json:"queue"`
	}
	err := c.Do(ctx, http.MethodGet, "/api/ce/component", url.Values{"component": {key}}, &q)
	return len(q.Queue), err
}

// Measures returns raw JSON for the project's key metrics.
func (c *Client) Measures(ctx context.Context, key string, metrics []string) (json.RawMessage, error) {
	var r json.RawMessage
	err := c.Do(ctx, http.MethodGet, "/api/measures/component",
		url.Values{"component": {key}, "metricKeys": {strings.Join(metrics, ",")}}, &r)
	return r, err
}
