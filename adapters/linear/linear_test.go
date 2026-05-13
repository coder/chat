package linear_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

func TestConstructionValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := linear.New(ctx, linear.Options{ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"}}); err == nil {
		t.Fatal("expected missing webhook secret to fail")
	}
	if _, err := linear.New(ctx, linear.Options{WebhookSecret: "whsec", ClientCredentials: linear.ClientCredentials{ClientSecret: "secret"}}); err == nil {
		t.Fatal("expected missing client id to fail")
	}
	if _, err := linear.New(ctx, linear.Options{WebhookSecret: "whsec", ClientCredentials: linear.ClientCredentials{ClientID: "id"}}); err == nil {
		t.Fatal("expected missing client secret to fail")
	}
	if _, err := linear.New(ctx, linear.Options{WebhookSecret: "whsec", ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"}, SignatureTolerance: -time.Second}); err == nil {
		t.Fatal("expected negative signature tolerance to fail")
	}
}

func TestInitRejectsMissingGrantedScopes(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.tokenScope = "read write app:mentionable"
	adapter, err := linear.New(context.Background(), linear.Options{
		WebhookSecret: "whsec",
		ClientCredentials: linear.ClientCredentials{
			ClientID:     "client",
			ClientSecret: "secret",
		},
		APIBaseURL: api.URL,
		Client:     api.Client(),
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err == nil || !strings.Contains(err.Error(), "app:assignable") {
		t.Fatalf("init err = %v, want missing app:assignable", err)
	}
}

func TestWebhookVerificationAndIgnoredEvents(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	body := []byte(fmt.Sprintf(`{"type":"Other","webhookTimestamp":%d}`, now.UnixMilli()))
	req := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(body))
	signLinearRequest(req, "whsec", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ignored event status = %d, body = %s", rec.Code, rec.Body.String())
	}

	badReq := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(body))
	badReq.Header.Set("Linear-Signature", "bad")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d", badRec.Code)
	}

	old := []byte(fmt.Sprintf(`{"type":"Other","webhookTimestamp":%d}`, now.Add(-2*time.Minute).UnixMilli()))
	oldReq := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(old))
	signLinearRequest(oldReq, "whsec", old)
	oldRec := httptest.NewRecorder()
	handler.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusUnauthorized {
		t.Fatalf("old timestamp status = %d", oldRec.Code)
	}

	malformed := []byte(`{"type":`)
	malformedReq := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(malformed))
	signLinearRequest(malformedReq, "whsec", malformed)
	malformedRec := httptest.NewRecorder()
	handler.ServeHTTP(malformedRec, malformedReq)
	if malformedRec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d", malformedRec.Code)
	}

	unbuildable := []byte(fmt.Sprintf(`{"type":"AgentSessionEvent","action":"created","organizationId":"ORG1","webhookTimestamp":%d,"agentSession":{"id":"S1","appUserId":"APP1"}}`, now.UnixMilli()))
	unbuildableReq := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(unbuildable))
	signLinearRequest(unbuildableReq, "whsec", unbuildable)
	unbuildableRec := httptest.NewRecorder()
	handler.ServeHTTP(unbuildableRec, unbuildableReq)
	if unbuildableRec.Code != http.StatusOK {
		t.Fatalf("unbuildable status = %d", unbuildableRec.Code)
	}
}

