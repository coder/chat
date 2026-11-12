package slackapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// OpenIDConnectTokenRequest is the request of openid.connect.token.
type OpenIDConnectTokenRequest struct {
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

// OpenIDConnectTokenResponse is the response of openid.connect.token for a
// Sign in with Slack login.
type OpenIDConnectTokenResponse struct {
	// AccessToken is the user token (xoxp-).
	AccessToken string `json:"access_token"`
	// TokenType is "Bearer".
	TokenType string `json:"token_type"`
	// IDToken is the ID token, a JWT, exactly as Slack sent it.
	IDToken string `json:"id_token"`
	// RefreshToken refreshes AccessToken when the app has token rotation on.
	// It is empty otherwise.
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn is the lifetime of AccessToken in seconds when the app has
	// token rotation on. It is zero otherwise.
	ExpiresIn int `json:"expires_in"`
	// Claims is the decoded payload of IDToken.
	Claims IDTokenClaims `json:"-"`
}

// IDTokenClaims are the claims of a Sign in with Slack ID token that the
// openid scope gives.
type IDTokenClaims struct {
	// Issuer is the issuer, "https://slack.com" for slack.com.
	Issuer string `json:"iss"`
	// Subject is the ID of the user who signed in.
	Subject string `json:"sub"`
	// Audience is the client ID of the app that the token is for.
	Audience string `json:"aud"`
	// ExpiresAt is the expiry time of the token in Unix seconds.
	ExpiresAt int64 `json:"exp"`
	// IssuedAt is the time that Slack issued the token in Unix seconds.
	IssuedAt int64 `json:"iat"`
	// AuthTime is the time that the user signed in in Unix seconds.
	AuthTime int64 `json:"auth_time"`
	// Nonce is the nonce of the authorize request. It is empty when the
	// authorize request had none.
	Nonce string `json:"nonce"`
	// TeamID is the ID of the workspace that the user signed in to.
	TeamID string `json:"https://slack.com/team_id"`
	// UserID is the ID of the user who signed in.
	UserID string `json:"https://slack.com/user_id"`
}

// OpenIDConnectToken calls openid.connect.token to exchange the code of a Sign
// in with Slack login for a user token and an ID token. It sends the client ID
// and the client secret in a form-encoded body, and never sends the token of
// c, so any Client can call it. It decodes the payload of the ID token into
// Claims, and returns an error when the ID token is missing or malformed, when
// its audience is not req.ClientID, or when it has no team ID or user ID.
//
// It does not check the signature of the ID token, because the token comes
// straight from Slack over TLS, which OpenID Connect Core 1.0 section 3.1.3.7
// allows. It does not check the issuer, which differs between Slack hosts, or
// the expiry and the nonce, which the caller can check with Claims.
func (c *Client) OpenIDConnectToken(ctx context.Context, req OpenIDConnectTokenRequest) (*OpenIDConnectTokenResponse, error) {
	values := url.Values{}
	setFormString(values, "client_id", req.ClientID)
	setFormString(values, "client_secret", req.ClientSecret)
	setFormString(values, "code", req.Code)
	setFormString(values, "redirect_uri", req.RedirectURI)
	var resp OpenIDConnectTokenResponse
	if err := c.WithToken("").Call(ctx, "openid.connect.token", values, &resp); err != nil {
		return nil, err
	}
	claims, err := parseIDToken(resp.IDToken, req.ClientID)
	if err != nil {
		return nil, fmt.Errorf("slack: openid.connect.token: %w", err)
	}
	resp.Claims = claims
	return &resp, nil
}

// parseIDToken decodes the payload of the ID token and checks its audience,
// team ID, and user ID. It does not check the signature.
func parseIDToken(token, clientID string) (IDTokenClaims, error) {
	var claims IDTokenClaims
	if token == "" {
		return claims, errors.New("id_token is missing")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, fmt.Errorf("invalid id_token payload: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("invalid id_token claims: %w", err)
	}
	switch {
	case claims.Audience != clientID:
		return claims, fmt.Errorf("id_token audience %q is not the client ID %q", claims.Audience, clientID)
	case claims.TeamID == "":
		return claims, errors.New("id_token has no team ID")
	case claims.UserID == "":
		return claims, errors.New("id_token has no user ID")
	}
	return claims, nil
}
