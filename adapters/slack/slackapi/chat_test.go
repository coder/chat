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

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.postMessage", methodsSlackError("markdown_text_conflict"))

		resp, err := methodsNewClient(srv).PostMessage(t.Context(), slackapi.PostMessageRequest{
			Channel:      "C1",
			Text:         "Done",
			MarkdownText: "**Done**",
		})
		if resp != nil {
			t.Errorf("response = %+v, want nil", resp)
		}
		methodsCheckAPIError(t, err, "chat.postMessage", "markdown_text_conflict")
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

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.update", methodsSlackError("message_not_found"))

		resp, err := methodsNewClient(srv).UpdateMessage(t.Context(), slackapi.UpdateMessageRequest{
			Channel: "C1",
			TS:      "1700000000.000100",
			Text:    "Edited",
		})
		if resp != nil {
			t.Errorf("response = %+v, want nil", resp)
		}
		methodsCheckAPIError(t, err, "chat.update", "message_not_found")
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

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("chat.delete", methodsSlackError("cant_delete_message"))

		err := methodsNewClient(srv).DeleteMessage(t.Context(), slackapi.DeleteMessageRequest{
			Channel: "C1",
			TS:      "1700000000.000100",
		})
		methodsCheckAPIError(t, err, "chat.delete", "cant_delete_message")
	})
}
