package msteams

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/coder/chat"
)

// activity is the Supported Platform Shape of a Bot Framework Activity: only the
// fields this slice reads. Decoding is permissive -- unrelated Bot Framework fields
// are tolerated -- and the full raw JSON is preserved as the Platform Escape Hatch
// on both Event.Raw and Message.Raw.
type activity struct {
	Type         string              `json:"type"`
	ID           string              `json:"id"`
	ChannelID    string              `json:"channelId"`
	ServiceURL   string              `json:"serviceUrl"`
	Text         string              `json:"text"`
	From         channelAccount      `json:"from"`
	Recipient    channelAccount      `json:"recipient"`
	Conversation conversationAccount `json:"conversation"`
	Entities     []entity            `json:"entities"`
	Raw          json.RawMessage     `json:"-"`
}

func (a *activity) UnmarshalJSON(data []byte) error {
	type alias activity
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*a = activity(decoded)
	a.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type channelAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AADObjectID string `json:"aadObjectId"`
}

type conversationAccount struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenantId"`
	ConversationType string `json:"conversationType"`
	IsGroup          bool   `json:"isGroup"`
}

type entity struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Mentioned channelAccount `json:"mentioned"`
}

// normalizeActivity converts a decoded Activity into a runtime Event. It returns
// ok=false for the Ignored Events of this slice: any non-message activity type, and
// the bot's own messages.
//
// Self-filtering is done here (not left solely to the runtime's BotActor() match)
// because a single-install Teams bot can receive activity from more than one
// Platform Tenant, so the runtime's tenant-scoped isSelfActor cannot be relied on
// to catch the bot's own echo -- the same reason the Slack multi-tenant path drops
// self messages in the adapter.
func (a *Adapter) normalizeActivity(act activity) (*chat.Event, bool, error) {
	if !strings.EqualFold(act.Type, "message") {
		return nil, false, nil
	}
	if act.ID == "" {
		return nil, false, errors.New("msteams: activity id is required")
	}
	if act.ServiceURL == "" {
		return nil, false, errors.New("msteams: activity service url is required")
	}
	if act.Conversation.ID == "" {
		return nil, false, errors.New("msteams: activity conversation id is required")
	}
	tenantID := act.Conversation.TenantID
	if tenantID == "" {
		return nil, false, errors.New("msteams: activity conversation tenant id is required")
	}

	author := a.actorForActivity(act)
	if author.ID == "" {
		return nil, false, errors.New("msteams: activity author is required")
	}
	if author.BotKind == chat.BotBot && a.botID != "" && author.ID == a.botID {
		// The bot's own message: an Ignored Event, dropped before dispatch.
		return nil, false, nil
	}

	threadID, err := encodeThreadID(conversationReference{
		ServiceURL:       act.ServiceURL,
		ConversationID:   act.Conversation.ID,
		TenantID:         tenantID,
		BotID:            act.Recipient.ID,
		ChannelID:        act.ChannelID,
		ConversationType: act.Conversation.ConversationType,
		IsGroup:          act.Conversation.IsGroup,
	})
	if err != nil {
		return nil, false, err
	}

	direct := act.Conversation.ConversationType == "personal" ||
		(act.Conversation.ConversationType == "" && !act.Conversation.IsGroup)

	return &chat.Event{
		ID:            act.ID,
		Adapter:       adapterName,
		Tenant:        tenantID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Raw:           act.Raw,
		Message: &chat.Message{
			ID:        act.ID,
			Text:      stripBotMention(act),
			Author:    author,
			Mentioned: botMentioned(act),
			Raw:       act.Raw,
		},
	}, true, nil
}

// actorForActivity maps the inbound author. The canonical Actor.ID prefers
// from.aadObjectId (tenant-stable across conversations) and falls back to from.id;
// a self-authored message (from.id == the configured bot id) is tagged BotBot with
// from.id so it matches BotActor() for Self Message filtering. The exact canonical
// key is spike-required (ADR 0007 Open Question 10).
func (a *Adapter) actorForActivity(act activity) chat.Actor {
	id := firstNonEmpty(act.From.AADObjectID, act.From.ID)
	kind := chat.BotHuman
	if a.botID != "" && act.From.ID == a.botID {
		id = act.From.ID
		kind = chat.BotBot
	}
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  act.Conversation.TenantID,
		ID:      id,
		Name:    act.From.Name,
		BotKind: kind,
	}
}

// botMentioned reports whether the bot is @mentioned, derived from the Activity's
// mention entities (mentioned.id == the bot's recipient id), never from substring
// matching on display-name text. The inbound recipient is, by definition, the bot.
func botMentioned(act activity) bool {
	botID := act.Recipient.ID
	if botID == "" {
		return false
	}
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Mentioned.ID == botID {
			return true
		}
	}
	return false
}

// stripBotMention removes the bot mention from the message text so handlers see the
// user's actual words. Teams embeds the mention as "<at>Bot</at>" both in the text
// and as an entity carrying that exact substring; the entity text is the reliable
// thing to strip. The leading-<at> fallback applies only when the bot is mentioned
// but no entity carried text, so it never strips another user's leading mention.
// Markdown subset fidelity beyond this is spike-required (ADR 0007 Open Question 5).
func stripBotMention(act activity) string {
	text := act.Text
	botID := act.Recipient.ID
	stripped := false
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Mentioned.ID == botID && e.Text != "" {
			text = strings.ReplaceAll(text, e.Text, "")
			stripped = true
		}
	}
	if !stripped && botMentioned(act) {
		text = stripLeadingAtTag(text)
	}
	return strings.TrimSpace(text)
}

func stripLeadingAtTag(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<at>") {
		return text
	}
	end := strings.Index(trimmed, "</at>")
	if end < 0 {
		return text
	}
	return trimmed[end+len("</at>"):]
}
