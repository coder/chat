package slackapi_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

// inboundEventBody wraps event in an event_callback envelope as Slack sends it.
func inboundEventBody(event string) string {
	return `{
		"token": "XXYYZZ",
		"team_id": "T0001",
		"context_team_id": "T0001",
		"context_enterprise_id": null,
		"api_app_id": "A0APP1234",
		"event": ` + event + `,
		"type": "event_callback",
		"event_id": "Ev0EVENT1234",
		"event_time": 1700000100,
		"authorizations": [{
			"enterprise_id": null,
			"team_id": "T0001",
			"user_id": "U0BOT1234",
			"is_bot": true,
			"is_enterprise_install": false
		}],
		"is_ext_shared_channel": false,
		"event_context": "4-eyJldCI6Im1lc3NhZ2UifQ"
	}`
}

func inboundParse(t *testing.T, body string) *slackapi.Envelope {
	t.Helper()
	env, err := slackapi.ParseEnvelope([]byte(body))
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	return env
}

func TestParseEnvelopeURLVerification(t *testing.T) {
	t.Parallel()

	env := inboundParse(t, `{
		"token": "Jhj5dZrVaK7ZwHHjRyZWjbDl",
		"challenge": "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P",
		"type": "url_verification"
	}`)
	want := &slackapi.Envelope{
		Type:      slackapi.EnvelopeURLVerification,
		Challenge: "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("envelope mismatch\ngot:  %+v\nwant: %+v", env, want)
	}
}

func TestParseEnvelopeEventCallback(t *testing.T) {
	t.Parallel()

	const event = `{"type":"reaction_added","user":"U061F7AUR","reaction":"eyes","item":{"type":"message","channel":"C0CHANNEL1","ts":"1700000200.000300"},"event_ts":"1700000900.001000"}`
	env := inboundParse(t, `{
		"token": "XXYYZZ",
		"team_id": "T0001",
		"api_app_id": "A0APP1234",
		"event": `+event+`,
		"type": "event_callback",
		"event_id": "Ev0EVENT5678",
		"event_time": 1700000900,
		"is_ext_shared_channel": true
	}`)
	want := &slackapi.Envelope{
		Type:               slackapi.EnvelopeEventCallback,
		TeamID:             "T0001",
		APIAppID:           "A0APP1234",
		EventID:            "Ev0EVENT5678",
		EventTime:          1700000900,
		IsExtSharedChannel: true,
		Event:              json.RawMessage(event),
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("envelope mismatch\ngot:  %+v\nwant: %+v", env, want)
	}
}

func TestParseEnvelopeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		syntax bool
	}{
		{name: "empty body", body: ``, syntax: true},
		{name: "truncated object", body: `{"type":"event_callback",`, syntax: true},
		{name: "not an object", body: `["event_callback"]`},
		{name: "wrong type field", body: `{"type":7}`},
		{name: "missing type", body: `{"team_id":"T0001","event_id":"Ev0EVENT1234","event":{"type":"message"}}`},
		{name: "empty type", body: `{"type":"","challenge":"abc"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, err := slackapi.ParseEnvelope([]byte(tt.body))
			if err == nil {
				t.Fatalf("expected error, got envelope %+v", env)
			}
			if env != nil {
				t.Fatalf("expected nil envelope on error, got %+v", env)
			}
			if _, ok := errors.AsType[*json.SyntaxError](err); ok != tt.syntax {
				t.Fatalf("wraps *json.SyntaxError = %t, want %t: %v", ok, tt.syntax, err)
			}
		})
	}
}

func TestEnvelopeMessageEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event string
		want  *slackapi.MessageEvent
	}{
		{
			name: "app mention in channel thread",
			event: `{
				"client_msg_id": "c9b8a8f1-7a3e-4a3c-9d2f-1f5e2b7c8d90",
				"type": "app_mention",
				"text": "<@U0BOT1234> can you look at this?",
				"user": "U061F7AUR",
				"ts": "1700000100.000200",
				"blocks": [{"type": "rich_text", "block_id": "a1B2c", "elements": [{"type": "rich_text_section", "elements": [{"type": "user", "user_id": "U0BOT1234"}, {"type": "text", "text": " can you look at this?"}]}]}],
				"team": "T0001",
				"thread_ts": "1700000000.000100",
				"parent_user_id": "U061F7AUR",
				"channel": "C0CHANNEL1",
				"event_ts": "1700000100.000200"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type:     slackapi.EventTypeAppMention,
					User:     "U061F7AUR",
					Text:     "<@U0BOT1234> can you look at this?",
					TS:       "1700000100.000200",
					ThreadTS: "1700000000.000100",
					Team:     "T0001",
				},
				Channel: "C0CHANNEL1",
				EventTS: "1700000100.000200",
			},
		},
		{
			name: "plain message in channel",
			event: `{
				"client_msg_id": "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b",
				"type": "message",
				"text": "deploy finished",
				"user": "U061F7AUR",
				"ts": "1700000200.000300",
				"blocks": [{"type": "rich_text", "block_id": "Qw3rT", "elements": []}],
				"team": "T0001",
				"channel": "C0CHANNEL1",
				"event_ts": "1700000200.000300",
				"channel_type": "channel"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type: slackapi.EventTypeMessage,
					User: "U061F7AUR",
					Text: "deploy finished",
					TS:   "1700000200.000300",
					Team: "T0001",
				},
				Channel:     "C0CHANNEL1",
				ChannelType: "channel",
				EventTS:     "1700000200.000300",
			},
		},
		{
			name: "bot message",
			event: `{
				"type": "message",
				"subtype": "bot_message",
				"text": "Build #42 passed",
				"ts": "1700000400.000500",
				"username": "CI",
				"icons": {"image_48": "https://example.com/ci.png"},
				"bot_id": "B0BOT5678",
				"bot_profile": {"id": "B0BOT5678", "app_id": "A0CIAPP12", "name": "CI Bot"},
				"app_id": "A0CIAPP12",
				"channel": "C0CHANNEL1",
				"event_ts": "1700000400.000500",
				"channel_type": "channel"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type:       slackapi.EventTypeMessage,
					Subtype:    slackapi.SubtypeBotMessage,
					BotID:      "B0BOT5678",
					Username:   "CI",
					BotProfile: &slackapi.BotProfile{ID: "B0BOT5678", Name: "CI Bot"},
					Text:       "Build #42 passed",
					TS:         "1700000400.000500",
				},
				Channel:     "C0CHANNEL1",
				ChannelType: "channel",
				EventTS:     "1700000400.000500",
			},
		},
		{
			name: "thread broadcast",
			event: `{
				"type": "message",
				"subtype": "thread_broadcast",
				"text": "fixed, details in the thread",
				"user": "U061F7AUR",
				"ts": "1700000500.000600",
				"thread_ts": "1700000200.000300",
				"root": {"type": "message", "user": "U061F7AUR", "text": "deploy finished", "ts": "1700000200.000300", "thread_ts": "1700000200.000300", "reply_count": 3},
				"client_msg_id": "4d5e6f7a-8b9c-4d0e-1f2a-3b4c5d6e7f8a",
				"team": "T0001",
				"channel": "C0CHANNEL1",
				"event_ts": "1700000500.000600",
				"channel_type": "channel"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type:     slackapi.EventTypeMessage,
					Subtype:  slackapi.SubtypeThreadBroadcast,
					User:     "U061F7AUR",
					Text:     "fixed, details in the thread",
					TS:       "1700000500.000600",
					ThreadTS: "1700000200.000300",
					Team:     "T0001",
				},
				Channel:     "C0CHANNEL1",
				ChannelType: "channel",
				EventTS:     "1700000500.000600",
			},
		},
		{
			name: "message changed",
			event: `{
				"type": "message",
				"subtype": "message_changed",
				"message": {
					"client_msg_id": "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b",
					"type": "message",
					"text": "deploy finished at 10:05",
					"user": "U061F7AUR",
					"team": "T0001",
					"edited": {"user": "U061F7AUR", "ts": "1700000600.000000"},
					"blocks": [],
					"ts": "1700000200.000300",
					"thread_ts": "1700000200.000300",
					"source_team": "T0001",
					"user_team": "T0001"
				},
				"previous_message": {
					"client_msg_id": "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b",
					"type": "message",
					"text": "deploy finished",
					"user": "U061F7AUR",
					"ts": "1700000200.000300",
					"thread_ts": "1700000200.000300",
					"team": "T0001",
					"blocks": []
				},
				"channel": "C0CHANNEL1",
				"hidden": true,
				"ts": "1700000600.000700",
				"event_ts": "1700000600.000700",
				"channel_type": "channel"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type:    slackapi.EventTypeMessage,
					Subtype: slackapi.SubtypeMessageChanged,
					TS:      "1700000600.000700",
				},
				Channel:     "C0CHANNEL1",
				ChannelType: "channel",
				EventTS:     "1700000600.000700",
				NewMessage: &slackapi.Message{
					Type:       "message",
					User:       "U061F7AUR",
					Text:       "deploy finished at 10:05",
					TS:         "1700000200.000300",
					ThreadTS:   "1700000200.000300",
					Team:       "T0001",
					UserTeam:   "T0001",
					SourceTeam: "T0001",
					Edited:     &slackapi.Edited{User: "U061F7AUR", TS: "1700000600.000000"},
				},
				PreviousMessage: &slackapi.Message{
					Type:     "message",
					User:     "U061F7AUR",
					Text:     "deploy finished",
					TS:       "1700000200.000300",
					ThreadTS: "1700000200.000300",
					Team:     "T0001",
				},
			},
		},
		{
			name: "message deleted",
			event: `{
				"type": "message",
				"subtype": "message_deleted",
				"previous_message": {
					"type": "message",
					"text": "oops, wrong channel",
					"user": "U061F7AUR",
					"ts": "1700000700.000800",
					"team": "T0001"
				},
				"channel": "C0CHANNEL1",
				"hidden": true,
				"deleted_ts": "1700000700.000800",
				"event_ts": "1700000800.000900",
				"ts": "1700000800.000900",
				"channel_type": "channel"
			}`,
			want: &slackapi.MessageEvent{
				Message: slackapi.Message{
					Type:    slackapi.EventTypeMessage,
					Subtype: slackapi.SubtypeMessageDeleted,
					TS:      "1700000800.000900",
				},
				Channel:     "C0CHANNEL1",
				ChannelType: "channel",
				EventTS:     "1700000800.000900",
				PreviousMessage: &slackapi.Message{
					Type: "message",
					User: "U061F7AUR",
					Text: "oops, wrong channel",
					TS:   "1700000700.000800",
					Team: "T0001",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := inboundParse(t, inboundEventBody(tt.event))
			got, ok, err := env.MessageEvent()
			if err != nil {
				t.Fatalf("message event: %v", err)
			}
			if !ok {
				t.Fatal("expected a message event")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("event mismatch\ngot:  %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestEnvelopeMessageEventIgnored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "reaction added",
			body: inboundEventBody(`{"type":"reaction_added","user":"U061F7AUR","reaction":"eyes","item_user":"U0BOT1234","item":{"type":"message","channel":"C0CHANNEL1","ts":"1700000200.000300"},"event_ts":"1700000900.001000"}`),
		},
		{
			name: "channel created with object channel",
			body: inboundEventBody(`{"type":"channel_created","channel":{"id":"C0NEW12345","name":"incident-42","created":1700001000,"creator":"U061F7AUR"},"event_ts":"1700001000.001100"}`),
		},
		{
			name: "null event",
			body: inboundEventBody(`null`),
		},
		{
			name: "url verification",
			body: `{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`,
		},
		{
			name: "app rate limited",
			body: `{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","type":"app_rate_limited","team_id":"T0001","minute_rate_limited":1700001060,"api_app_id":"A0APP1234"}`,
		},
		{
			name: "message event in other envelope type",
			body: `{"type":"app_rate_limited","team_id":"T0001","event":{"type":"message","channel":"C0CHANNEL1","user":"U061F7AUR","text":"hi","ts":"1700000200.000300"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := inboundParse(t, tt.body)
			got, ok, err := env.MessageEvent()
			if err != nil {
				t.Fatalf("message event: %v", err)
			}
			if ok || got != nil {
				t.Fatalf("expected no message event, got ok=%t event=%+v", ok, got)
			}
		})
	}
}

func TestEnvelopeMessageEventErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  *slackapi.Envelope
	}{
		{
			name: "missing event",
			env:  inboundParse(t, `{"type":"event_callback","team_id":"T0001","event_id":"Ev0EVENT1234"}`),
		},
		{
			name: "truncated event",
			env:  &slackapi.Envelope{Type: slackapi.EnvelopeEventCallback, Event: json.RawMessage(`{"type":"message",`)},
		},
		{
			name: "event is a string",
			env:  inboundParse(t, inboundEventBody(`"message"`)),
		},
		{
			name: "event type is a number",
			env:  inboundParse(t, inboundEventBody(`{"type":7,"text":"hi"}`)),
		},
		{
			name: "message ts is a number",
			env:  inboundParse(t, inboundEventBody(`{"type":"message","channel":"C0CHANNEL1","user":"U061F7AUR","text":"hi","ts":1700000200.0003}`)),
		},
		{
			name: "app mention files is an object",
			env:  inboundParse(t, inboundEventBody(`{"type":"app_mention","channel":"C0CHANNEL1","user":"U061F7AUR","ts":"1700000100.000200","files":{"id":"F0FILE1234"}}`)),
		},
		{
			name: "message changed inner message is a string",
			env:  inboundParse(t, inboundEventBody(`{"type":"message","subtype":"message_changed","channel":"C0CHANNEL1","ts":"1700000600.000700","message":"deploy finished"}`)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := tt.env.MessageEvent()
			if err == nil {
				t.Fatalf("expected error, got ok=%t event=%+v", ok, got)
			}
			if ok || got != nil {
				t.Fatalf("expected no event on error, got ok=%t event=%+v", ok, got)
			}
		})
	}
}
