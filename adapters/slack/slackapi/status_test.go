package slackapi_test

import (
	"encoding/json"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestSetAssistantThreadStatus(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("assistant.threads.setStatus", json.RawMessage(`{"ok":true}`))

		err := methodsNewClient(srv).SetAssistantThreadStatus(t.Context(), slackapi.SetAssistantThreadStatusRequest{
			ChannelID:       "D1",
			ThreadTS:        "1700000000.000001",
			Status:          "is thinking...",
			LoadingMessages: []string{"Reading the thread", "Writing a reply"},
		})
		if err != nil {
			t.Fatalf("SetAssistantThreadStatus: %v", err)
		}
		methodsCheckJSONBody(t, srv, "assistant.threads.setStatus", `{
			"channel_id": "D1",
			"thread_ts": "1700000000.000001",
			"status": "is thinking...",
			"loading_messages": ["Reading the thread", "Writing a reply"]
		}`)
	})

	t.Run("EmptyStatus", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("assistant.threads.setStatus", json.RawMessage(`{"ok":true}`))

		err := methodsNewClient(srv).SetAssistantThreadStatus(t.Context(), slackapi.SetAssistantThreadStatusRequest{
			ChannelID: "D1",
			ThreadTS:  "1700000000.000001",
		})
		if err != nil {
			t.Fatalf("SetAssistantThreadStatus: %v", err)
		}
		methodsCheckJSONBody(t, srv, "assistant.threads.setStatus", `{
			"channel_id": "D1",
			"thread_ts": "1700000000.000001",
			"status": ""
		}`)
	})
}