func TestAgentSessionRoutingDedupeSelfAndThreadReconstruction(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var seen []string
	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		seen = append(seen, "new:"+ev.Event.ID+":"+ev.Message.ID+":"+ev.Message.Author.Name)
		threadID = ev.Thread.ID()
		if !strings.HasPrefix(string(threadID), "linear:v1:") {
			t.Fatalf("thread id = %q", threadID)
		}
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		seen = append(seen, "subscribed:"+ev.Event.ID+":"+ev.Message.ID)
		return nil
	})

	delegated := delegatedPayload(now, "S-delegated", "<issue><title>Assigned task</title></issue>")
	postLinearEvent(t, bot, "whsec", delegated)

	created := createdPayload(now, "C1", "hello", "U1", "User One", "APP1")
	postLinearEvent(t, bot, "whsec", created)
	postLinearEvent(t, bot, "whsec", created)
	prompted := promptedPayload(now, "C2", "follow up", "U1", "User One")
	postLinearEvent(t, bot, "whsec", prompted)
	otherApp := createdPayload(now, "C4", "other app", "U2", "User Two", "OTHERAPP")
	postLinearEvent(t, bot, "whsec", otherApp)

	self := createdPayloadWithActorType(now, "C3", "self", "APP1", "Linear Bot", "APP1", "bot")
	postLinearEvent(t, bot, "whsec", self)

	want := []string{
		"new:linear:ORG1:message:S-delegated:S-delegated:",
		"new:linear:ORG1:message:C1:C1:User One",
		"subscribed:linear:ORG1:message:C2:C2",
	}
	if !equalStrings(seen, want) {
		t.Fatalf("seen = %#v, want %#v", seen, want)
	}

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("background result")); err != nil {
		t.Fatalf("thread post: %v", err)
	}
	api.assertActivity(t, 0, linearActivity{AgentSessionID: "S1", Ephemeral: false, Content: activityContent{Type: "response", Body: "background result"}})
}

func TestPostingResponseThoughtMarkdownAndLazyRefresh(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 1)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	botActor := adapter.BotActor()
	if botActor != (chat.Actor{Adapter: "linear", Tenant: "ORG1", ID: "APP1", Name: "Linear Bot", BotKind: chat.BotBot}) {
		t.Fatalf("bot actor = %#v", botActor)
	}

	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})
	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	sent, err := thread.Post(context.Background(), chat.Markdown("**final**"))
	if err != nil {
		t.Fatalf("markdown post: %v", err)
	}
	if sent == nil || sent.ID == "" || sent.ThreadID != threadID {
		t.Fatalf("sent = %#v", sent)
	}
	thought, err := adapter.PostThought(context.Background(), threadID, "Thinking...")
	if err != nil {
		t.Fatalf("thought: %v", err)
	}
	if thought == nil || thought.ID == "" || thought.ThreadID != threadID {
		t.Fatalf("thought = %#v", thought)
	}
	if _, err := adapter.PostThought(context.Background(), threadID, ""); err == nil {
		t.Fatal("expected empty thought to fail")
	}

	api.assertActivity(t, 0, linearActivity{AgentSessionID: "S1", Ephemeral: false, Content: activityContent{Type: "response", Body: "**final**"}})
	api.assertActivity(t, 1, linearActivity{AgentSessionID: "S1", Ephemeral: true, Content: activityContent{Type: "thought", Body: "Thinking..."}})
	if api.tokenRequests() < 2 {
		t.Fatalf("token requests = %d, want refresh before API calls", api.tokenRequests())
	}
}

func newLinearRuntime(t *testing.T, api *linearAPIServer, opts linear.Options) (*chat.Chat, *linear.Adapter) {
	t.Helper()
	opts.APIBaseURL = api.URL
	opts.Client = api.Client()
	if opts.ClientCredentials.ClientID == "" {
		opts.ClientCredentials = linear.ClientCredentials{ClientID: "client", ClientSecret: "secret"}
	}
	adapter, err := linear.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("new linear adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return bot, adapter
}

func postLinearEvent(t *testing.T, bot *chat.Chat, secret string, body string) int {
	t.Helper()
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	bodyBytes := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(bodyBytes))
	signLinearRequest(req, secret, bodyBytes)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Code
}

func signLinearRequest(req *http.Request, secret string, body []byte) {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req.Header.Set("Linear-Signature", hex.EncodeToString(mac.Sum(nil)))
}

func createdPayload(now time.Time, commentID string, body string, userID string, userName string, appUserID string) string {
	return createdPayloadWithActorType(now, commentID, body, userID, userName, appUserID, "user")
}

func createdPayloadWithActorType(now time.Time, commentID string, body string, userID string, userName string, appUserID string, actorType string) string {
	return fmt.Sprintf(`{"type":"AgentSessionEvent","action":"created","organizationId":"ORG1","createdAt":"2026-05-12T00:00:00Z","webhookTimestamp":%d,"agentSession":{"id":"S1","issueId":"ISSUE1","appUserId":"%s","comment":{"id":"%s","body":"%s","createdAt":"2026-05-12T00:00:00Z"},"creator":{"id":"%s","type":"%s","name":"%s"}}}`, now.UnixMilli(), appUserID, commentID, body, userID, actorType, userName)
}

