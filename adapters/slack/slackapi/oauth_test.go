package slackapi_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestOAuthV2Access(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("oauth.v2.access", json.RawMessage(`{
			"ok": true,
			"access_token": "xoxb-installed",
			"token_type": "bot",
			"scope": "app_mentions:read,chat:write",
			"bot_user_id": "U0BOT",
			"app_id": "A0APP",
			"team": {"name": "Acme", "id": "T0TEAM"},
			"enterprise": null,
			"authed_user": {"id": "U0ADMIN"},
			"is_enterprise_install": false
		}`))

		resp, err := methodsNewClient(srv).OAuthV2Access(t.Context(), slackapi.OAuthV2AccessRequest{
			ClientID:     "123.456",
			ClientSecret: "client-secret",
			Code:         "install-code",
			RedirectURI:  "https://coder.example.com/callback",
		})
		if err != nil {
			t.Fatalf("OAuthV2Access: %v", err)
		}
		calls := srv.Calls("oauth.v2.access")
		if len(calls) != 1 {
			t.Fatalf("oauth.v2.access calls = %d, want 1", len(calls))
		}
		if got := calls[0].Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
		methodsCheckEqual(t, "form", calls[0].Form, url.Values{
			"client_id":     {"123.456"},
			"client_secret": {"client-secret"},
			"code":          {"install-code"},
			"redirect_uri":  {"https://coder.example.com/callback"},
		})
		methodsCheckEqual(t, "response", resp, &slackapi.OAuthV2AccessResponse{
			AccessToken: "xoxb-installed",
			TokenType:   "bot",
			Scope:       "app_mentions:read,chat:write",
			BotUserID:   "U0BOT",
			AppID:       "A0APP",
			Team:        slackapi.OAuthTeam{ID: "T0TEAM", Name: "Acme"},
		})
	})

	t.Run("NoRedirectURI", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("oauth.v2.access", json.RawMessage(`{"ok":true}`))

		_, err := methodsNewClient(srv).OAuthV2Access(t.Context(), slackapi.OAuthV2AccessRequest{
			ClientID:     "123.456",
			ClientSecret: "client-secret",
			Code:         "install-code",
		})
		if err != nil {
			t.Fatalf("OAuthV2Access: %v", err)
		}
		methodsCheckEqual(t, "form", srv.Calls("oauth.v2.access")[0].Form, url.Values{
			"client_id":     {"123.456"},
			"client_secret": {"client-secret"},
			"code":          {"install-code"},
		})
	})
}
