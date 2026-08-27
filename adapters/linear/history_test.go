package linear_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

// historyRequest records one decoded history-related GraphQL request (a page
// read or a cursor lookup) so tests can assert Thread ID to query mapping,
// limit clamping, and cursor handling.
type historyRequest struct {
	Op        string
	Variables map[string]any
}

// historyOps are the GraphQL operation names the HistoryReader issues; the fake
// Linear API serves each from the mocked response registry.
var historyOps = []string{
	"AgentSessionHistory",
	"CommentThreadHistory",
	"HistoryActivityCursor",
	"HistoryCommentCursor",
}

// handleHistoryQuery serves the mocked GraphQL responses for history reads,
// recording every request. It supports the same blocking and throttling knobs
// the Slack history fake offers so hardening tests stay symmetric.
func (a *linearAPIServer) handleHistoryQuery(t *testing.T, w http.ResponseWriter, r *http.Request, req graphQLRequest) bool {
	t.Helper()
	op := ""
	for _, candidate := range historyOps {
		if strings.Contains(req.Query, candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return false
	}
	a.mu.Lock()
	a.historyReqs = append(a.historyReqs, historyRequest{Op: op, Variables: req.Variables})
	block := a.historyBlock
	throttled := a.historyRateLimit > a.historyRateSeen
	if throttled {
		a.historyRateSeen++
	}
	response := a.historyResponses[op]
	a.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return true
		}
	}
	if throttled {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(t, w, map[string]any{"retryAfter": "0.01"})
		return true
	}
	if response == nil {
		t.Errorf("no mocked response for history operation %s", op)
		writeJSON(t, w, map[string]any{"errors": []map[string]any{{"message": "no mocked response"}}})
		return true
	}
	writeJSON(t, w, response)
	return true
}

func (a *linearAPIServer) setHistoryResponse(op string, response map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.historyResponses == nil {
		a.historyResponses = map[string]map[string]any{}
	}
	a.historyResponses[op] = response
}

func (a *linearAPIServer) historyRequests() []historyRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]historyRequest(nil), a.historyReqs...)
}

func linearHistoryReader(t *testing.T, bot *chat.Chat) chat.HistoryReader {
	t.Helper()
	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "linear")
	if !ok {
		t.Fatal("linear adapter does not implement chat.HistoryReader")
	}
	return hr
}

// activityNode builds a mocked AgentActivity node for AgentSessionHistory
// responses (createdAt-ascending pages, as Linear returns them).
func activityNode(id, contentType, body, userID, userName string, app bool) map[string]any {
	typename := "AgentActivity" + strings.ToUpper(contentType[:1]) + contentType[1:] + "Content"
	return map[string]any{
		"id":        id,
		"createdAt": "2026-05-12T00:00:0" + id[len(id)-1:] + "Z",
		"ephemeral": false,
		"user":      map[string]any{"id": userID, "name": userName, "displayName": userName, "app": app},
		"content":   map[string]any{"__typename": typename, "type": contentType, "body": body},
	}
}

// countingState wraps a Runtime State and counts every call, so history tests
// can assert ReadHistory performs no runtime storage (ADR 0009).
type countingState struct {
	State chat.State
	calls atomic.Int64
}

func (s *countingState) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	s.calls.Add(1)
	return s.State.IsThreadSubscribed(ctx, id)
}

func (s *countingState) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	s.calls.Add(1)
	return s.State.SubscribeThread(ctx, id)
}

func (s *countingState) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	s.calls.Add(1)
	return s.State.UnsubscribeThread(ctx, id)
}

func (s *countingState) MarkEvent(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.State.MarkEvent(ctx, key, ttl)
}

func (s *countingState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	s.calls.Add(1)
	return s.State.AcquireLock(ctx, key, ttl)
}

func (s *countingState) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.State.ExtendLock(ctx, lease, ttl)
}

func (s *countingState) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	s.calls.Add(1)
	return s.State.ReleaseLock(ctx, lease)
}

func (s *countingState) Shutdown(ctx context.Context) error {
	return s.State.Shutdown(ctx)
}

func agentSessionHistoryResponse(nodes ...map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"agentSession": map[string]any{"activities": map[string]any{"nodes": nodes}}}}
}

