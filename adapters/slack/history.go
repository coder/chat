package slack

import (
	"context"
	"slices"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
)

// Compile-time assertion that the Slack adapter is the first HistoryReader
// (reached via Adapter Access, not the core Adapter interface).
var _ chat.HistoryReader = (*Adapter)(nil)

const (
	// slackMaxHistoryLimit is Slack's documented maximum page size for
	// conversations.history / conversations.replies.
	slackMaxHistoryLimit = 1000
	// slackDefaultHistoryLimit is the page size used when HistoryQuery.Limit <= 0.
	slackDefaultHistoryLimit = 100
)

// ReadHistory is the Slack implementation of the HistoryReader Optional Capability.
//
// It is a live platform read keyed by the opaque Thread ID: it performs NO runtime
// storage. It does not write Runtime State, does not dedupe, and does not cache.
// Stored/long-term conversation context (transcripts, LLM context windows,
// summaries, RAG corpora) is Thread Application State, owned by the application in
// its own storage keyed by Thread ID; this method is a thin live read-through only.
//
// Ordering and pagination are adapter-owned:
//   - Messages are returned newest-first. conversations.history is newest-first;
//     conversations.replies is oldest-first and puts the thread root at the start
//     of every page, so ReadHistory reverses the replies and returns the root only
//     on the page that reaches the start of the thread. That page may carry up to
//     Limit+1 messages.
//   - HistoryQuery.Before is a Message.ID (a Slack ts) returned by a prior page; it
//     pages toward older messages. It maps to Slack's latest=<ts> with
//     inclusive=false, so the cursor message itself is excluded. A Before cursor
//     at the thread root returns an empty page without a Slack call.
//   - HistoryQuery.Limit is clamped to Slack's maximum page size (1000) and defaults
//     to 100 when Limit <= 0.
//
// Read-API selection is adapter-owned:
//   - A threaded message Thread ID (channel-rooted, with a root ts) reads the
//     thread's replies via conversations.replies (channel + ts).
//   - A direct-message Thread ID reads via conversations.history (channel).
//
// Authorship is mapped by the same actorForEvent the inbound path uses, so
// bot-vs-human classification (BotKind) is faithful. In multi-tenant mode the bot
// identity is per-install and a.botUserID is empty, so a bot-authored history
// message is still classified BotBot via its bot_id/subtype but its Author.ID is
// the platform bot_id rather than the canonical per-install bot user id (inbound
// normalization rewrites that only because the webhook envelope carries it).
//
// The read goes through the adapter's slackapi client (ConversationReplies and
// ConversationHistory), so it inherits the Observation Hook (ObsAdapterCall /
// ObsRateLimit), the per-tenant token resolution (postToken), and
// context-bounded cancellation/backoff. The
// caller's context.Context bounds the read; when history must be fetched during long
// handler work, the application runs this after ack via the Ack-Then-Work /
// Detached Work Context seam. ReadHistory is never invoked on the inbound dispatch
// path.
func (a *Adapter) ReadHistory(ctx context.Context, id chat.ThreadID, q chat.HistoryQuery) ([]chat.Message, error) {
	payload, err := decodeThreadID(id)
	if err != nil {
		return nil, err
	}
	token, err := a.postToken(ctx, payload.Team)
	if err != nil {
		return nil, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = slackDefaultHistoryLimit
	}
	if limit > slackMaxHistoryLimit {
		limit = slackMaxHistoryLimit
	}

	// A direct message reads the channel history; a thread-rooted Thread ID reads
	// the thread's replies. Inclusive stays false so the Before cursor excludes
	// itself.
	api := a.api.WithToken(token)
	var page []slackapi.Message
	if payload.Direct {
		history, err := api.ConversationHistory(ctx, slackapi.ConversationHistoryRequest{
			Channel: payload.Channel,
			Latest:  q.Before,
			Limit:   limit,
		})
		if err != nil {
			return nil, err
		}
		page = history.Messages
	} else {
		if q.Before == payload.Root {
			return []chat.Message{}, nil
		}
		replies, err := api.ConversationReplies(ctx, slackapi.ConversationRepliesRequest{
			Channel: payload.Channel,
			TS:      payload.Root,
			Latest:  q.Before,
			Limit:   limit,
		})
		if err != nil {
			return nil, err
		}
		page = newestFirstReplies(replies, payload.Root)
	}

	messages := make([]chat.Message, 0, len(page))
	for _, msg := range page {
		author := slackEvent{Subtype: msg.Subtype, User: msg.User, BotID: msg.BotID}
		messages = append(messages, chat.Message{
			ID:     msg.TS,
			Text:   msg.Text,
			Author: a.actorForEvent(payload.Team, author, a.botUserID),
			Raw:    msg.Raw,
		})
	}
	return messages, nil
}

// newestFirstReplies returns the messages of a conversations.replies page
// newest-first. Slack returns the page oldest-first with the thread root at the
// start of every page, so the root is kept only when no older replies remain.
func newestFirstReplies(page *slackapi.MessagePage, root string) []slackapi.Message {
	messages := make([]slackapi.Message, 0, len(page.Messages))
	var rootMessage *slackapi.Message
	for _, msg := range slices.Backward(page.Messages) {
		if msg.TS == root {
			rootMessage = &msg
			continue
		}
		messages = append(messages, msg)
	}
	if rootMessage != nil && !page.HasMore {
		messages = append(messages, *rootMessage)
	}
	return messages
}
