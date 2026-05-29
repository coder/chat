package linear_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

// remove deletes a tenant's install record from the shared fake store so a test
// can simulate an uninstall between events.
func (s *fakeInstallStore) remove(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.installs, tenant)
}

// mutableClock is an adjustable monotonic-ish clock for driving the lazy
// token-refresh boundary deterministically.
type mutableClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMutableClock(start time.Time) *mutableClock { return &mutableClock{t: start} }

func (c *mutableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mutableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestLinearMultiTenantBadSignatureNoSideEffect proves the unverified-body routing
// read is re-validated by signature verification BEFORE any side effect: a webhook
// for an installed org but signed with the wrong secret is 401, never dispatched,
// and triggers no token exchange (no outbound GraphQL/auth side effect).
func TestLinearMultiTenantBadSignatureNoSideEffect(t *testing.T) {
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
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("badly-signed event reached dispatch")
		return nil
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := postLinearEventWithSecret(t, handler, "wrong-secret",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", rec.Code)
	}
	// The routing read resolved the install (one lookup) but verification failed before
	// any side effect: no token was ever exchanged.
	if got := api.exchangeCount("clientA"); got != 0 {
		t.Fatalf("bad signature triggered %d token exchanges, want 0 (no side effect before verify)", got)
	}
}

// TestLinearMultiTenantOrglessBodyIgnored proves an unparseable or org-less body
// cannot be routed to an install and is an Ignored Event (200, no dispatch, no
// signature requirement, no store lookup).
func TestLinearMultiTenantOrglessBodyIgnored(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
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
	// Org-less body signed with an arbitrary secret.
	if rec := postLinearEventWithSecret(t, handler, "anything",
		`{"type":"AgentSessionEvent","action":"created","webhookTimestamp":1}`); rec.Code != http.StatusOK {
		t.Fatalf("org-less body status = %d, want 200", rec.Code)
	}
	// Unparseable body.
	if rec := postLinearEventWithSecret(t, handler, "anything", `{not json`); rec.Code != http.StatusOK {
		t.Fatalf("unparseable body status = %d, want 200", rec.Code)
	}
	if dispatched != 0 {
		t.Fatalf("org-less/unparseable dispatched %d times", dispatched)
	}
	if store.callCount() != 0 {
		t.Fatalf("org-less/unparseable triggered %d lookups, want 0", store.callCount())
	}
}

// TestLinearMultiTenantPreExchangedAccessToken proves a pre-exchanged installation
// AccessToken on the install record is used directly and skips the client-credentials
// exchange entirely.
func TestLinearMultiTenantPreExchangedAccessToken(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", AccessToken: "preexchanged-A"},
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer preexchanged-A" {
		t.Fatalf("activity auth = %v, want [Bearer preexchanged-A]", auths)
	}
	// No client-credentials exchange occurred for the pre-exchanged token path.
	if got := api.exchangeCount("clientA"); got != 0 {
		t.Fatalf("pre-exchanged token triggered %d exchanges, want 0", got)
	}
}

// TestLinearMultiTenantCredentialWithoutTokenOrCreds proves an install credential
// carrying neither an access token nor client credentials is a clean error on the
// posting path (not a panic, not an empty-Bearer call).
func TestLinearMultiTenantCredentialWithoutTokenOrCreds(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA"}, // no token, no client creds
	})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	// Verification (which uses only the webhook secret) succeeds, but the post cannot
	// resolve a token, so the deferred work fails; nothing is posted to the API.
	postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA"))
	api.mu.Lock()
	posts := len(api.activityAuth)
	api.mu.Unlock()
	if posts != 0 {
		t.Fatalf("credential without token/creds posted %d activities, want 0", posts)
	}
}

// TestLinearGraphQLRejectedInMultiTenant proves GraphQL (no tenant) is rejected in
// multi-tenant mode while GraphQLForTenant resolves the per-org token and queries.
func TestLinearGraphQLRejectedInMultiTenant(t *testing.T) {
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
	_, adapter := newMultiTenantLinearRuntime(t, api, store, now)

	if err := adapter.GraphQL(context.Background(), `query { viewer { id } }`, nil, nil); err == nil {
		t.Fatal("GraphQL must require a tenant in multi-tenant mode")
	}
	// GraphQLForTenant for an uninstalled org fails cleanly.
	if err := adapter.GraphQLForTenant(context.Background(), "ORG_GONE", `query { viewer { id } }`, nil, nil); err == nil {
		t.Fatal("GraphQLForTenant must fail for an uninstalled org")
	}
	// GraphQLForTenant resolves ORG_A's per-org token for an AgentActivityCreate-shaped
	// mutation (the MT server only knows that and oauth/token).
	var dest struct {
		AgentActivityCreate struct {
			Success bool `json:"success"`
		} `json:"agentActivityCreate"`
	}
	if err := adapter.GraphQLForTenant(context.Background(), "ORG_A",
		`mutation AgentActivityCreate { agentActivityCreate { success } }`, nil, &dest); err != nil {
		t.Fatalf("GraphQLForTenant: %v", err)
	}
	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer token-A" {
		t.Fatalf("GraphQLForTenant auth = %v, want [Bearer token-A]", auths)
	}
}

