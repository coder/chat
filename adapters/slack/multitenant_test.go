package slack_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

// fakeInstallStore is an in-memory chat.InstallStore for adapter tests. It records
// every lookup (adapter+tenant) and can be configured to return a non-not-found
// transport error.
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

func (s *fakeInstallStore) lookedUp(tenant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == "slack:"+tenant {
			return true
		}
	}
	return false
}

func TestSlackConstructionModeSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newFakeInstallStore()

	if _, err := slack.New(ctx, slack.Options{SigningSecret: "secret", BotToken: "xoxb-test"}); err != nil {
		t.Fatalf("C1 single-install construction: %v", err)
	}
	if _, err := slack.New(ctx, slack.Options{SigningSecret: "secret", InstallStore: store}); err != nil {
		t.Fatalf("C2 multi-tenant construction: %v", err)
	}
	if _, err := slack.New(ctx, slack.Options{SigningSecret: "secret", BotToken: "xoxb-test", InstallStore: store}); err == nil {
		t.Fatal("C3 both bot token and install store should fail")
	}
	if _, err := slack.New(ctx, slack.Options{SigningSecret: "secret"}); err == nil {
		t.Fatal("C4 neither bot token nor install store should fail")
	}
	if _, err := slack.New(ctx, slack.Options{InstallStore: store}); err == nil {
		t.Fatal("missing signing secret should fail in multi-tenant mode")
	}
}

func newMultiTenantSlackRuntime(t *testing.T, api *slackAPIServer, store chat.InstallStore, now time.Time) *chat.Chat {
	t.Helper()
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		InstallStore:  store,
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new multi-tenant slack adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	return bot
}

func slackEventBody(team, eventID, user, text string) string {
	return fmt.Sprintf(`{
		"type":"event_callback",
		"team_id":"%s",
		"event_id":"%s",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"%s",
			"text":"%s",
			"ts":"111.000"
		}
	}`, team, eventID, user, text)
}

// L1 + V1 + S1: a hit returns the right per-workspace bot token; verification uses
// the shared signing secret; the reply Authorization carries the workspace token.
func TestSlackMultiTenantPerWorkspaceTokenSelection(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2", BotUserID: "UBOT2"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	var threads = map[string]chat.ThreadID{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threads[ev.Event.Tenant] = ev.Thread.ID()
		_, err := ev.Thread.Post(ctx, chat.Text("reply for "+ev.Event.Tenant))
		return err
	})

	postSlackEvent(t, bot, now, slackEventBody("T1", "EvT1", "U1", "<@UBOT1> hi"), "", "")
	postSlackEvent(t, bot, now, slackEventBody("T2", "EvT2", "U2", "<@UBOT2> hi"), "", "")

	// Per-workspace token on verify+post: T1 reply carries xoxb-T1, T2 carries xoxb-T2.
	if got := api.authForPost(t, 0); got != "Bearer xoxb-T1" {
		t.Fatalf("post[0] auth = %q, want Bearer xoxb-T1", got)
	}
	if got := api.authForPost(t, 1); got != "Bearer xoxb-T2" {
		t.Fatalf("post[1] auth = %q, want Bearer xoxb-T2", got)
	}
	// The dispatched threads carry the right tenant baked into the opaque Thread ID.
	adapter, _ := chat.AdapterAs[*slack.Adapter](bot, "slack")
	for _, team := range []string{"T1", "T2"} {
		ref, err := adapter.ValidateThreadID(threads[team])
		if err != nil {
			t.Fatalf("validate %s thread id: %v", team, err)
		}
		if ref.Tenant != team {
			t.Fatalf("thread tenant = %q, want %q", ref.Tenant, team)
		}
	}
}

// T1: the tenant extracted for lookup equals the tenant baked into ThreadID and
// author Actor, and the store was asked for exactly that tenant.
func TestSlackMultiTenantTenantCorrectIdentifiers(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2", BotUserID: "UBOT2"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	var captured *chat.Event
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		captured = ev.Event
		return nil
	})
	postSlackEvent(t, bot, now, slackEventBody("T2", "EvT2", "U2", "<@UBOT2> hi"), "", "")

	if captured == nil {
		t.Fatal("event not dispatched")
	}
	if captured.Tenant != "T2" {
		t.Fatalf("event tenant = %q, want T2", captured.Tenant)
	}
	if captured.Message.Author.Tenant != "T2" {
		t.Fatalf("author tenant = %q, want T2", captured.Message.Author.Tenant)
	}
	adapter, _ := chat.AdapterAs[*slack.Adapter](bot, "slack")
	ref, err := adapter.ValidateThreadID(captured.ThreadID)
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	if ref.Tenant != "T2" {
		t.Fatalf("thread id tenant = %q, want T2", ref.Tenant)
	}
	if !store.lookedUp("T2") {
		t.Fatalf("store was not asked for T2, calls = %v", store.calls)
	}
}

