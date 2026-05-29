package linear_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

// TestDeferredDispatchPostsFromDetachedTail verifies the Linear adapter's use of
// the ADR 0002 deferred-dispatch seam: under DispatchDeferred the handler runs on
// the detached work context after ack, and the adapter can still post an Agent
// Activity Response there even though the inbound request context is cancelled.
func TestDeferredDispatchPostsFromDetachedTail(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)

	adapter, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "client", ClientSecret: "secret"},
		APIBaseURL:        api.URL,
		Client:            api.Client(),
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(memory.New()),
		chat.WithAdapter(adapter),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer func() { _ = bot.Shutdown(context.Background()) }()

	var wg sync.WaitGroup
	wg.Add(1)
	var postErr error
	var handlerCtxErr error
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		defer wg.Done()
		// The detached tail context must be live, not the cancelled request context.
		handlerCtxErr = ctx.Err()
		_, postErr = ev.Thread.Post(ctx, chat.Text("deferred reply"))
		return postErr
	})

	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	wg.Wait()

	if handlerCtxErr != nil {
		t.Fatalf("handler ctx already cancelled: %v", handlerCtxErr)
	}
	if postErr != nil {
		t.Fatalf("deferred post failed: %v", postErr)
	}
	api.assertActivity(t, 0, linearActivity{AgentSessionID: "S1", Content: activityContent{Type: "response", Body: "deferred reply"}})
}
