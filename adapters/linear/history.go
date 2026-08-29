package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/coder/chat"
)

// Compile-time assertion that the Linear adapter implements the HistoryReader
// Optional Capability (reached via Adapter Access, not the core Adapter
// interface).
var _ chat.HistoryReader = (*Adapter)(nil)

const (
	// linearMaxHistoryLimit is Linear's maximum GraphQL connection page size for
	// first/last pagination arguments.
	linearMaxHistoryLimit = 250
	// linearDefaultHistoryLimit is the page size used when HistoryQuery.Limit <= 0,
	// matching Linear's own connection default.
	linearDefaultHistoryLimit = 50
)

// ReadHistory is the Linear implementation of the HistoryReader Optional
// Capability (ADR 0009).
//
// It is a live platform read keyed by the opaque Thread ID: it performs NO
// runtime storage. It does not write Runtime State, does not dedupe, and does not
// cache. Stored/long-term conversation context (transcripts, LLM context windows,
// summaries, RAG corpora) is Thread Application State, owned by the application
// in its own storage keyed by Thread ID; this method is a thin live read-through
// only.
//
// Read-API selection is adapter-owned, keyed by the thread-kind discriminator
// inside the opaque Thread ID:
//
//   - An agent-session Thread ID (ADR 0008) reads the session's Agent Activities
//     (agentSession.activities), Linear's canonical frozen-in-time conversation
//     record: user prompts, thoughts, actions, elicitations, responses, and
//     errors. The session's opening comment / promptContext is not an activity
//     and is not replayed here; it arrives on the "created" Message Event.
//   - An issue-comment Thread ID (ADR 0013) reads the thread's root comment and
//     its reply children (comment.children). The root comment is part of the
//     thread: it closes the oldest page, so a page may carry up to Limit+1
//     messages when it reaches the beginning of the thread.
//
// Ordering and pagination are adapter-owned (Linear connections are
// createdAt-ascending; this read returns each page newest-first, mirroring the
// Slack HistoryReader):
//
//   - Messages are returned newest-first within each page.
//   - HistoryQuery.Before is a Message.ID returned by a prior page; it pages
//     toward older messages and excludes the cursor message itself. Because
//     Linear's Relay cursors are not node ids, the cursor message's createdAt is
//     resolved first (one extra GraphQL lookup) and the page is filtered with a
//     strict createdAt less-than; messages sharing the cursor's exact createdAt
//     are excluded with it. Passing the root comment's ID as Before returns an
//     empty page (the end of a comment thread's history) without a platform call.
//   - HistoryQuery.Limit is clamped to Linear's maximum page size (250) and
//     defaults to 50 when Limit <= 0.
//
// Authorship is normalized from the node's author: Linear app users (User.app,
// e.g. agents) and bot actors map to BotBot, everyone else to BotHuman. History
// Messages are never Mentioned; history is not a routing surface. Each Message
// preserves its verbatim node JSON via the Platform Escape Hatch (Message.Raw).
//
// Every read goes through the adapter's shared GraphQL seam (callGraphQL), so it
// inherits per-tenant token resolution (single-install lazy refresh or
// multi-tenant InstallStore lookup keyed by the thread's organization), the
// bounded rate-limit retry policy (ADR 0005), and the Observation Hook
// (ObsAdapterCall / ObsRateLimit, ADR 0010). The caller's context.Context bounds
// the read; when history must be fetched during long handler work, the
// application runs this after ack via the Ack-Then-Work / Detached Work Context
// seam (ADR 0002). ReadHistory is never invoked on the inbound dispatch path.
func (a *Adapter) ReadHistory(ctx context.Context, id chat.ThreadID, q chat.HistoryQuery) ([]chat.Message, error) {
	assertAdapter(a)
	payload, err := a.validateThreadPayload(id)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = linearDefaultHistoryLimit
	}
	if limit > linearMaxHistoryLimit {
		limit = linearMaxHistoryLimit
	}
	switch payload.kind() {
	case threadKindComment:
		return a.readCommentThreadHistory(ctx, payload, q.Before, limit)
	default:
		return a.readAgentSessionHistory(ctx, payload, q.Before, limit)
	}
}