func commentNode(id, body string, user map[string]any, botActor map[string]any) map[string]any {
	node := map[string]any{"id": id, "body": body, "createdAt": "2026-05-12T00:00:00Z"}
	if user != nil {
		node["user"] = user
	}
	if botActor != nil {
		node["botActor"] = botActor
	}
	return node
}

func commentThreadHistoryResponse(root map[string]any, hasPreviousPage bool, children ...map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{
		"root": root,
		"thread": map[string]any{"children": map[string]any{
			"nodes":    children,
			"pageInfo": map[string]any{"hasPreviousPage": hasPreviousPage},
		}},
	}}
}

// HistoryReader is detected and reached only through typed Adapter Access
// (chat.AdapterAs); an unknown adapter name resolves to the explicit
// unsupported result, never a panic or an empty history.
func TestLinearReadHistoryCapabilityDetection(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	if _, ok := chat.AdapterAs[chat.HistoryReader](bot, "linear"); !ok {
		t.Fatal("expected linear to be reachable as chat.HistoryReader")
	}
	if _, ok := chat.AdapterAs[chat.HistoryReader](bot, "nope"); ok {
		t.Fatal("unknown adapter name must not resolve a HistoryReader")
	}
}

// An agent-session Thread ID reads the session's Agent Activities and
// normalizes them newest-first: activity id as Message.ID, content body as
// Text, and authorship from the activity user with Linear's app flag driving
// BotBot vs BotHuman. History Messages are never Mentioned.
func TestLinearReadHistoryAgentSessionNormalization(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse(
		activityNode("A1", "prompt", "please fix", "U1", "User One", false),
		activityNode("A2", "thought", "thinking", "APP1", "Linear Bot", true),
	))
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 20})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	reqs := api.historyRequests()
	if len(reqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(reqs))
	}
	if reqs[0].Op != "AgentSessionHistory" {
		t.Fatalf("op = %q, want AgentSessionHistory", reqs[0].Op)
	}
	if got := reqs[0].Variables["session"]; got != "S1" {
		t.Fatalf("session variable = %v, want S1", got)
	}
	if got := reqs[0].Variables["last"]; got != float64(20) {
		t.Fatalf("last variable = %v, want 20", got)
	}
	if _, ok := reqs[0].Variables["filter"]; ok {
		t.Fatalf("first page carried a filter: %v", reqs[0].Variables["filter"])
	}

	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	// Newest-first: the page arrives createdAt-ascending and is reversed.
	if msgs[0].ID != "A2" || msgs[0].Text != "thinking" {
		t.Fatalf("msg0 = %#v, want newest activity A2", msgs[0])
	}
	if msgs[0].Author.ID != "APP1" || msgs[0].Author.BotKind != chat.BotBot {
		t.Fatalf("msg0 author = %#v, want app actor BotBot", msgs[0].Author)
	}
	if msgs[1].ID != "A1" || msgs[1].Text != "please fix" {
		t.Fatalf("msg1 = %#v, want prompt A1", msgs[1])
	}
	if msgs[1].Author.ID != "U1" || msgs[1].Author.BotKind != chat.BotHuman {
		t.Fatalf("msg1 author = %#v, want human U1", msgs[1].Author)
	}
	for i, msg := range msgs {
		if msg.Author.Adapter != "linear" || msg.Author.Tenant != "ORG1" {
			t.Fatalf("msg[%d] author scope = %#v", i, msg.Author)
		}
		// History is not a routing surface: Messages are never Mentioned.
		if msg.Mentioned {
			t.Fatalf("msg[%d].Mentioned = true; history Messages must never be Mentioned", i)
		}
	}
}