// L2 + SM1 region: ErrInstallNotFound is an Ignored Event: acked 200, not dispatched.
func TestSlackMultiTenantNotInstalledIsIgnored(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore() // empty: every tenant is not-installed
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	status := postSlackEvent(t, bot, now, slackEventBody("TX", "EvTX", "U1", "<@UBOT> hi"), "", "")
	if status != http.StatusOK {
		t.Fatalf("not-installed status = %d, want 200", status)
	}
	if dispatched != 0 {
		t.Fatalf("not-installed event dispatched %d times", dispatched)
	}
	api.mu.Lock()
	posts := len(api.posts)
	api.mu.Unlock()
	if posts != 0 {
		t.Fatalf("not-installed event posted %d messages", posts)
	}
}

// L3: a non-not-found store error surfaces as a retriable 5xx; not dispatched.
func TestSlackMultiTenantStoreTransportErrorIsRetriable(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.transport = errors.New("store offline")
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("transport error reached dispatch")
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, slackEventBody("TX", "EvTX", "U1", "<@UBOT> hi"), "", "")
	if rec.Code < 500 {
		t.Fatalf("transport error status = %d, want 5xx", rec.Code)
	}
}

// L4: Lookup is called per event; the adapter does not cache install records.
func TestSlackMultiTenantLookupPerEvent(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	postSlackEvent(t, bot, now, slackEventBody("T1", "EvA", "U1", "<@UBOT1> one"), "", "")
	postSlackEvent(t, bot, now, slackEventBody("T1", "EvB", "U1", "<@UBOT1> two"), "", "")
	if store.callCount() != 2 {
		t.Fatalf("lookup calls = %d, want 2 (no adapter caching)", store.callCount())
	}
}

// SM1: a webhook authored by the per-install bot is dropped (Ignored, not
// dispatched), proving self-filtering is tenant-correct from the install record.
func TestSlackMultiTenantSelfMessageFilteredFromInstall(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	// Authored by the workspace's own bot user (UBOT1) -> self -> Ignored.
	body := `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"EvSelf",
		"event":{"type":"message","channel":"C1","user":"UBOT1","text":"bot echo","ts":"222.000"}
	}`
	status := postSlackEvent(t, bot, now, body, "", "")
	if status != http.StatusOK {
		t.Fatalf("self message status = %d, want 200", status)
	}
	if dispatched != 0 {
		t.Fatalf("self message dispatched %d times", dispatched)
	}
}

// BotActorID precedence: Install.BotActorID is preferred over SlackInstall.BotUserID
// for self-filtering.
func TestSlackMultiTenantBotActorIDPrecedence(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", BotActorID: "UGENERIC", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UCRED"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)
	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	body := `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"EvSelfGeneric",
		"event":{"type":"message","channel":"C1","user":"UGENERIC","text":"bot echo","ts":"222.000"}
	}`
	postSlackEvent(t, bot, now, body, "", "")
	if dispatched != 0 {
		t.Fatalf("Install.BotActorID self message dispatched %d times", dispatched)
	}
}

// TH1: out-of-webhook posting resolves credentials through the InstallStore for the
// tenant decoded from a stored Thread ID, and posts with that tenant's token.
func TestSlackMultiTenantThreadHandlePostResolvesToken(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	threadID := slack.EncodeChannelThreadIDForTest("T2", "C9")
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("out of band")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := api.authForPost(t, 0); got != "Bearer xoxb-T2" {
		t.Fatalf("thread post auth = %q, want Bearer xoxb-T2", got)
	}
}

// TH2: when the install record is gone, out-of-webhook Post fails cleanly and posts
// nothing.
func TestSlackMultiTenantThreadHandlePostFailsCleanlyWhenUninstalled(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore() // T3 not installed
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	threadID := slack.EncodeChannelThreadIDForTest("T3", "C9")
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("out of band")); err == nil {
		t.Fatal("expected clean error posting to uninstalled tenant")
	}
	api.mu.Lock()
	posts := len(api.posts)
	api.mu.Unlock()
	if posts != 0 {
		t.Fatalf("uninstalled post wrote %d messages", posts)
	}
}

// T2: two tenants delivering concurrent webhooks resolve distinct credentials and
// do not cross dedupe or lock scopes (distinct Event IDs / Thread IDs).
func TestSlackMultiTenantConcurrentTenantsDoNotCross(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2", BotUserID: "UBOT2"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)

	var mu sync.Mutex
	seen := map[string]bool{}
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		seen[ev.Event.Tenant] = true
		mu.Unlock()
		return nil
	})

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	var wg sync.WaitGroup
	for _, team := range []string{"T1", "T2"} {
		wg.Add(1)
		go func(team string) {
			defer wg.Done()
			rec := serveSignedSlackWebhook(t, handler, now,
				slackEventBody(team, "Ev"+team, "U"+team, "<@UBOT"+team[1:]+"> hi"), "", "")
			if rec.Code != http.StatusOK {
				t.Errorf("team %s status = %d", team, rec.Code)
			}
		}(team)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !seen["T1"] || !seen["T2"] {
		t.Fatalf("both tenants should dispatch, seen = %v", seen)
	}
}

// A non-SlackInstall credential is a clear error, not a panic.
func TestSlackMultiTenantWrongCredentialType(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: "not-a-slack-install"})
	bot := newMultiTenantSlackRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, slackEventBody("T1", "EvBad", "U1", "<@UBOT> hi"), "", "")
	if rec.Code < 500 {
		t.Fatalf("wrong credential type status = %d, want 5xx", rec.Code)
	}
}
