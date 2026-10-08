package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type GitHubNativeLink struct {
	ID                  string    `json:"id"`
	State               string    `json:"state"`
	BrowserURL          string    `json:"browser_url,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
	PollIntervalSeconds int       `json:"poll_interval_seconds,omitempty"`
}

func (c *Client) StartGitHubNativeLink(ctx context.Context) (GitHubNativeLink, error) {
	var out GitHubNativeLink
	err := c.do(ctx, http.MethodPost, "/v1/github/native-links", map[string]any{}, &out)
	if err != nil {
		return out, err
	}
	origin, baseErr := url.Parse(c.baseURL)
	browser, parseErr := url.Parse(out.BrowserURL)
	if baseErr != nil || parseErr != nil || out.ID == "" || out.State != "pending" || out.ExpiresAt.IsZero() || browser.Scheme != origin.Scheme || browser.Host != origin.Host || browser.User != nil || browser.Fragment != "" || browser.Path != "/v1/github/native-links/"+out.ID+"/launch" {
		return out, errors.New("control plane returned an invalid GitHub connection handoff")
	}
	return out, nil
}

func (c *Client) GitHubNativeLinkStatus(ctx context.Context, id string) (GitHubNativeLink, error) {
	var out GitHubNativeLink
	err := c.do(ctx, http.MethodGet, "/v1/github/native-links/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) CancelGitHubNativeLink(ctx context.Context, id string) (GitHubNativeLink, error) {
	var out GitHubNativeLink
	err := c.do(ctx, http.MethodDelete, "/v1/github/native-links/"+url.PathEscape(id), nil, &out)
	return out, err
}
