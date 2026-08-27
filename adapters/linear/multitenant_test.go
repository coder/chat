package linear_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// fakeInstallStore is an in-memory chat.InstallStore for Linear adapter tests.
type fakeInstallStore struct {
	mu        sync.Mutex
	installs  map[string]chat.Install
	transport error
	calls     []string
}

func newFakeInstallStore() *fakeInstallStore {
	return &fakeInstallStore{installs: map[string]chat.Install{}}
}

func (s *fakeInstallStore) set(tenant string, install chat.Install) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installs[tenant] = install
}

func (s *fakeInstallStore) Lookup(_ context.Context, adapter, tenant string) (chat.Install, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, adapter+":"+tenant)
	if s.transport != nil {
		return chat.Install{}, s.transport
	}
	install, ok := s.installs[tenant]
	if !ok {
		return chat.Install{}, chat.ErrInstallNotFound
	}
	return install, nil
}

func (s *fakeInstallStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestLinearConstructionModeSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newFakeInstallStore()

	// C5 single-install (client creds + secret) valid.
	if _, err := linear.New(ctx, linear.Options{WebhookSecret: "whsec", ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"}}); err != nil {
		t.Fatalf("C5 single-install construction: %v", err)
	}
	// C6 multi-tenant (install store) valid, no static secret/creds.
	if _, err := linear.New(ctx, linear.Options{InstallStore: store}); err != nil {
		t.Fatalf("C6 multi-tenant construction: %v", err)
	}
	// C7 both webhook secret and install store -> error.
	if _, err := linear.New(ctx, linear.Options{WebhookSecret: "whsec", InstallStore: store}); err == nil {
		t.Fatal("C7 webhook secret + install store should fail")
	}
	// C7b both client credentials and install store -> error.
	if _, err := linear.New(ctx, linear.Options{ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"}, InstallStore: store}); err == nil {
		t.Fatal("C7b client credentials + install store should fail")
	}
	// C8 neither -> error (no store, no secret).
	if _, err := linear.New(ctx, linear.Options{}); err == nil {
		t.Fatal("C8 neither install store nor webhook secret should fail")
	}
}

// linearMTServer is a multi-org Linear API mock. Each org has its own client
// credentials -> token mapping; it records every /oauth/token exchange and every
// GraphQL call's bearer token so tests can prove per-tenant resolution.
type linearMTServer struct {
	*httptest.Server
	mu sync.Mutex
	// tokenForClient maps an oauth client id to the token it should mint.
	tokenForClient map[string]string
	// exchanges counts /oauth/token calls per client id.
	exchanges map[string]int
	// activityAuth records the bearer token of each AgentActivityCreate call.
	activityAuth []string
	// historyAuth records the bearer token of each AgentSessionHistory read, so
	// history tests can prove per-tenant token resolution (see history_test.go).
	historyAuth []string
}

func newLinearMTServer(t *testing.T) *linearMTServer {
	t.Helper()
	api := &linearMTServer{
		tokenForClient: map[string]string{},
		exchanges:      map[string]int{},
	}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			clientID := r.Form.Get("client_id")
			api.mu.Lock()
			api.exchanges[clientID]++
			token, ok := api.tokenForClient[clientID]
			api.mu.Unlock()
			if !ok {
				t.Fatalf("unexpected client id %q in token exchange", clientID)
			}
			writeJSON(t, w, map[string]any{"access_token": token, "expires_in": 7200, "scope": "read write app:mentionable app:assignable"})
		case "/graphql":
			auth := r.Header.Get("Authorization")
			var req graphQLRequest
			decodeJSON(t, r.Body, &req)
			if strings.Contains(req.Query, "AgentActivityCreate") {
				api.mu.Lock()
				api.activityAuth = append(api.activityAuth, auth)
				id := fmt.Sprintf("ACT%d", len(api.activityAuth))
				api.mu.Unlock()
				writeJSON(t, w, map[string]any{"data": map[string]any{"agentActivityCreate": map[string]any{"success": true, "agentActivity": map[string]any{"id": id}}}})
				return
			}
			if strings.Contains(req.Query, "AgentSessionHistory") {
				api.mu.Lock()
				api.historyAuth = append(api.historyAuth, auth)
				api.mu.Unlock()
				writeJSON(t, w, map[string]any{"data": map[string]any{"agentSession": map[string]any{"activities": map[string]any{"nodes": []any{}}}}})
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

func (api *linearMTServer) setOrgToken(clientID, token string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.tokenForClient[clientID] = token
}

func (api *linearMTServer) exchangeCount(clientID string) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.exchanges[clientID]
}

func (api *linearMTServer) historyAuths() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]string(nil), api.historyAuth...)
}

