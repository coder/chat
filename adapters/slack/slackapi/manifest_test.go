package slackapi_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

const formatsManifestJSON = `{
  "_metadata": {"major_version": 1, "minor_version": 1},
  "display_information": {
    "name": "Coder Agents",
    "description": "Run Coder Agents from Slack.",
    "long_description": "Mention the bot in a channel to start a Coder Agents chat.",
    "background_color": "#1F2937"
  },
  "features": {
    "bot_user": {"display_name": "coder", "always_online": true}
  },
  "oauth_config": {
    "scopes": {"bot": ["app_mentions:read", "chat:write", "users:read"]}
  },
  "settings": {
    "event_subscriptions": {
      "request_url": "https://coder.example.com/api/v2/slack/events",
      "bot_events": ["app_mention", "message.im"]
    },
    "interactivity": {
      "is_enabled": true,
      "request_url": "https://coder.example.com/api/v2/slack/interactions"
    },
    "org_deploy_enabled": false,
    "socket_mode_enabled": false,
    "token_rotation_enabled": false
  }
}`

func formatsManifest() slackapi.Manifest {
	return slackapi.Manifest{
		Metadata: &slackapi.ManifestMetadata{MajorVersion: 1, MinorVersion: 1},
		DisplayInformation: slackapi.ManifestDisplayInformation{
			Name:            "Coder Agents",
			Description:     "Run Coder Agents from Slack.",
			LongDescription: "Mention the bot in a channel to start a Coder Agents chat.",
			BackgroundColor: "#1F2937",
		},
		Features: &slackapi.ManifestFeatures{
			BotUser: &slackapi.ManifestBotUser{DisplayName: "coder", AlwaysOnline: true},
		},
		OAuthConfig: &slackapi.ManifestOAuthConfig{
			Scopes: &slackapi.ManifestScopes{Bot: []string{"app_mentions:read", "chat:write", "users:read"}},
		},
		Settings: &slackapi.ManifestSettings{
			EventSubscriptions: &slackapi.ManifestEventSubscriptions{
				RequestURL: "https://coder.example.com/api/v2/slack/events",
				BotEvents:  []string{"app_mention", "message.im"},
			},
			Interactivity: &slackapi.ManifestInteractivity{
				IsEnabled:  true,
				RequestURL: "https://coder.example.com/api/v2/slack/interactions",
			},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	t.Parallel()

	var m slackapi.Manifest
	if err := json.Unmarshal([]byte(formatsManifestJSON), &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if want := formatsManifest(); !reflect.DeepEqual(m, want) {
		t.Fatalf("decoded manifest\n got: %+v\nwant: %+v", m, want)
	}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	formatsAssertSameJSON(t, got, []byte(formatsManifestJSON))
}

func TestManifestMinimal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest slackapi.Manifest
		want     string
	}{
		{
			name:     "NameOnly",
			manifest: slackapi.Manifest{DisplayInformation: slackapi.ManifestDisplayInformation{Name: "bot"}},
			want:     `{"display_information":{"name":"bot"}}`,
		},
		{
			name: "EmptySections",
			manifest: slackapi.Manifest{
				Metadata:           &slackapi.ManifestMetadata{},
				DisplayInformation: slackapi.ManifestDisplayInformation{Name: "bot"},
				Features:           &slackapi.ManifestFeatures{BotUser: &slackapi.ManifestBotUser{DisplayName: "bot"}},
				OAuthConfig:        &slackapi.ManifestOAuthConfig{Scopes: &slackapi.ManifestScopes{}},
				Settings: &slackapi.ManifestSettings{
					EventSubscriptions: &slackapi.ManifestEventSubscriptions{},
					Interactivity:      &slackapi.ManifestInteractivity{},
				},
			},
			want: `{"_metadata":{},"display_information":{"name":"bot"},` +
				`"features":{"bot_user":{"display_name":"bot","always_online":false}},` +
				`"oauth_config":{"scopes":{}},` +
				`"settings":{"event_subscriptions":{},"interactivity":{"is_enabled":false},` +
				`"org_deploy_enabled":false,"socket_mode_enabled":false,"token_rotation_enabled":false}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			formatsAssertJSON(t, tt.manifest, tt.want)
		})
	}
}

func formatsAssertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v", err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
