package slackapi_test

import (
	"encoding/json"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestPostMessage(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.postMessage", json.RawMessage(`{
			"ok": true,
			"channel": "C1",
			"ts": "1700000000.000100",
			"message": {"type": "message", "bot_id": "B1", "text": "Done", "ts": "1700000000.000100", "thread_ts": "1700000000.000001"}
		}`))

		resp, err := methodsNewClient(srv).PostMessage(t.Context(), slackapi.PostMessageRequest{
			Channel:     "C1",
			ThreadTS:    "1700000000.000001",
			Text:        "Done",
			Blocks:      []slackapi.Block{slackapi.MarkdownBlock{Text: "**Done**"}},
			UnfurlLinks: new(false),
		})
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.postMessage", `{
			"channel": "C1",
			"thread_ts": "1700000000.000001",
			"text": "Done",
			"blocks": [{"type": "markdown", "text": "**Done**"}],
			"unfurl_links": false
		}`)
		methodsCheckEqual(t, "response", resp, &slackapi.PostMessageResponse{
			Channel: "C1",
			TS:      "1700000000.000100",
			Message: &slackapi.Message{
				Type:     "message",
				BotID:    "B1",
				Text:     "Done",
				TS:       "1700000000.000100",
				ThreadTS: "1700000000.000001",
			},
		})
	})

	t.Run("MarkdownText", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.postMessage", json.RawMessage(`{"ok":true,"channel":"C1","ts":"1700000000.000100"}`))

		_, err := methodsNewClient(srv).PostMessage(t.Context(), slackapi.PostMessageRequest{
			Channel:      "C1",
			MarkdownText: "**Done**",
			UnfurlMedia:  new(true),
		})
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.postMessage", `{
			"channel": "C1",
			"markdown_text": "**Done**",
			"unfurl_media": true
		}`)
	})

	t.Run("Mrkdwn", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.postMessage", json.RawMessage(`{"ok":true,"channel":"C1","ts":"1700000000.000100"}`))

		_, err := methodsNewClient(srv).PostMessage(t.Context(), slackapi.PostMessageRequest{
			Channel: "C1",
			Text:    "*not bold*",
			Mrkdwn:  new(false),
		})
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.postMessage", `{
			"channel": "C1",
			"text": "*not bold*",
			"mrkdwn": false
		}`)
	})
}

func TestUpdateMessage(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.update", json.RawMessage(`{"ok":true,"channel":"C1","ts":"1700000000.000100","text":"Edited"}`))

		resp, err := methodsNewClient(srv).UpdateMessage(t.Context(), slackapi.UpdateMessageRequest{
			Channel: "C1",
			TS:      "1700000000.000100",
			Text:    "Edited",
			Blocks:  []slackapi.Block{slackapi.MarkdownBlock{Text: "_Edited_", BlockID: "b1"}},
		})
		if err != nil {
			t.Fatalf("UpdateMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.update", `{
			"channel": "C1",
			"ts": "1700000000.000100",
			"text": "Edited",
			"blocks": [{"type": "markdown", "text": "_Edited_", "block_id": "b1"}]
		}`)
		methodsCheckEqual(t, "response", resp, &slackapi.UpdateMessageResponse{
			Channel: "C1",
			TS:      "1700000000.000100",
			Text:    "Edited",
		})
	})

	t.Run("MarkdownText", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.update", json.RawMessage(`{"ok":true,"channel":"C1","ts":"1700000000.000100"}`))

		_, err := methodsNewClient(srv).UpdateMessage(t.Context(), slackapi.UpdateMessageRequest{
			Channel:      "C1",
			TS:           "1700000000.000100",
			MarkdownText: "_Edited_",
		})
		if err != nil {
			t.Fatalf("UpdateMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.update", `{
			"channel": "C1",
			"ts": "1700000000.000100",
			"markdown_text": "_Edited_"
		}`)
	})
}

func TestDeleteMessage(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.delete", json.RawMessage(`{"ok":true,"channel":"C1","ts":"1700000000.000100"}`))

		err := methodsNewClient(srv).DeleteMessage(t.Context(), slackapi.DeleteMessageRequest{
			Channel: "C1",
			TS:      "1700000000.000100",
		})
		if err != nil {
			t.Fatalf("DeleteMessage: %v", err)
		}
		methodsCheckJSONBody(t, srv, "chat.delete", `{"channel":"C1","ts":"1700000000.000100"}`)
	})
}

func TestPostEphemeral(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postEphemeral", json.RawMessage(`{"ok":true,"message_ts":"1700000000.000200"}`))

	resp, err := methodsNewClient(srv).PostEphemeral(t.Context(), slackapi.PostEphemeralRequest{
		Channel:  "C1",
		User:     "U1",
		ThreadTS: "1700000000.000001",
		Text:     "I only answer members of this workspace.",
	})
	if err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	methodsCheckJSONBody(t, srv, "chat.postEphemeral", `{
		"channel": "C1",
		"user": "U1",
		"thread_ts": "1700000000.000001",
		"text": "I only answer members of this workspace."
	}`)
	methodsCheckEqual(t, "response", resp, &slackapi.PostEphemeralResponse{MessageTS: "1700000000.000200"})
}

func TestPostEphemeralMrkdwn(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postEphemeral", json.RawMessage(`{"ok":true,"message_ts":"1700000000.000200"}`))

	_, err := methodsNewClient(srv).PostEphemeral(t.Context(), slackapi.PostEphemeralRequest{
		Channel: "C1",
		User:    "U1",
		Text:    "*not bold*",
		Mrkdwn:  new(false),
	})
	if err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	methodsCheckJSONBody(t, srv, "chat.postEphemeral", `{
		"channel": "C1",
		"user": "U1",
		"text": "*not bold*",
		"mrkdwn": false
	}`)
}