// Each returned Message preserves the verbatim per-node JSON via the Platform
// Escape Hatch (Message.Raw), including fields the normalized shape drops
// (signal, action content fields).
func TestLinearReadHistoryPreservesRaw(t *testing.T) {
	t.Parallel()

	action := map[string]any{
		"id":        "A3",
		"createdAt": "2026-05-12T00:00:03Z",
		"ephemeral": true,
		"signal":    "stop",
		"user":      map[string]any{"id": "APP1", "name": "Linear Bot", "displayName": "Linear Bot", "app": true},
		"content":   map[string]any{"__typename": "AgentActivityActionContent", "type": "action", "action": "grep", "parameter": "-r foo", "result": "3 hits"},
	}
	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse(action))
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	// Action content has no body: normalized Text stays empty, Raw carries it.
	if msgs[0].Text != "" {
		t.Fatalf("action activity text = %q, want empty", msgs[0].Text)
	}
	raw, ok := msgs[0].Raw.(json.RawMessage)
	if !ok {
		t.Fatalf("Raw type = %T, want json.RawMessage", msgs[0].Raw)
	}
	var decoded struct {
		Signal  string `json:"signal"`
		Content struct {
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if decoded.Signal != "stop" || decoded.Content.Action != "grep" || decoded.Content.Result != "3 hits" {
		t.Fatalf("raw did not preserve the verbatim node: %s", raw)
	}
}

// Limit clamping is adapter-owned: above Linear's max it clamps to 250, and
// <= 0 uses Linear's connection default (50).
func TestLinearReadHistoryClampsLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		limit     int
		wantLimit float64
	}{
		{"above-max", 5000, 250},
		{"zero-default", 0, 50},
		{"negative-default", -3, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := newLinearAPIServer(t, 3600)
			api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse())
			bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
			hr := linearHistoryReader(t, bot)
			id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
			if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: tc.limit}); err != nil {
				t.Fatalf("read history: %v", err)
			}
			reqs := api.historyRequests()
			if len(reqs) != 1 {
				t.Fatalf("history requests = %d, want 1", len(reqs))
			}
			if got := reqs[0].Variables["last"]; got != tc.wantLimit {
				t.Fatalf("last = %v, want %v", got, tc.wantLimit)
			}
		})
	}
}

// The Before cursor (a Message.ID) resolves to the cursor activity's createdAt
// (one extra lookup) and pages toward older activities with a strict
// createdAt less-than filter, excluding the cursor itself.
func TestLinearReadHistoryAgentSessionBeforeCursor(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("HistoryActivityCursor", map[string]any{"data": map[string]any{"agentActivity": map[string]any{"createdAt": "2026-05-12T00:00:01Z"}}})
	api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse())
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 10, Before: "A1"}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	reqs := api.historyRequests()
	if len(reqs) != 2 {
		t.Fatalf("history requests = %d, want 2 (cursor lookup + page)", len(reqs))
	}
	if reqs[0].Op != "HistoryActivityCursor" {
		t.Fatalf("first op = %q, want HistoryActivityCursor", reqs[0].Op)
	}
	if got := reqs[0].Variables["id"]; got != "A1" {
		t.Fatalf("cursor id = %v, want A1", got)
	}
	if reqs[1].Op != "AgentSessionHistory" {
		t.Fatalf("second op = %q, want AgentSessionHistory", reqs[1].Op)
	}
	filter, _ := reqs[1].Variables["filter"].(map[string]any)
	createdAt, _ := filter["createdAt"].(map[string]any)
	if createdAt["lt"] != "2026-05-12T00:00:01Z" {
		t.Fatalf("filter = %v, want createdAt.lt = cursor createdAt", reqs[1].Variables["filter"])
	}
}

