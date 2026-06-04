package msteams

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/coder/chat"
)

// activity is the supported subset of a Bot Framework Activity; decoding is
// permissive and the full raw JSON is preserved as the Platform Escape Hatch.
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

// normalizeActivity converts a message Activity into an Event, returning ok=false
// for Ignored Events (non-message types and the bot's own messages). The self-drop
// here is the authoritative Self Message filter: a single-install bot can span
// tenants, so the runtime's tenant-scoped isSelfActor cannot catch its echo. It
// relies on a.botID being the bot's true id (spike-required, Open Question 10).
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

	ref := conversationReference{
		ServiceURL:       act.ServiceURL,
		ConversationID:   act.Conversation.ID,
		TenantID:         tenantID,
		BotID:            act.Recipient.ID,
		ChannelID:        act.ChannelID,
		ConversationType: act.Conversation.ConversationType,
		IsGroup:          act.Conversation.IsGroup,
	}
	threadID, err := encodeThreadID(ref)
	if err != nil {
		return nil, false, err
	}

	mentioned := botMentioned(act)
	return &chat.Event{
		ID:            act.ID,
		Adapter:       adapterName,
		Tenant:        tenantID,
		ThreadID:      threadID,
		DirectMessage: ref.direct(),
		Raw:           act.Raw,
		Message: &chat.Message{
			ID:        act.ID,
			Text:      stripBotMention(act, mentioned),
			Author:    author,
			Mentioned: mentioned,
			Raw:       act.Raw,
		},
	}, true, nil
}

// actorForActivity maps the inbound author: Actor.ID prefers the tenant-stable
// from.aadObjectId, except a self-authored message keeps from.id so it matches
// a.botID for self-filtering (canonical key spike-required, Open Question 10).
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

// botMentioned reports whether the bot is @mentioned, from the Activity's mention
// entities (the inbound recipient is the bot), never from substring text matching.
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

// stripBotMention removes the bot's mention so handlers see the user's words: it
// deletes the mention entity's exact text (first occurrence only), falling back to a
// leading <at>...</at> only when the bot is mentioned but the entity carried no text.
func stripBotMention(act activity, mentioned bool) string {
	text := act.Text
	botID := act.Recipient.ID
	stripped := false
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Mentioned.ID == botID && e.Text != "" {
			text = strings.Replace(text, e.Text, "", 1)
			stripped = true
		}
	}
	if !stripped && mentioned {
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
