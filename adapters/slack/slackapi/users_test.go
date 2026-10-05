package slackapi_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestUserInfo(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("users.info", json.RawMessage(`{
			"ok": true,
			"user": {
				"id": "U1",
				"team_id": "T1",
				"name": "ada",
				"real_name": "Ada Lovelace",
				"deleted": true,
				"is_bot": true,
				"is_stranger": true,
				"is_restricted": true,
				"is_ultra_restricted": true,
				"tz": "Europe/London",
				"locale": "en-GB",
				"profile": {"display_name": "ada.l", "real_name": "Ada Lovelace", "email": "ada@example.com"}
			}
		}`))

		user, err := methodsNewClient(srv).UserInfo(t.Context(), slackapi.UserInfoRequest{
			User:          "U1",
			IncludeLocale: true,
		})
		if err != nil {
			t.Fatalf("UserInfo: %v", err)
		}
		methodsCheckForm(t, srv, "users.info", url.Values{
			"user":           {"U1"},
			"include_locale": {"true"},
		})
		methodsCheckEqual(t, "user", user, &slackapi.User{
			ID:                "U1",
			TeamID:            "T1",
			Name:              "ada",
			RealName:          "Ada Lovelace",
			Deleted:           true,
			IsBot:             true,
			IsStranger:        true,
			IsRestricted:      true,
			IsUltraRestricted: true,
			TZ:                "Europe/London",
			Locale:            "en-GB",
			Profile: slackapi.UserProfile{
				DisplayName: "ada.l",
				RealName:    "Ada Lovelace",
				Email:       "ada@example.com",
			},
		})
	})
}

func TestUserDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user slackapi.User
		want string
	}{
		{
			name: "ProfileDisplayName",
			user: slackapi.User{
				Name:     "ada",
				RealName: "Ada Lovelace",
				Profile:  slackapi.UserProfile{DisplayName: "ada.l", RealName: "Ada L."},
			},
			want: "ada.l",
		},
		{
			name: "RealName",
			user: slackapi.User{
				Name:     "ada",
				RealName: "Ada Lovelace",
				Profile:  slackapi.UserProfile{RealName: "Ada L."},
			},
			want: "Ada Lovelace",
		},
		{
			name: "ProfileRealName",
			user: slackapi.User{
				Name:    "ada",
				Profile: slackapi.UserProfile{RealName: "Ada L."},
			},
			want: "Ada L.",
		},
		{
			name: "Name",
			user: slackapi.User{Name: "ada"},
			want: "ada",
		},
		{
			name: "Empty",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.user.DisplayName(); got != tt.want {
				t.Errorf("DisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}
