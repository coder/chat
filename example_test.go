package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/state/memory"
)

// demoAdapter is a minimal in-process chat.Adapter for the examples. Its
// webhook turns query parameters into a normalized chat.Event, and it prints
// posted messages instead of calling a platform API. A real adapter verifies
// the request signature and parses the platform payload instead.
type demoAdapter struct {
	nextEventID atomic.Int64
}

func (*demoAdapter) Name() string                   { return "demo" }
func (*demoAdapter) Init(context.Context) error     { return nil }
func (*demoAdapter) Shutdown(context.Context) error { return nil }

func (*demoAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: "demo", ID: "bot", Name: "bot", BotKind: chat.BotBot}
}

func (a *demoAdapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		user := q.Get("user")
		event := &chat.Event{
			ID:       fmt.Sprintf("demo-event-%d", a.nextEventID.Add(1)),
			Adapter:  "demo",
			ThreadID: chat.ThreadID("demo:" + q.Get("thread")),
			Message: &chat.Message{
				Text:      q.Get("text"),
				Author:    chat.Actor{Adapter: "demo", ID: user, Name: user, BotKind: chat.BotHuman},
				Mentioned: q.Has("mention"),
			},
		}
		if err := dispatch(r.Context(), event); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, chat.ErrAdmissionRejected) {
				// Overload: ask the platform to retry later.
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (*demoAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	channel, ok := strings.CutPrefix(string(id), "demo:")
	if !ok || channel == "" {
		return chat.ThreadRef{}, fmt.Errorf("demo: invalid thread id %q", id)
	}
	return chat.ThreadRef{ID: id, Adapter: "demo", Channel: channel}, nil
}

func (*demoAdapter) PostMessage(_ context.Context, ref chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	fmt.Printf("[%s] bot: %s\n", ref.Channel, msg.Text)
	return &chat.SentMessage{ID: "demo-sent", ThreadID: ref.ID}, nil
}

// ReadHistory implements the chat.HistoryReader optional capability with a
// fixed transcript.
func (*demoAdapter) ReadHistory(context.Context, chat.ThreadID, chat.HistoryQuery) ([]chat.Message, error) {
	return []chat.Message{
		{ID: "m1", Text: "is the build green?", Author: chat.Actor{Adapter: "demo", ID: "alice", Name: "alice"}},
		{ID: "m2", Text: "not yet", Author: chat.Actor{Adapter: "demo", ID: "bob", Name: "bob"}},
	}, nil
}

// Reply to every mention. The example drives the webhook in memory the way a
// platform would; a message that mentions nobody in an unsubscribed thread is
// acknowledged and ignored.
func ExampleChat_OnNewMention() {
	ctx := context.Background()
	bot, err := chat.New(ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(&demoAdapter{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("Hello, "+ev.Message.Author.Name+"!"))
		return err
	})

	webhook, err := bot.Webhook("demo")
	if err != nil {
		log.Fatal(err)
	}
	send := func(query string) int {
		rec := httptest.NewRecorder()
		webhook.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook?"+query, nil))
		return rec.Code
	}

	send("thread=general&user=alice&text=hi&mention")
	send("thread=random&user=bob&text=lunch")
	if err := bot.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	// Output:
	// [general] bot: Hello, alice!
}

// Subscribe to a thread so its later messages reach OnSubscribedMessage
// without a mention, and unsubscribe to stop listening.
func ExampleThread_Subscribe() {
	ctx := context.Background()
	bot, err := chat.New(ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(&demoAdapter{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		_, err := ev.Thread.Post(ctx, chat.Text(`Listening. Say "bye" to stop.`))
		return err
	})
	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Message.Text == "bye" {
			if err := ev.Thread.Unsubscribe(ctx); err != nil {
				return err
			}
			_, err := ev.Thread.Post(ctx, chat.Text("Goodbye!"))
			return err
		}
		_, err := ev.Thread.Post(ctx, chat.Text("You said: "+ev.Message.Text))
		return err
	})

	webhook, err := bot.Webhook("demo")
	if err != nil {
		log.Fatal(err)
	}
	send := func(query string) int {
		rec := httptest.NewRecorder()
		webhook.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook?"+query, nil))
		return rec.Code
	}

	send("thread=general&user=alice&text=hi&mention")
	send("thread=general&user=alice&text=deploy+status")
	send("thread=general&user=alice&text=bye")
	send("thread=general&user=alice&text=anyone+there") // ignored: no longer subscribed
	if err := bot.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	// Output:
	// [general] bot: Listening. Say "bye" to stop.
	// [general] bot: You said: deploy status
	// [general] bot: Goodbye!
}

