package slackapi_test

import (
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func TestParseBlockActions(t *testing.T) {
	t.Parallel()

	t.Run("ButtonClick", func(t *testing.T) {
		t.Parallel()
		got, ok, err := slackapi.ParseBlockActions([]byte(`{
			"type": "block_actions",
			"team": {"id": "T0TEAM", "domain": "acme"},
			"user": {"id": "U0USER", "username": "ada", "name": "ada", "team_id": "T0TEAM"},
			"api_app_id": "A0APP",
			"token": "verification-token",
			"container": {"type": "message", "message_ts": "1700000000.000200", "channel_id": "C0CHAN", "is_ephemeral": false},
			"trigger_id": "123.456.abc",
			"channel": {"id": "C0CHAN", "name": "general"},
			"enterprise": null,
			"message": {
				"type": "message",
				"user": "U0BOT",
				"bot_id": "B0BOT",
				"text": "Done.",
				"ts": "1700000000.000200",
				"thread_ts": "1700000000.000100",
				"team": "T0TEAM",
				"blocks": [{"type": "actions", "block_id": "open", "elements": []}]
			},
			"state": {"values": {}},
			"response_url": "https://hooks.slack.com/actions/T0TEAM/1/abc",
			"actions": [{
				"action_id": "open_in_coder",
				"block_id": "open",
				"text": {"type": "plain_text", "text": "Open in Coder", "emoji": true},
				"value": "chat-1",
				"type": "button",
				"action_ts": "1700000001.000300"
			}]
		}`))
		if err != nil || !ok {
			t.Fatalf("ParseBlockActions = %v, %v, want true, nil", ok, err)
		}
		methodsCheckEqual(t, "payload", got, &slackapi.BlockActionsPayload{
			Type:     slackapi.InteractionTypeBlockActions,
			Team:     slackapi.InteractionTeam{ID: "T0TEAM", Domain: "acme"},
			User:     slackapi.InteractionUser{ID: "U0USER", Username: "ada", Name: "ada", TeamID: "T0TEAM"},
			APIAppID: "A0APP",
			Container: slackapi.InteractionContainer{
				Type:      "message",
				MessageTS: "1700000000.000200",
				ChannelID: "C0CHAN",
			},
			TriggerID: "123.456.abc",
			Channel:   slackapi.InteractionChannel{ID: "C0CHAN", Name: "general"},
			Message: &slackapi.Message{
				Type:     "message",
				User:     "U0BOT",
				BotID:    "B0BOT",
				Text:     "Done.",
				TS:       "1700000000.000200",
				ThreadTS: "1700000000.000100",
				Team:     "T0TEAM",
			},
			ResponseURL: "https://hooks.slack.com/actions/T0TEAM/1/abc",
			Actions: []slackapi.BlockAction{{
				Type:     "button",
				ActionID: "open_in_coder",
				BlockID:  "open",
				Value:    "chat-1",
				ActionTS: "1700000001.000300",
			}},
		})
	})

	t.Run("OtherType", func(t *testing.T) {
		t.Parallel()
		got, ok, err := slackapi.ParseBlockActions([]byte(`{"type":"view_submission","channel":"C0CHAN","view":{"id":"V0VIEW"}}`))
		if got != nil || ok || err != nil {
			t.Fatalf("ParseBlockActions = %+v, %v, %v, want nil, false, nil", got, ok, err)
		}
	})

	for name, payload := range map[string]string{
		"InvalidJSON":         `{`,
		"InvalidBlockActions": `{"type":"block_actions","actions":"open"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := slackapi.ParseBlockActions([]byte(payload))
			if got != nil || ok || err == nil {
				t.Fatalf("ParseBlockActions = %+v, %v, %v, want nil, false, an error", got, ok, err)
			}
		})
	}
}