func delegatedPayload(now time.Time, sessionID string, promptContext string) string {
	return fmt.Sprintf(`{"type":"AgentSessionEvent","action":"created","organizationId":"ORG1","createdAt":"2026-05-12T00:00:00Z","promptContext":"%s","webhookTimestamp":%d,"agentSession":{"id":"%s","issueId":"ISSUE1","appUserId":"APP1"}}`, promptContext, now.UnixMilli(), sessionID)
}

func promptedPayload(now time.Time, commentID string, body string, userID string, userName string) string {
	return fmt.Sprintf(`{"type":"AgentSessionEvent","action":"prompted","organizationId":"ORG1","createdAt":"2026-05-12T00:00:01Z","webhookTimestamp":%d,"agentSession":{"id":"S1","issueId":"ISSUE1","appUserId":"APP1","comment":{"id":"C1","body":"hello"}},"agentActivity":{"id":"A1","sourceCommentId":"%s","createdAt":"2026-05-12T00:00:01Z","content":{"type":"prompt","body":"%s"},"user":{"id":"%s","type":"user","name":"%s"}}}`, now.UnixMilli(), commentID, body, userID, userName)
}

type linearAPIServer struct {
	*httptest.Server
	mu         sync.Mutex
	tokens     int
	activity   []linearActivity
	expires    int64
	tokenScope string
}

type linearActivity struct {
	AgentSessionID string          `json:"agentSessionId"`
	Content        activityContent `json:"content"`
	Ephemeral      bool            `json:"ephemeral"`
}

type activityContent struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

func newLinearAPIServer(t *testing.T, expires int64) *linearAPIServer {
	t.Helper()
	api := &linearAPIServer{expires: expires, tokenScope: "read write app:mentionable app:assignable"}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" {
				t.Fatalf("unexpected token form: %v", r.Form)
			}
			if got := r.Form.Get("scope"); got != "read,write,app:mentionable,app:assignable" {
				t.Fatalf("scopes = %q", got)
			}
			api.mu.Lock()
			api.tokens++
			token := fmt.Sprintf("token-%d", api.tokens)
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"access_token": token, "expires_in": api.expires, "scope": api.tokenScope})
		case "/graphql":
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer token-") {
				t.Fatalf("authorization = %q", got)
			}
			var req graphQLRequest
			decodeJSON(t, r.Body, &req)
			if strings.Contains(req.Query, "ViewerIdentity") {
				writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "APP1", "name": "Linear Bot", "displayName": "Linear Bot", "organization": map[string]any{"id": "ORG1"}}}})
				return
			}
			if strings.Contains(req.Query, "AgentActivityCreate") {
				input, ok := req.Variables["input"].(map[string]any)
				if !ok {
					t.Fatalf("variables = %#v", req.Variables)
				}
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatalf("marshal activity: %v", err)
				}
				var activity linearActivity
				if err := json.Unmarshal(body, &activity); err != nil {
					t.Fatalf("decode activity: %v", err)
				}
				api.mu.Lock()
				api.activity = append(api.activity, activity)
				id := fmt.Sprintf("ACT%d", len(api.activity))
				api.mu.Unlock()
				writeJSON(t, w, map[string]any{"data": map[string]any{"agentActivityCreate": map[string]any{"success": true, "agentActivity": map[string]any{"id": id}}}})
				return
			}
			t.Fatalf("unexpected graphql query: %s", req.Query)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *linearAPIServer) tokenRequests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens
}

func (a *linearAPIServer) assertActivity(t *testing.T, index int, want linearActivity) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.activity) <= index {
		t.Fatalf("activity count = %d, want index %d", len(a.activity), index)
	}
	if a.activity[index] != want {
		t.Fatalf("activity[%d] = %#v, want %#v", index, a.activity[index], want)
	}
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func decodeJSON(t *testing.T, body io.Reader, dest any) {
	t.Helper()
	if err := json.NewDecoder(body).Decode(dest); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func equalStrings(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
