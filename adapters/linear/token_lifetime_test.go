package linear_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// tokenWithoutExpiryServer mints client-credentials tokens whose response has
// no expires_in, and answers any GraphQL query with an empty data object.
type tokenWithoutExpiryServer struct {
	*httptest.Server
	mu     sync.Mutex
	tokens int
}

func newTokenWithoutExpiryServer(t *testing.T) *tokenWithoutExpiryServer {
	t.Helper()
	api := &tokenWithoutExpiryServer{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			api.mu.Lock()
			api.tokens++
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"access_token": "token", "scope": "read write app:mentionable app:assignable"})
		case "/graphql":
			writeJSON(t, w, map[string]any{"data": map[string]any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (api *tokenWithoutExpiryServer) tokenRequests() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.tokens
}

// assertTokenRefreshedAfterDefaultLifetime drives query across Linear's 30-day
// client-credentials lifetime: the token is reused within it and re-fetched
// once the clock passes 30 days minus the 1-hour refresh buffer.
func assertTokenRefreshedAfterDefaultLifetime(t *testing.T, api *tokenWithoutExpiryServer, clock *mutableClock, query func() error) {
	t.Helper()
	if err := query(); err != nil {
		t.Fatalf("first query: %v", err)
	}
	if got := api.tokenRequests(); got != 1 {
		t.Fatalf("token requests after first query = %d, want 1", got)
	}
	clock.advance(29 * 24 * time.Hour)
	if err := query(); err != nil {
		t.Fatalf("query within lifetime: %v", err)
	}
	if got := api.tokenRequests(); got != 1 {
		t.Fatalf("token requests within lifetime = %d, want 1 (cached)", got)
	}
	clock.advance(24*time.Hour - time.Hour + time.Minute)
	if err := query(); err != nil {
		t.Fatalf("query past lifetime: %v", err)
	}
	if got := api.tokenRequests(); got != 2 {
		t.Fatalf("token requests past 30 days minus refresh buffer = %d, want 2 (refreshed)", got)
	}
}

func TestSingleInstallRefreshesTokenWithoutExpiresIn(t *testing.T) {
	t.Parallel()
	api := newTokenWithoutExpiryServer(t)
	clock := newMutableClock(time.UnixMilli(1_700_000_000_000))
	adapter, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "client", ClientSecret: "secret"},
		APIBaseURL:        api.URL,
		Client:            api.Client(),
		Now:               clock.now,
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	assertTokenRefreshedAfterDefaultLifetime(t, api, clock, func() error {
		var dest map[string]any
		return adapter.GraphQL(context.Background(), `query Q { viewer { id } }`, nil, &dest)
	})
}

func TestMultiTenantRefreshesTokenWithoutExpiresIn(t *testing.T) {
	t.Parallel()
	api := newTokenWithoutExpiryServer(t)
	clock := newMutableClock(time.UnixMilli(1_700_000_000_000))
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
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	assertTokenRefreshedAfterDefaultLifetime(t, api, clock, func() error {
		var dest map[string]any
		return adapter.GraphQLForTenant(context.Background(), "ORG_A", `query Q { viewer { id } }`, nil, &dest)
	})
}
