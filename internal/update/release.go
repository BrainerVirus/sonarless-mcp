// Ported from ytrack (github.com/BrainerVirus/youtrack-cli, internal/update/release.go).

package update

import (
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

const (
	// Repo is the GitHub repository that publishes sonarless-mcp releases.
	Repo = "BrainerVirus/sonarless-mcp"
	// DefaultAPIURL is the GitHub REST API root.
	DefaultAPIURL = "https://api.github.com"

	maxReleaseJSON = 1 << 20
)

// ErrReleaseNotFound is returned when the requested release does not exist.
var ErrReleaseNotFound = errors.New("release not found")

// Release is a published GitHub release.
type Release struct {
	Tag        string `json:"tag_name"`
	URL        string `json:"html_url"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	// PublishedAt is when the release was published; the background upgrade
	// waits SoakPeriod after it.
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	// APIURL is the asset's REST API URL: with Accept application/octet-stream
	// it serves the same bytes when the download CDN fails.
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// Version returns the tag without its leading "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// Stable reports whether r is neither a draft nor a prerelease.
func (r *Release) Stable() bool { return !r.Draft && !r.Prerelease }

// Asset returns the asset called name, or nil.
func (r *Release) Asset(name string) *Asset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// Client reads releases from the GitHub REST API without credentials.
type Client struct {
	HTTP      *http.Client
	APIURL    string // DefaultAPIURL when empty
	UserAgent string
}

// Latest returns the newest release that is neither a draft nor a prerelease.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.get(ctx, "releases/latest")
}

// Tag returns the release for tag (with or without the leading "v").
func (c *Client) Tag(ctx context.Context, tag string) (*Release, error) {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return c.get(ctx, "releases/tags/"+url.PathEscape(tag))
}

func (c *Client) get(ctx context.Context, path string) (*Release, error) {
	base := c.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	u := strings.TrimSuffix(base, "/") + "/repos/" + Repo + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrReleaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checking releases: GitHub answered HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("checking releases: %w", err)
	}
	if rel.Tag == "" {
		return nil, errors.New("checking releases: the answer has no tag")
	}
	return &rel, nil
}