// TestLinearMultiTenantPointerCredential proves a *LinearInstall credential is
// accepted (pointer and value forms both decode) and a typed nil pointer is a clean
// error rather than a panic.
func TestLinearMultiTenantPointerCredential(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: &linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	store.set("ORG_NIL", chat.Install{Tenant: "ORG_NIL", Credential: (*linear.LinearInstall)(nil)})
	bot, _ := newMultiTenantLinearRuntime(t, api, store, now)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	// Pointer credential resolves and verifies.
	if rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA")); rec.Code != http.StatusOK {
		t.Fatalf("pointer credential status = %d", rec.Code)
	}
	// Typed nil pointer: the install record has no webhook secret, so verification
	// material is missing and the event is a clean 5xx, never a panic.
	rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_NIL", "SN", "CN1", "hi", "U1", "APP_X", "clientX"))
	if rec.Code < 500 {
		t.Fatalf("nil pointer credential status = %d, want 5xx", rec.Code)
	}
}

// TestLinearMultiTenantNoInstallSurfaceOnState proves Runtime State is not expanded
// for install records: a multi-tenant Linear adapter serves a webhook over plain
// coordination State, and the credential source is only the app-owned InstallStore.
func TestLinearMultiTenantNoInstallSurfaceOnState(t *testing.T) {
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
	adapter, err := linear.New(context.Background(), linear.Options{
		InstallStore: store,
		APIBaseURL:   api.URL,
		Client:       api.Client(),
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if rec := postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer token-A" {
		t.Fatalf("activity auth = %v, want [Bearer token-A] (credential from store, not State)", auths)
	}
}

// TestLinearMultiTenantLazyRefreshReExchangesAfterExpiry proves the tenant-keyed
// lazy refresh (ADR-0001): the derived token is cached per Platform Tenant and
// reused until near expiry, then re-exchanged. The install record is re-fetched per
// event (no adapter install-record caching), so once the cached token expires the
// re-exchange picks up the install record's current client credentials. Advancing a
// mutable clock past expiry drives the re-exchange.
func TestLinearMultiTenantLazyRefreshReExchangesAfterExpiry(t *testing.T) {
	t.Parallel()
	api := newLinearMTServer(t)
	clock := newMutableClock(time.UnixMilli(1_700_000_000_000))
	api.setOrgToken("clientA", "token-A")
	api.setOrgToken("clientROT", "token-ROT")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	adapter, err := linear.New(context.Background(), linear.Options{
		InstallStore: store,
		APIBaseURL:   api.URL,
		Client:       api.Client(),
		Now:          clock.now,
		// Loosen tolerance so the same payload timestamp passes after the clock advances.
		SignatureTolerance: 365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	at := clock.now()
	// Event 1: exchanges and caches token-A under ORG_A.
	postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(at, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA"))
	// Event 2 (token still fresh): reuses the cached token-A, no second exchange even
	// though the durable record is re-fetched per event.
	postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(at, "ORG_A", "SA", "CA2", "again", "U1", "APP_A", "clientA"))
	if got := api.exchangeCount("clientA"); got != 1 {
		t.Fatalf("clientA exchanges before expiry = %d, want 1 (cached, reused)", got)
	}

	// Rotate the install record and advance the clock past the token's expiry window
	// (mock mints expires_in=7200s; tokenRefreshBuffer is 1h, so >1h past mint forces
	// a re-exchange). The per-event Lookup now resolves the rotated credentials.
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientROT", ClientSecret: "csROT"}},
	})
	clock.advance(3 * time.Hour)
	postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(clock.now(), "ORG_A", "SA", "CA3", "rotated", "U1", "APP_A", "clientROT"))

	api.mu.Lock()
	auths := append([]string(nil), api.activityAuth...)
	api.mu.Unlock()
	if len(auths) != 3 {
		t.Fatalf("activity posts = %d, want 3", len(auths))
	}
	if auths[0] != "Bearer token-A" || auths[1] != "Bearer token-A" {
		t.Fatalf("pre-expiry auths = %v, want both token-A (cached)", auths[:2])
	}
	if auths[2] != "Bearer token-ROT" {
		t.Fatalf("post-expiry auth = %q, want token-ROT (re-exchanged after expiry with rotated creds)", auths[2])
	}
}

// TestLinearMultiTenantUninstallStopsResolving proves a revoked install stops
// resolving once the record is gone: after a successful event for ORG_A, removing
// the record makes a later out-of-webhook post for the same org fail cleanly, even
// though a derived token was previously cached.
func TestLinearMultiTenantUninstallStopsResolving(t *testing.T) {
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
	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		_, err := ev.Thread.Post(ctx, chat.Text("response"))
		return err
	})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	postLinearEventWithSecret(t, handler, "secretA",
		mtCreatedPayload(now, "ORG_A", "SA", "CA1", "hi", "U1", "APP_A", "clientA"))
	if threadID == "" {
		t.Fatal("no thread id captured")
	}

	// Uninstall: remove the record. A later out-of-webhook post must fail cleanly,
	// proving the derived-token cache does not outlive the install record.
	store.remove("ORG_A")
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("after uninstall")); err == nil {
		t.Fatal("expected clean error posting after uninstall")
	}
}

// TestLinearSingleInstallNeverTouchesStore is a guard that the default Single-Install
// Adapter construction validates and reaches Init without any InstallStore: it is the
// unchanged additive default. (Construction-only; no network Init is driven here.)
func TestLinearSingleInstallNeverTouchesStore(t *testing.T) {
	t.Parallel()
	if _, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"},
	}); err != nil {
		t.Fatalf("single-install construction: %v", err)
	}
	// A store transport error is irrelevant to single-install: it never calls Lookup.
	store := newFakeInstallStore()
	store.transport = errors.New("should never be called")
	if _, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "id", ClientSecret: "secret"},
	}); err != nil {
		t.Fatalf("single-install construction with unrelated store error: %v", err)
	}
	if store.callCount() != 0 {
		t.Fatalf("single-install touched the store %d times", store.callCount())
	}
}