// agentSessionHistoryQuery pages an agent session's activities. The content
// union carries per-type fields via inline fragments; every member exposes the
// shared type discriminator, matching the webhook content shape.
const agentSessionHistoryQuery = `query AgentSessionHistory($session: String!, $last: Int!, $filter: AgentActivityFilter) { agentSession(id: $session) { activities(last: $last, filter: $filter) { nodes { id createdAt ephemeral signal signalMetadata sourceComment { id } user { id name displayName app } content { __typename ... on AgentActivityPromptContent { type body } ... on AgentActivityThoughtContent { type body } ... on AgentActivityResponseContent { type body } ... on AgentActivityElicitationContent { type body } ... on AgentActivityErrorContent { type body } ... on AgentActivityActionContent { type action parameter result } } } } } }`

func (a *Adapter) readAgentSessionHistory(ctx context.Context, thread threadPayload, before string, limit int) ([]chat.Message, error) {
	variables := map[string]any{"session": thread.Session, "last": limit}
	if before != "" {
		filter, err := a.historyCursorFilter(ctx, thread.Organization,
			`query HistoryActivityCursor($id: String!) { agentActivity(id: $id) { createdAt } }`, before, "agentActivity")
		if err != nil {
			return nil, err
		}
		variables["filter"] = filter
	}
	var resp graphQLResponse[agentSessionHistoryData]
	if err := a.callGraphQL(ctx, thread.Organization, agentSessionHistoryQuery, variables, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	nodes := resp.Data.AgentSession.Activities.Nodes
	messages := make([]chat.Message, 0, len(nodes))
	// Linear returns the page createdAt-ascending; reverse for newest-first.
	for _, node := range slices.Backward(nodes) {
		messages = append(messages, chat.Message{
			ID:     node.ID,
			Text:   node.Content.Body,
			Author: historyUserActor(thread.Organization, node.User),
			Raw:    node.Raw,
		})
	}
	return messages, nil
}

// commentThreadHistoryQuery reads a comment thread's root and one page of its
// reply children in a single request. The two aliases resolve the same root
// comment so each returned Message.Raw stays a clean per-node payload.
const commentThreadHistoryQuery = `query CommentThreadHistory($comment: String!, $last: Int!, $filter: CommentFilter) { root: comment(id: $comment) { id body createdAt user { id name displayName app } botActor { id name userDisplayName } } thread: comment(id: $comment) { children(last: $last, filter: $filter) { nodes { id body createdAt user { id name displayName app } botActor { id name userDisplayName } } pageInfo { hasPreviousPage } } } }`

func (a *Adapter) readCommentThreadHistory(ctx context.Context, thread threadPayload, before string, limit int) ([]chat.Message, error) {
	// The root comment is the thread's oldest message: paging before it is the
	// explicit end of history, an empty page rather than a platform call.
	if before == thread.Comment {
		return []chat.Message{}, nil
	}
	variables := map[string]any{"comment": thread.Comment, "last": limit}
	if before != "" {
		filter, err := a.historyCursorFilter(ctx, thread.Organization,
			`query HistoryCommentCursor($id: String!) { comment(id: $id) { createdAt } }`, before, "comment")
		if err != nil {
			return nil, err
		}
		variables["filter"] = filter
	}
	var resp graphQLResponse[commentThreadHistoryData]
	if err := a.callGraphQL(ctx, thread.Organization, commentThreadHistoryQuery, variables, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	if resp.Data.Root.ID == "" {
		return nil, fmt.Errorf("linear: history root comment %q not found", thread.Comment)
	}
	children := resp.Data.Thread.Children.Nodes
	messages := make([]chat.Message, 0, len(children)+1)
	// Linear returns the page createdAt-ascending; reverse for newest-first.
	for _, c := range slices.Backward(children) {
		messages = append(messages, historyCommentMessage(thread.Organization, c))
	}
	// No older replies remain, so the root comment closes the oldest page.
	if !resp.Data.Thread.Children.PageInfo.HasPreviousPage {
		messages = append(messages, historyCommentMessage(thread.Organization, resp.Data.Root))
	}
	return messages, nil
}

// historyCursorFilter resolves a Before cursor (a Message.ID from a prior page)
// into a createdAt pagination filter. Linear's Relay cursors are not node ids,
// so the cursor node's createdAt is looked up first and the next page is
// filtered with a strict less-than, excluding the cursor message itself.
func (a *Adapter) historyCursorFilter(ctx context.Context, tenant, query, id, node string) (map[string]any, error) {
	var resp graphQLResponse[map[string]historyCursorNode]
	if err := a.callGraphQL(ctx, tenant, query, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	createdAt := resp.Data[node].CreatedAt
	if createdAt == "" {
		return nil, fmt.Errorf("linear: history cursor %q did not resolve", id)
	}
	return map[string]any{"createdAt": map[string]any{"lt": createdAt}}, nil
}

type historyCursorNode struct {
	CreatedAt string `json:"createdAt"`
}

type agentSessionHistoryData struct {
	AgentSession struct {
		Activities struct {
			Nodes []historyActivity `json:"nodes"`
		} `json:"activities"`
	} `json:"agentSession"`
}

// historyActivity is one AgentActivity node from a history read, decoded as a
// Supported Platform Shape with the verbatim node JSON preserved for the
// Platform Escape Hatch (Message.Raw). Content decodes the shared type/body
// discriminator; action-content fields stay reachable through Raw.
type historyActivity struct {
	ID      string               `json:"id"`
	Content agentActivityContent `json:"content"`
	User    historyUser          `json:"user"`
	Raw     json.RawMessage      `json:"-"`
}

// UnmarshalJSON preserves the verbatim activity node on the escape hatch while
// decoding the typed fields.
func (n *historyActivity) UnmarshalJSON(data []byte) error {
	type alias historyActivity
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*n = historyActivity(raw)
	n.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type commentThreadHistoryData struct {
	Root   historyComment `json:"root"`
	Thread struct {
		Children struct {
			Nodes    []historyComment `json:"nodes"`
			PageInfo struct {
				HasPreviousPage bool `json:"hasPreviousPage"`
			} `json:"pageInfo"`
		} `json:"children"`
	} `json:"thread"`
}

// historyComment is one Comment node from a history read, with the verbatim
// node JSON preserved for the Platform Escape Hatch (Message.Raw).
type historyComment struct {
	ID       string           `json:"id"`
	Body     string           `json:"body"`
	User     *historyUser     `json:"user"`
	BotActor *historyBotActor `json:"botActor"`
	Raw      json.RawMessage  `json:"-"`
}

// UnmarshalJSON preserves the verbatim comment node on the escape hatch while
// decoding the typed fields.
func (c *historyComment) UnmarshalJSON(data []byte) error {
	type alias historyComment
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = historyComment(raw)
	c.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// historyUser is the Linear User shape selected by history reads. App is
// Linear's app-user flag, distinguishing app actors (agents) from humans.
type historyUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	App         bool   `json:"app"`
}

// historyBotActor is the Linear ActorBot shape selected by history reads.
type historyBotActor struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	UserDisplayName string `json:"userDisplayName"`
}

func historyUserActor(tenant string, user historyUser) chat.Actor {
	kind := chat.BotHuman
	if user.App {
		kind = chat.BotBot
	}
	return chat.Actor{Adapter: adapterName, Tenant: tenant, ID: user.ID, Name: firstNonEmpty(user.DisplayName, user.Name), BotKind: kind}
}

func historyCommentMessage(tenant string, comment historyComment) chat.Message {
	return chat.Message{
		ID:     comment.ID,
		Text:   comment.Body,
		Author: historyCommentActor(tenant, comment),
		Raw:    comment.Raw,
	}
}

// historyCommentActor resolves a history comment's author with the same
// preference order as inbound comment normalization: the explicit user first
// (its app flag classifies app actors as BotBot), then the bot actor.
func historyCommentActor(tenant string, comment historyComment) chat.Actor {
	switch {
	case comment.User != nil && comment.User.ID != "":
		return historyUserActor(tenant, *comment.User)
	case comment.BotActor != nil && comment.BotActor.ID != "":
		return chat.Actor{Adapter: adapterName, Tenant: tenant, ID: comment.BotActor.ID, Name: firstNonEmpty(comment.BotActor.Name, comment.BotActor.UserDisplayName), BotKind: chat.BotBot}
	default:
		return chat.Actor{}
	}
}