// Acknowledge webhooks before slow handlers run, and cap the deferred work in
// flight with the admission bound. MaxDetached is 1 here so the second
// delivery is rejected while the first handler is still working; the default
// is 1024.
func ExampleRuntimeOptions_deferredDispatch() {
	opts := chat.DefaultRuntimeOptions()
	opts.Dispatch = chat.DispatchDeferred
	opts.DetachTimeout = time.Minute // each handler's budget after the acknowledgement
	opts.MaxDetached = 1

	ctx := context.Background()
	bot, err := chat.New(ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(&demoAdapter{}),
		chat.WithRuntimeOptions(opts),
	)
	if err != nil {
		log.Fatal(err)
	}

	release := make(chan struct{})
	done := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		defer close(done)
		select {
		case <-release: // stands in for slow work, such as an LLM call
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		_, err := ev.Thread.Post(ctx, chat.Text("Summary ready."))
		return err
	})

	webhook, err := bot.Webhook("demo")
	if err != nil {
		log.Fatal(err)
	}
	send := func(query string) int {
		rec := httptest.NewRecorder()
		webhook.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook?"+query, nil))
		return rec.Code
	}

	// The first delivery is acknowledged while its handler keeps running.
	fmt.Println("first delivery:", send("thread=general&user=alice&text=summarize&mention"))
	// The second arrives at the admission bound. It is rejected before
	// acknowledgement and before dedupe, so the platform's retry is accepted.
	fmt.Println("second delivery:", send("thread=random&user=bob&text=help&mention"))

	close(release)
	<-done
	if err := bot.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	// Output:
	// first delivery: 200
	// second delivery: 503
	// [general] bot: Summary ready.
}

// Look up an optional capability before using it. The demo adapter
// implements chat.HistoryReader but not chat.EphemeralPoster.
func ExampleAdapterAs() {
	ctx := context.Background()
	bot, err := chat.New(ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(&demoAdapter{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	_, ok := chat.AdapterAs[chat.EphemeralPoster](bot, "demo")
	fmt.Println("ephemeral messages supported:", ok)

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		reader, ok := chat.AdapterAs[chat.HistoryReader](bot, ev.Event.Adapter)
		if !ok {
			_, err := ev.Thread.Post(ctx, chat.Text("I can't read this thread's history."))
			return err
		}
		history, err := reader.ReadHistory(ctx, ev.Thread.ID(), chat.HistoryQuery{Limit: 20})
		if err != nil {
			return err
		}
		// Ordering and pagination are adapter-owned; see the adapter's GoDoc.
		_, err = ev.Thread.Post(ctx, chat.Text(fmt.Sprintf("I read %d earlier messages.", len(history))))
		return err
	})

	webhook, err := bot.Webhook("demo")
	if err != nil {
		log.Fatal(err)
	}
	send := func(query string) int {
		rec := httptest.NewRecorder()
		webhook.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook?"+query, nil))
		return rec.Code
	}

	send("thread=general&user=alice&text=catch+me+up&mention")
	if err := bot.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	// Output:
	// ephemeral messages supported: false
	// [general] bot: I read 2 earlier messages.
}
