package slack

import (
	"context"
	"fmt"

	"github.com/coder/chat"
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
// Ordering and pagination are adapter-owned (Slack's read APIs are newest-first):
//   - Messages are returned newest-first, as Slack returns them.
//   - HistoryQuery.Before is a Message.ID (a Slack ts) returned by a prior page; it
//     pages toward older messages. It maps to Slack's latest=<ts> with
//     inclusive=false, so the cursor message itself is excluded.
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
// Outbound calls reuse the adapter's shared callWithToken seam, so this read
// inherits the Observation Hook (ObsAdapterCall / ObsRateLimit), the per-tenant
// token resolution (postToken), and context-bounded cancellation/backoff. The
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

	// A thread-rooted Thread ID reads the thread's replies; a direct message reads
	// the channel history. Both share one request shape (TS is empty, hence omitted,
	// for the history path); inclusive defaults to false so the Before cursor excludes
	// itself.
	req := conversationsReadPayload{
		Channel: payload.Channel,
		Limit:   limit,
		Latest:  q.Before,
	}
	method := "conversations.history"
	if !payload.Direct && payload.Root != "" {
		method = "conversations.replies"
		req.TS = payload.Root
	}

	var resp conversationsHistoryResponse
	if err := a.callWithToken(ctx, token, method, req, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack: %s failed: %s", method, resp.Error)
	}

	messages := make([]chat.Message, 0, len(resp.Messages))
	for _, ev := range resp.Messages {
		messages = append(messages, chat.Message{
			ID:     ev.TS,
			Text:   ev.Text,
			Author: a.actorForEvent(payload.Team, ev, a.botUserID),
			Raw:    ev.Raw,
		})
	}
	return messages, nil
}

// conversationsReadPayload is the shared request shape for conversations.history
// and conversations.replies. TS is set only for the threaded (replies) read.
type conversationsReadPayload struct {
	Channel   string `json:"channel"`
	TS        string `json:"ts,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Inclusive bool   `json:"inclusive,omitempty"`
}

type conversationsHistoryResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// Messages decode into slackEvent so history normalization reuses the inbound
	// actor mapping (actorForEvent) and the per-message Raw capture (Platform Escape
	// Hatch) instead of duplicating the message shape.
	Messages []slackEvent `json:"messages"`
}
