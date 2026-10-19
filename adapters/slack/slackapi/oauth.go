package slackapi

import (
	"context"
	"net/url"
)

// OAuthV2AccessRequest is the request of oauth.v2.access.
type OAuthV2AccessRequest struct {
	// ClientID is the OAuth client ID of the app.
	ClientID string
	// ClientSecret is the OAuth client secret of the app.
	ClientSecret string
	// Code is the code that Slack sent to the redirect URL of the app.
	Code string
	// RedirectURI is the redirect_uri of the authorize request. Slack requires
	// it when the authorize request had one. Empty sends none.
	RedirectURI string
}

// OAuthV2AccessResponse is the response of oauth.v2.access for a bot install.
type OAuthV2AccessResponse struct {
	// AccessToken is the bot token (xoxb-).
	AccessToken string `json:"access_token"`
	// TokenType is "bot" for a bot token.
	TokenType string `json:"token_type"`
	// Scope is the comma-separated list of the scopes of the bot token.
	Scope string `json:"scope"`
	// BotUserID is the user ID of the bot.
	BotUserID string `json:"bot_user_id"`
	// AppID is the ID of the app.
	AppID string `json:"app_id"`
	// Team is the workspace that installed the app. Slack sends no team for an
	// install on an Enterprise Grid organization.
	Team OAuthTeam `json:"team"`
	// Enterprise is the Enterprise Grid organization of the install. It is
	// empty outside Enterprise Grid.
	Enterprise OAuthTeam `json:"enterprise"`
	// IsEnterpriseInstall is true for an install on a whole Enterprise Grid
	// organization.
	IsEnterpriseInstall bool `json:"is_enterprise_install"`
	// RefreshToken refreshes AccessToken when the app has token rotation on.
	// It is empty otherwise.
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn is the lifetime of AccessToken in seconds when the app has
	// token rotation on. It is zero otherwise.
	ExpiresIn int `json:"expires_in"`
}

// OAuthTeam is the ID and the name of a workspace or an Enterprise Grid
// organization in an oauth.v2.access response.
type OAuthTeam struct {
	// ID is the team ID (T) or the organization ID (E).
	ID string `json:"id"`
	// Name is the name of the workspace or the organization.
	Name string `json:"name"`
}

// OAuthV2Access calls oauth.v2.access to exchange the code of an install for a
// bot token. It sends the client ID and the client secret in a form-encoded
// body, and never sends the token of c, so any Client can call it. Slack
// returns the error code "invalid_code" for a code that is unknown, expired, or
// already used, and "bad_client_secret" for a wrong client secret.
func (c *Client) OAuthV2Access(ctx context.Context, req OAuthV2AccessRequest) (*OAuthV2AccessResponse, error) {
	values := url.Values{}
	setFormString(values, "client_id", req.ClientID)
	setFormString(values, "client_secret", req.ClientSecret)
	setFormString(values, "code", req.Code)
	setFormString(values, "redirect_uri", req.RedirectURI)
	var resp OAuthV2AccessResponse
	if err := c.WithToken("").Call(ctx, "oauth.v2.access", values, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
