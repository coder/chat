package linear

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/coder/chat"
)

// commentData is the Linear Comment webhook payload (ADR 0013), decoded as a
// Supported Platform Shape with unknown-field tolerance. Linear delivers it under
// the top-level "data" key for type=="Comment" webhooks.
type commentData struct {
	ID       string           `json:"id"`
	Body     string           `json:"body"`
	IssueID  string           `json:"issueId"`
	Issue    issueRef         `json:"issue"`
	Parent   *commentParent   `json:"parent"`
	ParentID string           `json:"parentId"`
	User     *webhookActor    `json:"user"`
	Actor    *webhookActor    `json:"actor"`
	BotActor *webhookActor    `json:"botActor"`
	Mentions []commentMention `json:"botActorMentions"`
	Raw      json.RawMessage  `json:"-"`
}

type commentParent struct {
	ID string `json:"id"`
}

type commentMention struct {
	ActorID string `json:"actorId"`
	ID      string `json:"id"`
}

// createIssueComment posts an ordinary Linear issue comment (ADR 0013). Threaded
// replies use the root comment as parent. Plain Text and Portable Markdown pass
// through unchanged; there is no Markdown conversion layer.
func (a *Adapter) createIssueComment(ctx context.Context, thread threadPayload, text string) (*chat.SentMessage, error) {
	input := map[string]any{
		"issueId": thread.Issue,
		"body":    text,
	}
	if thread.Comment != "" {
		input["parentId"] = thread.Comment
	}
	variables := map[string]any{"input": input}
	var resp graphQLResponse[commentCreateData]
	if err := a.callGraphQL(ctx, thread.Organization, `mutation CommentCreate($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id } } }`, variables, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	if !resp.Data.CommentCreate.Success || resp.Data.CommentCreate.Comment.ID == "" {
		return nil, errors.New("linear: failed to create issue comment")
	}
	id, err := encodeThreadID(thread)
	if err != nil {
		return nil, err
	}
	return &chat.SentMessage{ID: resp.Data.CommentCreate.Comment.ID, ThreadID: id, Raw: resp.Data.CommentCreate}, nil
}

type commentCreateData struct {
	CommentCreate struct {
		Success bool `json:"success"`
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	} `json:"commentCreate"`
}

// normalizeCommentEvent normalizes a Linear Comment webhook into a Message Event
// (ADR 0013). Event Identity stays keyed on the source comment id; routing flows
// through OnNewMention / OnSubscribedMessage exactly as agent-session prompts do.
// The bot's own comments are surfaced with the app actor as author so the
// runtime's Self Message filter drops them.
func (a *Adapter) normalizeCommentEvent(envelope webhookEnvelope, raw []byte, resolved resolvedInstall) (*chat.Event, bool) {
	comment := envelope.Data
	if comment == nil || comment.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear comment event", "reason", "missing comment")
		return nil, false
	}
	if envelope.OrganizationID == "" {
		a.logger.Warn("ignoring Linear comment event without organization", "comment_id", comment.ID)
		return nil, false
	}
	if resolved.tenant != "" && envelope.OrganizationID != resolved.tenant {
		a.logger.Warn(
			"ignoring Linear comment event for another organization",
			"comment_id", comment.ID,
			"organization_id", envelope.OrganizationID,
		)
		return nil, false
	}
	if envelope.OAuthClientID != "" && resolved.oauthClientID != "" && envelope.OAuthClientID != resolved.oauthClientID {
		a.logger.Warn("ignoring Linear comment event for another OAuth client", "comment_id", comment.ID)
		return nil, false
	}
	issueID := firstNonEmpty(comment.IssueID, comment.Issue.ID)
	if issueID == "" {
		a.logger.Warn("ignoring Linear comment event without issue", "comment_id", comment.ID)
		return nil, false
	}

	bot := a.mentionBotActor(resolved)
	author := commentAuthor(envelope.OrganizationID, comment)
	if author.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear comment event", "comment_id", comment.ID)
		return nil, false
	}
	// Tenant-correct self-filtering from the per-install app actor: the runtime's
	// single-valued BotActor() filter cannot match in multi-tenant mode, so drop the
	// bot's own comments here as an Ignored Event.
	if a.multiTenant() && resolved.botUserID != "" && author.BotKind == chat.BotBot && author.ID == resolved.botUserID {
		return nil, false
	}

	rootCommentID := comment.ID
	if comment.Parent != nil && comment.Parent.ID != "" {
		rootCommentID = comment.Parent.ID
	} else if comment.ParentID != "" {
		rootCommentID = comment.ParentID
	}

	threadID, err := encodeThreadID(threadPayload{
		Organization: envelope.OrganizationID,
		Issue:        issueID,
		Comment:      rootCommentID,
		Kind:         threadKindComment,
	})
	if err != nil {
		a.logger.Warn("ignoring Linear comment event with invalid thread", "error", err)
		return nil, false
	}

	rawMessage := &RawMessage{
		Kind:           threadKindComment,
		Action:         envelope.Action,
		OrganizationID: envelope.OrganizationID,
		Comment:        comment.Raw,
		Envelope:       append(json.RawMessage(nil), raw...),
	}

	return &chat.Event{
		ID:       "linear:" + envelope.OrganizationID + ":message:" + comment.ID,
		Adapter:  adapterName,
		Tenant:   envelope.OrganizationID,
		ThreadID: threadID,
		Raw:      rawMessage,
		Message: &chat.Message{
			ID:        comment.ID,
			Text:      comment.Body,
			Author:    author,
			Mentioned: a.commentMentionsBot(comment, bot),
			Raw:       rawMessage,
		},
	}, true
}

