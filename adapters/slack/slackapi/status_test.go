package slackapi_test

import (
	"encoding/json"
	"errors"
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

func TestSetAgentSessionStatus(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("agents.sessions.setStatus", json.RawMessage(`{"ok":true,"status":"processing","agent_status":"processing"}`))

		err := methodsNewClient(srv).SetAgentSessionStatus(t.Context(), slackapi.SetAgentSessionStatusRequest{
			ChannelID: "C1",
			ThreadTS:  "1700000000.000001",
			Status:    slackapi.AgentSessionProcessing,
		})
		if err != nil {
			t.Fatalf("SetAgentSessionStatus: %v", err)
		}
		methodsCheckJSONBody(t, srv, "agents.sessions.setStatus", `{
			"channel_id": "C1",
			"thread_ts": "1700000000.000001",
			"status": "processing"
		}`)
	})

	t.Run("FeatureDisabled", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("agents.sessions.setStatus", json.RawMessage(`{"ok":false,"error":"feature_disabled"}`))

		err := methodsNewClient(srv).SetAgentSessionStatus(t.Context(), slackapi.SetAgentSessionStatusRequest{
			ChannelID: "C1",
			ThreadTS:  "1700000000.000001",
			Status:    slackapi.AgentSessionActive,
		})
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok {
			t.Fatalf("SetAgentSessionStatus error = %v, want *slackapi.APIError", err)
		}
		if apiErr.Code != "feature_disabled" {
			t.Errorf("APIError.Code = %q, want %q", apiErr.Code, "feature_disabled")
		}
		if calls := srv.Calls("agents.sessions.setStatus"); len(calls) != 1 {
			t.Errorf("agents.sessions.setStatus calls = %d, want 1", len(calls))
		}
	})
}
