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
				"is_group": true,
				"is_im": true,
				"is_mpim": true,
				"is_private": true,
				"is_archived": true,
				"is_shared": true,
				"is_ext_shared": true,
				"is_org_shared": true,
				"is_pending_ext_shared": true,
				"is_member": true
			}
		}`))

		conv, err := methodsNewClient(srv).ConversationInfo(t.Context(), slackapi.ConversationInfoRequest{Channel: "C1"})
		if err != nil {
			t.Fatalf("ConversationInfo: %v", err)
		}
		methodsCheckForm(t, srv, "conversations.info", url.Values{"channel": {"C1"}})
		methodsCheckEqual(t, "conversation", conv, &slackapi.Conversation{
			ID:                 "C1",
			Name:               "general",
			IsChannel:          true,
			IsGroup:            true,
			IsIM:               true,
			IsMPIM:             true,
			IsPrivate:          true,
			IsArchived:         true,
			IsShared:           true,
			IsExtShared:        true,
			IsOrgShared:        true,
			IsPendingExtShared: true,
			IsMember:           true,
		})
	})
}

const (
	methodsPageFirst  = `{"type": "message", "user": "U1", "text": "first", "ts": "1700000000.000001", "reactions": [{"name": "wave", "count": 1}]}`
	methodsPageSecond = `{"type": "message", "bot_id": "B1", "text": "second", "ts": "1700000000.000002", "thread_ts": "1700000000.000001"}`
	methodsPageBody   = `{
	"ok": true,
	"messages": [
		` + methodsPageFirst + `,
		` + methodsPageSecond + `
	],
	"has_more": true,
	"response_metadata": {"next_cursor": "bmV4dA=="}
}`
)

var methodsWantPage = &slackapi.MessagePage{
	Messages: []slackapi.Message{
		{Type: "message", User: "U1", Text: "first", TS: "1700000000.000001", Raw: json.RawMessage(methodsPageFirst)},
		{Type: "message", BotID: "B1", Text: "second", TS: "1700000000.000002", ThreadTS: "1700000000.000001", Raw: json.RawMessage(methodsPageSecond)},
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
}