// An issue-comment Thread ID reads the root comment and its reply children.
// The page is newest-first and the root comment closes the oldest page (no
// older replies remain), mirroring Slack's replies read including the root.
func TestLinearReadHistoryCommentThreadRootAndReplies(t *testing.T) {
	t.Parallel()

	human := map[string]any{"id": "U1", "name": "User One", "displayName": "User One", "app": false}
	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("CommentThreadHistory", commentThreadHistoryResponse(
		commentNode("C1", "root question", human, nil),
		false,
		commentNode("R1", "older reply", human, nil),
		commentNode("R2", "newer reply", nil, map[string]any{"id": "BOT9", "name": "Other Bot"}),
	))
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeCommentThreadIDForTest("ORG1", "ISSUE1", "C1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	reqs := api.historyRequests()
	if len(reqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(reqs))
	}
	if reqs[0].Op != "CommentThreadHistory" {
		t.Fatalf("op = %q, want CommentThreadHistory", reqs[0].Op)
	}
	if got := reqs[0].Variables["comment"]; got != "C1" {
		t.Fatalf("comment variable = %v, want C1", got)
	}

	wantIDs := []string{"R2", "R1", "C1"}
	if len(msgs) != len(wantIDs) {
		t.Fatalf("messages = %d, want %d", len(msgs), len(wantIDs))
	}
	for i, want := range wantIDs {
		if msgs[i].ID != want {
			t.Fatalf("msg[%d].ID = %q, want %q (newest-first, root closes the page)", i, msgs[i].ID, want)
		}
	}
	// A bot-actor comment normalizes to BotBot; user comments carry the app flag.
	if msgs[0].Author.ID != "BOT9" || msgs[0].Author.BotKind != chat.BotBot {
		t.Fatalf("bot reply author = %#v, want BotBot BOT9", msgs[0].Author)
	}
	if msgs[2].Author.ID != "U1" || msgs[2].Author.BotKind != chat.BotHuman {
		t.Fatalf("root author = %#v, want human U1", msgs[2].Author)
	}
	// Raw preserves the verbatim comment node.
	raw, ok := msgs[2].Raw.(json.RawMessage)
	if !ok {
		t.Fatalf("Raw type = %T, want json.RawMessage", msgs[2].Raw)
	}
	var decoded struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if decoded.Body != "root question" {
		t.Fatalf("raw = %s, want verbatim root comment", raw)
	}
}

// Comment-thread pagination: a Before cursor resolves the cursor comment's
// createdAt and filters older replies; while older replies remain the root is
// NOT appended, and paging before the root itself is an empty page with no
// platform call.
func TestLinearReadHistoryCommentThreadPagination(t *testing.T) {
	t.Parallel()

	human := map[string]any{"id": "U1", "name": "User One", "displayName": "User One", "app": false}
	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("HistoryCommentCursor", map[string]any{"data": map[string]any{"comment": map[string]any{"createdAt": "2026-05-12T00:00:05Z"}}})
	api.setHistoryResponse("CommentThreadHistory", commentThreadHistoryResponse(
		commentNode("C1", "root question", human, nil),
		true,
		commentNode("R1", "older reply", human, nil),
	))
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeCommentThreadIDForTest("ORG1", "ISSUE1", "C1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 1, Before: "R5"})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	reqs := api.historyRequests()
	if len(reqs) != 2 {
		t.Fatalf("history requests = %d, want 2 (cursor lookup + page)", len(reqs))
	}
	if reqs[0].Op != "HistoryCommentCursor" || reqs[0].Variables["id"] != "R5" {
		t.Fatalf("cursor lookup = %+v, want HistoryCommentCursor for R5", reqs[0])
	}
	filter, _ := reqs[1].Variables["filter"].(map[string]any)
	createdAt, _ := filter["createdAt"].(map[string]any)
	if createdAt["lt"] != "2026-05-12T00:00:05Z" {
		t.Fatalf("filter = %v, want createdAt.lt = cursor createdAt", reqs[1].Variables["filter"])
	}
	// Older replies remain (hasPreviousPage): the root must not be appended yet.
	if len(msgs) != 1 || msgs[0].ID != "R1" {
		t.Fatalf("messages = %#v, want only R1", msgs)
	}

	// Paging before the root comment is the end of history: an empty page and no
	// further platform calls.
	msgs, err = hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Before: "C1"})
	if err != nil {
		t.Fatalf("read history past root: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages past root = %#v, want empty page", msgs)
	}
	if got := len(api.historyRequests()); got != 2 {
		t.Fatalf("history requests = %d, want 2 (no platform call past the root)", got)
	}
}

// A malformed Thread ID returns the decode error before any platform call, and
// a thread scoped to another organization fails the single-install tenant
// guard, never silently reading across orgs.
func TestLinearReadHistoryThreadValidation(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	if msgs, err := hr.ReadHistory(context.Background(), chat.ThreadID("not-a-linear-id"), chat.HistoryQuery{}); err == nil || msgs != nil {
		t.Fatalf("malformed id: msgs = %#v, err = %v; want decode error and nil messages", msgs, err)
	}
	otherOrg := linear.EncodeAgentSessionThreadIDForTest("ORG2", "ISSUE1", "S1")
	if _, err := hr.ReadHistory(context.Background(), otherOrg, chat.HistoryQuery{}); err == nil || !strings.Contains(err.Error(), "organization") {
		t.Fatalf("cross-org read error = %v, want tenant-guard error", err)
	}
	if got := len(api.historyRequests()); got != 0 {
		t.Fatalf("history requests = %d, want 0 (validation precedes platform calls)", got)
	}
}

