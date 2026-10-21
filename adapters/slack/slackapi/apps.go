package slackapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateAppRequest is the request of apps.manifest.create.
type CreateAppRequest struct {
	// Manifest is the manifest of the new app.
	Manifest Manifest
}

// CreateAppResponse is the response of apps.manifest.create.
type CreateAppResponse struct {
	// AppID is the ID of the new app.
	AppID string `json:"app_id"`
	// Credentials are the credentials of the new app.
	Credentials AppCredentials `json:"credentials"`
}

// AppCredentials are the credentials of an app that apps.manifest.create
// returns.
type AppCredentials struct {
	// ClientID is the OAuth client ID.
	ClientID string `json:"client_id"`
	// ClientSecret is the OAuth client secret.
	ClientSecret string `json:"client_secret"`
	// SigningSecret is the secret that VerifyRequest checks the requests from
	// Slack with.
	SigningSecret string `json:"signing_secret"`
}

// DeleteAppRequest is the request of apps.manifest.delete.
type DeleteAppRequest struct {
	// AppID is the ID of the app.
	AppID string
}

// CreateApp calls apps.manifest.create to create an app from req.Manifest.
// Call it on a Client with an app configuration token (xoxe.xoxp-):
// api.WithToken(configToken).CreateApp(ctx, req). The token belongs to one
// user and one workspace, and expires after 12 hours. CreateApp sends the
// manifest as a JSON string in a form-encoded body. For an invalid manifest,
// Slack returns the error code "invalid_manifest", and APIError.Raw holds the
// "errors" list of the response.
func (c *Client) CreateApp(ctx context.Context, req CreateAppRequest) (*CreateAppResponse, error) {
	const method = "apps.manifest.create"
	manifest, err := json.Marshal(req.Manifest)
	if err != nil {
		return nil, fmt.Errorf("slack: encode %s request: %w", method, err)
	}
	var resp CreateAppResponse
	if err := c.Call(ctx, method, url.Values{"manifest": {string(manifest)}}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteApp calls apps.manifest.delete to delete an app that was created from
// a manifest. Call it on a Client with an app configuration token, as for
// CreateApp. It sends a form-encoded body.
func (c *Client) DeleteApp(ctx context.Context, req DeleteAppRequest) error {
	values := url.Values{}
	setFormString(values, "app_id", req.AppID)
	return c.Call(ctx, "apps.manifest.delete", values, nil)
}