// mentionBotActor returns the app actor used for mention detection. In
// single-install mode it is the discovered BotActor (carrying the bot name for the
// textual @-mention fallback); in multi-tenant mode it is the per-install app actor
// id resolved for this webhook.
func (a *Adapter) mentionBotActor(resolved resolvedInstall) chat.Actor {
	if !a.multiTenant() {
		return a.BotActor()
	}
	return chat.Actor{Adapter: adapterName, Tenant: resolved.tenant, ID: resolved.botUserID, BotKind: chat.BotBot}
}

// commentAuthor resolves the comment author, preferring the explicit user/actor.
func commentAuthor(tenant string, comment *commentData) chat.Actor {
	switch {
	case comment.User != nil && comment.User.ID != "":
		return actorFromWebhook(tenant, *comment.User)
	case comment.Actor != nil && comment.Actor.ID != "":
		return actorFromWebhook(tenant, *comment.Actor)
	case comment.BotActor != nil && comment.BotActor.ID != "":
		return actorFromWebhook(tenant, *comment.BotActor)
	default:
		return chat.Actor{}
	}
}

// commentMentionsBot reports whether the comment mentions the app actor, driving
// OnNewMention vs OnSubscribedMessage. Structured botActorMentions are the
// authoritative app-actor mention signal; the textual @-mention fallback (by id
// or name) covers payloads that omit the structured field. Both fallbacks require
// an "@" prefix so a bare opaque id appearing elsewhere in the body cannot
// false-positive.
func (a *Adapter) commentMentionsBot(comment *commentData, bot chat.Actor) bool {
	for _, m := range comment.Mentions {
		if bot.ID != "" && (m.ActorID == bot.ID || m.ID == bot.ID) {
			return true
		}
	}
	body := strings.ToLower(comment.Body)
	if body == "" {
		return false
	}
	if bot.ID != "" && strings.Contains(body, "@"+strings.ToLower(bot.ID)) {
		return true
	}
	if bot.Name != "" && strings.Contains(body, "@"+strings.ToLower(bot.Name)) {
		return true
	}
	return false
}

// UnmarshalJSON preserves the verbatim comment payload on the escape hatch while
// decoding the typed fields.
func (c *commentData) UnmarshalJSON(data []byte) error {
	type alias commentData
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = commentData(raw)
	c.Raw = append(json.RawMessage(nil), data...)
	return nil
}
