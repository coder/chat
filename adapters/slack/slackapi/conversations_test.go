package slackapi_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestConversationInfo(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.info", json.RawMessage(`{
			"ok": true,
			"channel": {
				"id": "C1",
				"name": "general",
				"is_channel": true,
				"is_group": false,
				"is_im": false,
				"is_mpim": false,
				"is_private": false,
				"is_archived": false,
				"is_shared": true,
				"is_ext_shared": true,
				"is_org_shared": false,
				"is_pending_ext_shared": false,
				"is_member": true
			}
		}`))

		conv, err := methodsNewClient(srv).ConversationInfo(t.Context(), slackapi.ConversationInfoRequest{Channel: "C1"})
		if err != nil {
			t.Fatalf("ConversationInfo: %v", err)
		}
		methodsCheckForm(t, srv, "conversations.info", url.Values{"channel": {"C1"}})
		methodsCheckEqual(t, "conversation", conv, &slackapi.Conversation{
			ID:          "C1",
			Name:        "general",
			IsChannel:   true,
			IsShared:    true,
			IsExtShared: true,
			IsMember:    true,
		})
	})

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.info", methodsSlackError("channel_not_found"))

		conv, err := methodsNewClient(srv).ConversationInfo(t.Context(), slackapi.ConversationInfoRequest{Channel: "C404"})
		if conv != nil {
			t.Errorf("conversation = %+v, want nil", conv)
		}
		methodsCheckAPIError(t, err, "conversations.info", "channel_not_found")
	})
}

const methodsPageBody = `{
	"ok": true,
	"messages": [
		{"type": "message", "user": "U1", "text": "first", "ts": "1700000000.000001"},
		{"type": "message", "bot_id": "B1", "text": "second", "ts": "1700000000.000002", "thread_ts": "1700000000.000001"}
	],
	"has_more": true,
	"response_metadata": {"next_cursor": "bmV4dA=="}
}`

var methodsWantPage = &slackapi.MessagePage{
	Messages: []slackapi.Message{
		{Type: "message", User: "U1", Text: "first", TS: "1700000000.000001"},
		{Type: "message", BotID: "B1", Text: "second", TS: "1700000000.000002", ThreadTS: "1700000000.000001"},
	},
	HasMore:    true,
	NextCursor: "bmV4dA==",
}

func TestConversationHistory(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.history", json.RawMessage(methodsPageBody))

		page, err := methodsNewClient(srv).ConversationHistory(t.Context(), slackapi.ConversationHistoryRequest{
			Channel:   "C1",
			Oldest:    "1700000000.000000",
			Latest:    "1700000100.000000",
			Inclusive: true,
			Limit:     50,
			Cursor:    "Y3Vy",
		})
		if err != nil {
			t.Fatalf("ConversationHistory: %v", err)
		}
		methodsCheckForm(t, srv, "conversations.history", url.Values{
			"channel":   {"C1"},
			"oldest":    {"1700000000.000000"},
			"latest":    {"1700000100.000000"},
			"inclusive": {"true"},
			"limit":     {"50"},
			"cursor":    {"Y3Vy"},
		})
		methodsCheckEqual(t, "page", page, methodsWantPage)
	})

	t.Run("LastPage", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.history", json.RawMessage(`{"ok":true,"messages":[],"has_more":false}`))

		page, err := methodsNewClient(srv).ConversationHistory(t.Context(), slackapi.ConversationHistoryRequest{Channel: "C1"})
		if err != nil {
			t.Fatalf("ConversationHistory: %v", err)
		}
		if page.HasMore || page.NextCursor != "" || len(page.Messages) != 0 {
			t.Errorf("page = %+v, want an empty last page", page)
		}
	})

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.history", methodsSlackError("channel_not_found"))

		page, err := methodsNewClient(srv).ConversationHistory(t.Context(), slackapi.ConversationHistoryRequest{Channel: "C404"})
		if page != nil {
			t.Errorf("page = %+v, want nil", page)
		}
		methodsCheckAPIError(t, err, "conversations.history", "channel_not_found")
	})
}

func TestConversationReplies(t *testing.T) {
	t.Parallel()

	t.Run("Request", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.replies", json.RawMessage(methodsPageBody))

		page, err := methodsNewClient(srv).ConversationReplies(t.Context(), slackapi.ConversationRepliesRequest{
			Channel:   "C1",
			TS:        "1700000000.000001",
			Oldest:    "1700000000.000000",
			Latest:    "1700000100.000000",
			Inclusive: true,
			Limit:     200,
			Cursor:    "Y3Vy",
		})
		if err != nil {
			t.Fatalf("ConversationReplies: %v", err)
		}
		methodsCheckForm(t, srv, "conversations.replies", url.Values{
			"channel":   {"C1"},
			"ts":        {"1700000000.000001"},
			"oldest":    {"1700000000.000000"},
			"latest":    {"1700000100.000000"},
			"inclusive": {"true"},
			"limit":     {"200"},
			"cursor":    {"Y3Vy"},
		})
		methodsCheckEqual(t, "page", page, methodsWantPage)
	})

	t.Run("APIError", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.Respond("conversations.replies", methodsSlackError("thread_not_found"))

		page, err := methodsNewClient(srv).ConversationReplies(t.Context(), slackapi.ConversationRepliesRequest{
			Channel: "C1",
			TS:      "1700000000.999999",
		})
		if page != nil {
			t.Errorf("page = %+v, want nil", page)
		}
		methodsCheckAPIError(t, err, "conversations.replies", "thread_not_found")
	})
}