func newMultiTenantLinearRuntime(t *testing.T, api *linearMTServer, store chat.InstallStore, now time.Time) (*chat.Chat, *linear.Adapter) {
	t.Helper()
	adapter, err := linear.New(context.Background(), linear.Options{
		InstallStore: store,
		APIBaseURL:   api.URL,
		Client:       api.Client(),
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new multi-tenant linear adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return bot, adapter
}

func mtCreatedPayload(now time.Time, org, sessionID, commentID, body, userID, appUserID, oauthClientID string) string {
	return fmt.Sprintf(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"organizationId":"%s",
		"oauthClientId":"%s",
		"createdAt":"2026-05-12T00:00:00Z",
		"webhookTimestamp":%d,
		"agentSession":{
			"id":"%s",
			"issueId":"ISSUE1",
			"appUserId":"%s",
			"comment":{"id":"%s","body":"%s","createdAt":"2026-05-12T00:00:00Z"},
			"creator":{"id":"%s","type":"user","name":"User"}
		}
	}`, org, oauthClientID, now.UnixMilli(), sessionID, appUserID, commentID, body, userID)
}

func postLinearEventWithSecret(t *testing.T, handler http.Handler, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(bodyBytes))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(bodyBytes)
	req.Header.Set("Linear-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// Li1 + V2 + TH-region: per-org credential resolution keyed by organizationId, with
// per-tenant lazy token exchange. ORG_A and ORG_B resolve distinct client creds and
// exchange distinct tokens; a repeat ORG_A event reuses the cached token.
func TestLinearMultiTenantPerOrgResolutionAndLazyRefresh(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	api.setOrgToken("clientB", "token-B")

	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	store.set("ORG_B", chat.Install{
		Tenant:     "ORG_B",
		BotActorID: "APP_B",
		Credential: linear.LinearInstall{WebhookSecret: "secretB", ClientCredentials: linear.ClientCredentials{ClientID: "clientB", ClientSecret: "csB"}},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)

	posted := map[string]bool{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		posted[ev.Event.Tenant] = true
		_, err := ev.Thread.Post(ctx, chat.Text("response for "+ev.Event.Tenant))
		return err
	})

	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	// ORG_A signed with secretA verifies and posts with token-A.
	if rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA")); rec.Code != http.StatusOK {
		t.Fatalf("ORG_A status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// ORG_B signed with secretB verifies and posts with token-B.
	if rec := postLinearEventWithSecret(t, handler, "secretB",
		mtCreatedPayload(now, "ORG_B", "SB", "CB1", "hi", "U2", "APP_B", "clientB")); rec.Code != http.StatusOK {
		t.Fatalf("ORG_B status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Repeat ORG_A: reuses the cached derived token (no second exchange for clientA).
	if rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA2", "again", "U1", "APP_A", "clientA")); rec.Code != http.StatusOK {
		t.Fatalf("ORG_A repeat status = %d", rec.Code)
	}

	if !posted["ORG_A"] || !posted["ORG_B"] {
		t.Fatalf("both orgs should dispatch, posted = %v", posted)
	}
	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) != 3 {
		t.Fatalf("activity posts = %d, want 3", len(auths))
	}
	if auths[0] != "Bearer token-A" || auths[2] != "Bearer token-A" {
		t.Fatalf("ORG_A posts auth = %v, want token-A", []string{auths[0], auths[2]})
	}
	if auths[1] != "Bearer token-B" {
		t.Fatalf("ORG_B post auth = %q, want token-B", auths[1])
	}
	// Per-tenant lazy refresh keyed by Platform Tenant: one exchange per client.
	if got := api.exchangeCount("clientA"); got != 1 {
		t.Fatalf("clientA exchanges = %d, want 1 (cached after first)", got)
	}
	if got := api.exchangeCount("clientB"); got != 1 {
		t.Fatalf("clientB exchanges = %d, want 1", got)
	}
}

// V2: a webhook signed with the wrong secret is rejected 401; a not-installed org
// is an Ignored Event (200) before any signature work is required to be correct.
func TestLinearMultiTenantPerInstallSignatureAndUninstalled(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	// Wrong secret for an installed org -> 401, not dispatched.
	if rec := postLinearEventWithSecret(t, handler, "wrong-secret",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want 401", rec.Code)
	}
	// Uninstalled org -> Ignored Event 200, never reaching signature requirement.
	if rec := postLinearEventWithSecret(t, handler, "whatever",
		mtCreatedPayload(now, "ORG_X", "SX", "CX1", "hi", "U1", "APP_X", "clientX")); rec.Code != http.StatusOK {
		t.Fatalf("uninstalled org status = %d, want 200", rec.Code)
	}
	if dispatched != 0 {
		t.Fatalf("dispatched %d times, want 0", dispatched)
	}
	// The uninstalled org never triggered a token exchange.
	if got := api.exchangeCount("clientX"); got != 0 {
		t.Fatalf("uninstalled org token exchanges = %d, want 0", got)
	}
}

// Li2: an event for an uninstalled org is an Ignored Event with no dispatch and no
// token exchange (covered above), plus a transport store error is a retriable 5xx.
func TestLinearMultiTenantStoreTransportErrorIsRetriable(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
	store.transport = errors.New("store offline")
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("transport error reached dispatch")
		return nil
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA"))
	if rec.Code < 500 {
		t.Fatalf("transport error status = %d, want 5xx", rec.Code)
	}
}

// Lookup is called per event; the adapter does not cache install records.
func TestLinearMultiTenantLookupPerEvent(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error { return nil })
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	postLinearEventWithSecret(t, handler, "secretA", mtCreatedPayload(now, "ORG_A", "S1", "C1", "a", "U1", "APP_A", "clientA"))
	postLinearEventWithSecret(t, handler, "secretA", mtCreatedPayload(now, "ORG_A", "S2", "C2", "b", "U1", "APP_A", "clientA"))
	// Two webhooks: at least two lookups (verification + any out-of-webhook posting
	// also looks up, so it is >= 2, never cached down to 1).
	if store.callCount() < 2 {
		t.Fatalf("lookup calls = %d, want >= 2 (no adapter caching)", store.callCount())
	}
}

// SM1: a webhook authored by the per-install app actor is dropped (Ignored), proving
// self-filtering is tenant-correct from the install record's BotActorID.
func TestLinearMultiTenantSelfMessageFilteredFromInstall(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	// Creator is the per-install app actor APP_A with a bot actor type.
	self := fmt.Sprintf(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"organizationId":"ORG_A",
		"oauthClientId":"clientA",
		"createdAt":"2026-05-12T00:00:00Z",
		"webhookTimestamp":%d,
		"agentSession":{
			"id":"S1",
			"issueId":"ISSUE1",
			"appUserId":"APP_A",
			"comment":{"id":"C1","body":"self","createdAt":"2026-05-12T00:00:00Z"},
			"creator":{"id":"APP_A","type":"bot","name":"App A"}
		}
	}`, now.UnixMilli())
	if rec := postLinearEventWithSecret(t, handler, "secretA", self); rec.Code != http.StatusOK {
		t.Fatalf("self event status = %d", rec.Code)
	}
	if dispatched != 0 {
		t.Fatalf("self event dispatched %d times", dispatched)
	}
}

// TH1: out-of-webhook posting resolves credentials through the InstallStore for the
// tenant decoded from a stored Thread ID, and posts with that org's token.
func TestLinearMultiTenantThreadHandlePostResolvesToken(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientB", "token-B")
	store := newFakeInstallStore()
	store.set("ORG_B", chat.Install{
		Tenant:     "ORG_B",
		BotActorID: "APP_B",
		Credential: linear.LinearInstall{WebhookSecret: "secretB", ClientCredentials: linear.ClientCredentials{ClientID: "clientB", ClientSecret: "csB"}},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)

	// Drive a webhook to mint a real agent-session Thread ID, then post out of band.
	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	postLinearEventWithSecret(t, handler, "secretB", mtCreatedPayload(now, "ORG_B", "SB", "CB1", "hi", "U1", "APP_B", "clientB"))
	if threadID == "" {
		t.Fatal("no thread id captured")
	}
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("background result")); err != nil {
		t.Fatalf("thread post: %v", err)
	}
	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) == 0 || auths[len(auths)-1] != "Bearer token-B" {
		t.Fatalf("thread post auth = %v, want last = token-B", auths)
	}
}

// TH2: when the install record is gone, out-of-webhook Post fails cleanly.
func TestLinearMultiTenantThreadHandlePostFailsCleanlyWhenUninstalled(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore() // ORG_C not installed
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)

	threadID := linear.EncodeAgentSessionThreadIDForTest("ORG_C", "ISSUE1", "S-C")
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("nope")); err == nil {
		t.Fatal("expected clean error posting to uninstalled org")
	}
	api.mu.Lock()
	posts := len(api.activityAuth)
	api.mu.Unlock()
	if posts != 0 {
		t.Fatalf("uninstalled post wrote %d activities", posts)
	}
}

// A non-LinearInstall credential is a clear error, not a panic.
func TestLinearMultiTenantWrongCredentialType(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{Tenant: "ORG_A", Credential: 12345})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "S1", "C1", "hi", "U1", "APP_A", "clientA"))
	if rec.Code < 500 {
		t.Fatalf("wrong credential type status = %d, want 5xx", rec.Code)
	}
}