// A GraphQL errors payload surfaces as an error, never a silent empty slice.
func TestLinearReadHistoryAPIErrorSurfaces(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("AgentSessionHistory", map[string]any{"errors": []map[string]any{{"message": "boom"}}})
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want surfaced graphql error", err)
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on error", msgs)
	}
}

// ReadHistory performs no runtime storage (ADR 0009): a full read drives zero
// Runtime State calls, proving the seam stays a live read-through and never
// becomes a hidden message store.
func TestLinearReadHistoryWritesNoRuntimeState(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse(
		activityNode("A1", "prompt", "please fix", "U1", "User One", false),
	))
	adapter, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "client", ClientSecret: "secret"},
		APIBaseURL:        api.URL,
		Client:            api.Client(),
	})
	if err != nil {
		t.Fatalf("new linear adapter: %v", err)
	}
	state := &countingState{State: memory.New()}
	bot, err := chat.New(context.Background(), chat.WithState(state), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if got := state.calls.Load(); got != 0 {
		t.Fatalf("runtime state calls during ReadHistory = %d, want 0", got)
	}
}

// A history read inherits the adapter Observation Hook (ADR 0010): the shared
// GraphQL seam emits ObsAdapterCall around the platform read.
func TestLinearReadHistoryEmitsAdapterCallObservation(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.setHistoryResponse("AgentSessionHistory", agentSessionHistoryResponse())
	obs := &rlCountingObserver{}
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Observer: obs})
	hr := linearHistoryReader(t, bot)

	before := obs.count(chat.ObsAdapterCall)
	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if got := obs.count(chat.ObsAdapterCall); got <= before {
		t.Fatalf("ObsAdapterCall = %d, want > %d (one per read attempt)", got, before)
	}
}

// A persistent 429 on a history read exhausts the adapter-owned bounded retry
// (ADR 0005) and surfaces a typed *linear.RateLimited, with the throttled
// attempt emitting ObsRateLimit through the shared seam. MaxAttempts: 1 keeps
// the assertion deterministic and single-shot.
func TestLinearReadHistoryRateLimitObservedAndErrors(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.historyRateLimit = 1
	obs := &rlCountingObserver{}
	bot, _ := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Observer:      obs,
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 1},
	})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{})
	var limited *linear.RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *linear.RateLimited on a throttled read", err)
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on rate-limit error", msgs)
	}
	if obs.count(chat.ObsRateLimit) == 0 {
		t.Fatal("expected ObsRateLimit observation on 429 read")
	}
	if got := len(api.historyRequests()); got != 1 {
		t.Fatalf("read calls = %d, want 1 (MaxAttempts: 1 is single-shot)", got)
	}
}

// A read whose caller context carries a deadline cannot outlive it: when the
// platform read blocks, ReadHistory returns promptly with the context error
// (ADR 0005: read backoff is bounded by the caller's context).
func TestLinearReadHistoryDeadlineBounded(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.historyBlock = make(chan struct{}) // never closed: the server blocks until ctx fires
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec"})
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := hr.ReadHistory(ctx, id, chat.HistoryQuery{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read outlived its caller deadline: did not return promptly")
	}
}

// In multi-tenant mode the read resolves the per-org access token from the
// Thread ID's organization via the InstallStore (reusing resolveToken), proving
// no new credential plumbing.
func TestLinearReadHistoryMultiTenantToken(t *testing.T) {
	t.Parallel()

	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
	store.set("ORG2", chat.Install{Tenant: "ORG2", Credential: linear.LinearInstall{WebhookSecret: "whsec-2", AccessToken: "tok-ORG2"}})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	hr := linearHistoryReader(t, bot)

	id := linear.EncodeAgentSessionThreadIDForTest("ORG2", "ISSUE9", "S9")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	auths := api.historyAuths()
	if len(auths) != 1 || auths[0] != "Bearer tok-ORG2" {
		t.Fatalf("history authorization = %v, want [Bearer tok-ORG2]", auths)
	}
}
