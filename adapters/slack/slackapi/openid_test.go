package slackapi_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// openIDToken returns an unsigned JWT with claims as its payload.
func openIDToken(claims string) string {
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

func TestOpenIDConnectToken(t *testing.T) {
	t.Parallel()

	request := slackapi.OpenIDConnectTokenRequest{
		ClientID:     "123.456",
		ClientSecret: "client-secret",
		Code:         "login-code",
		RedirectURI:  "https://coder.example.com/callback",
	}

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		idToken := openIDToken(`{
			"iss": "https://slack.com",
			"sub": "U0USER",
			"aud": "123.456",
			"exp": 1700000300,
			"iat": 1700000000,
			"auth_time": 1699999990,
			"nonce": "login-nonce",
			"at_hash": "abc",
			"https://slack.com/team_id": "T0TEAM",
			"https://slack.com/user_id": "U0USER"
		}`)
		srv := slackapitest.NewServer(t)
		srv.Respond("openid.connect.token", json.RawMessage(`{
			"ok": true,
			"access_token": "xoxp-user",
			"token_type": "Bearer",
			"id_token": "`+idToken+`"
		}`))

		resp, err := methodsNewClient(srv).OpenIDConnectToken(t.Context(), request)
		if err != nil {
			t.Fatalf("OpenIDConnectToken: %v", err)
		}
		calls := srv.Calls("openid.connect.token")
		if len(calls) != 1 {
			t.Fatalf("openid.connect.token calls = %d, want 1", len(calls))
		}
		if got := calls[0].Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
		methodsCheckEqual(t, "form", calls[0].Form, url.Values{
			"client_id":     {"123.456"},
			"client_secret": {"client-secret"},
			"code":          {"login-code"},
			"redirect_uri":  {"https://coder.example.com/callback"},
		})
		methodsCheckEqual(t, "response", resp, &slackapi.OpenIDConnectTokenResponse{
			AccessToken: "xoxp-user",
			TokenType:   "Bearer",
			IDToken:     idToken,
			Claims: slackapi.IDTokenClaims{
				Issuer:    "https://slack.com",
				Subject:   "U0USER",
				Audience:  "123.456",
				ExpiresAt: 1700000300,
				IssuedAt:  1700000000,
				AuthTime:  1699999990,
				Nonce:     "login-nonce",
				TeamID:    "T0TEAM",
				UserID:    "U0USER",
			},
		})
	})

	t.Run("InvalidIDToken", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name    string
			idToken string
			want    string
		}{
			{"Missing", "", "id_token is missing"},
			{"NotThreeParts", "header.payload", "id_token is not a JWT"},
			{"BadBase64", "header.!!!.signature", "invalid id_token payload"},
			{"BadJSON", openIDToken(`not json`), "invalid id_token claims"},
			{
				"OtherAudience",
				openIDToken(`{"aud":"999.999","https://slack.com/team_id":"T0TEAM","https://slack.com/user_id":"U0USER"}`),
				`id_token audience "999.999" is not the client ID "123.456"`,
			},
			{"NoTeamID", openIDToken(`{"aud":"123.456","https://slack.com/user_id":"U0USER"}`), "id_token has no team ID"},
			{"NoUserID", openIDToken(`{"aud":"123.456","https://slack.com/team_id":"T0TEAM"}`), "id_token has no user ID"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				body, err := json.Marshal(map[string]any{"ok": true, "access_token": "xoxp-user", "id_token": tt.idToken})
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				srv := slackapitest.NewServer(t)
				srv.Respond("openid.connect.token", json.RawMessage(body))

				resp, err := methodsNewClient(srv).OpenIDConnectToken(t.Context(), request)
				if resp != nil {
					t.Errorf("response = %+v, want nil", resp)
				}
				if want := "slack: openid.connect.token: " + tt.want; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("error = %v, want prefix %q", err, want)
				}
			})
		}
	})
}
