package slackapi_test

import (
	"encoding/json"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestAuthTest(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("auth.test", json.RawMessage(`{
			"ok": true,
			"url": "https://example.slack.com/",
			"team": "Example",
			"user": "bot",
			"team_id": "T1",
			"user_id": "U1",
			"bot_id": "B1"
		}`))

		resp, err := methodsNewClient(srv).AuthTest(t.Context())
		if err != nil {
			t.Fatalf("AuthTest: %v", err)
		}
		methodsCheckJSONBody(t, srv, "auth.test", `{}`)
		methodsCheckEqual(t, "response", resp, &slackapi.AuthTestResponse{
			TeamID: "T1",
			Team:   "Example",
			UserID: "U1",
			BotID:  "B1",
			URL:    "https://example.slack.com/",
		})
	})
}
