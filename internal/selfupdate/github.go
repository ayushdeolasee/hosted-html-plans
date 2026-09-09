package selfupdate

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

const latestReleaseURL = "https://api.github.com/repos/ayushdeolasee/hosted-html-plans/releases/latest"

type githubSource struct {
	client *http.Client
}

func newGitHubSource() *githubSource {
	return &githubSource{client: &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if !trustedUpdateURL(req.URL) {
				return fmt.Errorf("refusing redirect to untrusted update URL %s", req.URL.Redacted())
			}
			return nil
		},
	}}
}

func allowedDownloadHost(host string) bool {
	switch strings.ToLower(host) {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	default:
		return false
	}
}

func trustedUpdateURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil &&
		(u.Port() == "" || u.Port() == "443") && allowedDownloadHost(u.Hostname())
}

func (g *githubSource) latest(ctx context.Context, assetName string) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dl, err := g.open(ctx, latestReleaseURL, maxReleaseMetadata)
	if err != nil {
		return release{}, err
	}
	defer dl.body.Close()
	body, err := io.ReadAll(io.LimitReader(dl.body, maxReleaseMetadata+1))
	if err != nil {
		return release{}, err
	}
	if len(body) > maxReleaseMetadata {
		return release{}, fmt.Errorf("release metadata exceeds %d bytes", maxReleaseMetadata)
	}

	var response struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return release{}, fmt.Errorf("decode release metadata: %w", err)
	}
	if response.Draft || response.Prerelease {
		return release{}, fmt.Errorf("GitHub latest release is not stable")
	}
	if _, err := parseStableVersion(response.TagName); err != nil {
		return release{}, err
	}

	rel := release{version: response.TagName, assetName: assetName}
	for _, asset := range response.Assets {
		switch asset.Name {
		case assetName:
			rel.assetURL, rel.assetSize = asset.URL, asset.Size
		case "checksums.txt":
			rel.checksumsURL = asset.URL
		}
	}
	if rel.assetURL == "" {
		return release{}, fmt.Errorf("release %s has no %s asset", rel.version, assetName)
	}
	if rel.checksumsURL == "" {
		return release{}, fmt.Errorf("release %s has no checksums.txt asset", rel.version)
	}
	if rel.assetSize <= 0 || rel.assetSize > maxBinary {
		return release{}, fmt.Errorf("release asset %s has invalid size %d", assetName, rel.assetSize)
	}
	return rel, nil
}

func (g *githubSource) open(ctx context.Context, rawURL string, limit int64) (download, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !trustedUpdateURL(u) {
		return download{}, fmt.Errorf("refusing untrusted update URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return download{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hosted-html-plans-self-update")
	resp, err := g.client.Do(req)
	if err != nil {
		return download{}, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if rawURL == latestReleaseURL && resp.StatusCode == http.StatusNotFound {
			return download{}, ErrNoRelease
		}
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return download{}, fmt.Errorf("GitHub returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if resp.ContentLength > limit {
		resp.Body.Close()
		return download{}, fmt.Errorf("response length %d exceeds %d bytes", resp.ContentLength, limit)
	}
	return download{body: resp.Body, contentLength: resp.ContentLength}, nil
}
