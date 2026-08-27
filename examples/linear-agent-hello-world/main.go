package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx := context.Background()
	slogLogger := slog.Default()

	linearAdapter, err := linear.New(ctx, linear.Options{
		WebhookSecret: mustEnv("LINEAR_WEBHOOK_SECRET"),
		ClientCredentials: linear.ClientCredentials{
			ClientID:     mustEnv("LINEAR_CLIENT_CREDENTIALS_CLIENT_ID"),
			ClientSecret: mustEnv("LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET"),
		},
		Logger: slogLogger,
	})
	if err != nil {
		panic(err)
	}

	bot, err := chat.New(
		ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(linearAdapter),
		chat.WithLogger(slogLogger),
		// Deferred dispatch (Ack-Then-Work) lets follow-up work outlive the inbound
		// webhook request and run inside Linear's ~30-minute follow-up window.
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     24 * time.Hour,
			ThreadLockTTL: 2 * time.Minute,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Minute,
		}),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			slog.Error("chat shutdown failed", "error", err)
		}
	}()

	linearAccess, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
	if !ok {
		panic("linear adapter is not registered")
	}

	pending := newPendingSelections()
	bot.OnNewMention(newMentionHandler(linearAccess, pending))

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		// A "comment"-kind thread is an ordinary issue comment (ADR 0013); an
		// "agent_session" thread is an agent session (ADR 0008). Thread.Post routes
		// by kind automatically.
		if stopped, err := confirmStop(ctx, ev); stopped {
			return err
		}
		// The answer to the mention handler's deploy elicitation arrives here as a
		// regular follow-up prompt — interpret it as a choice only while one is
		// pending on this thread (see capabilities.go). take is take-once: a
		// free-text follow-up also consumes the pending state, matching Linear
		// dismissing the elicitation UI.
		if optionValues, ok := pending.take(ev.Thread.ID()); ok {
			if handled, err := handleSelection(ctx, ev, optionValues); handled {
				return err
			}
		}
		_, _ = linearAccess.PostThought(ctx, ev.Thread.ID(), "Reading your follow-up...")
		_, err := ev.Thread.Post(ctx, chat.Text("Follow-up received: "+ev.Message.Text))
		return err
	})

	linearWebhook, err := bot.Webhook("linear")
	if err != nil {
		panic(err)
	}
	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	slog.Info("listening", "addr", addr)
	server := newWebhookServer(addr, linearWebhook)
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}

// linearAgentAccess is the Linear-specific surface this example reaches through
// Adapter Access. It is satisfied by *linear.Adapter and by a test fake.
type linearAgentAccess interface {
	PostThought(context.Context, chat.ThreadID, string) (*chat.SentMessage, error)
	PostAction(context.Context, chat.ThreadID, linear.ActionInput) (*chat.SentMessage, error)
	PostElicitation(context.Context, chat.ThreadID, linear.ElicitationInput) (*chat.SentMessage, error)
	PostError(context.Context, chat.ThreadID, linear.ErrorInput) (*chat.SentMessage, error)
	UpdateSession(context.Context, chat.ThreadID, linear.AgentSessionUpdateInput) error
	CreateSessionOnIssue(context.Context, linear.CreateSessionOnIssueInput) (*linear.CreatedAgentSession, error)
	SuggestRepositories(context.Context, chat.ThreadID, []linear.CandidateRepository) ([]linear.RepositorySuggestion, error)
}

func newMentionHandler(linearAccess linearAgentAccess, pending *pendingSelections) chat.MessageHandler {
	return func(ctx context.Context, ev *chat.MessageEvent) error {
		// On a comment thread (ADR 0013), reply with an ordinary comment.
		if raw, ok := linear.RawMessageFrom(ev.Message); ok && raw.Kind == "comment" {
			_, err := ev.Thread.Post(ctx, chat.Text("Thanks for the mention — replying as the app actor."))
			return err
		}

		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}

		// Ack-Then-Work: post a first thought inside the ~10s first-thought window,
		// then do the rest. Under DispatchDeferred this whole handler already runs on
		// the Detached Work Context after the webhook is acknowledged.
		_, _ = linearAccess.PostThought(ctx, ev.Thread.ID(), "Thinking...")

		// Show native tool-call progress as an action.
		_, _ = linearAccess.PostAction(ctx, ev.Thread.ID(), linear.ActionInput{Action: "search-codebase", Parameter: ev.Message.Text})

		// Publish an external link, which also keeps the session responsive.
		_ = linearAccess.UpdateSession(ctx, ev.Thread.ID(), linear.AgentSessionUpdateInput{
			ExternalURLs: []linear.ExternalURL{{URL: "https://example.com/pr/1", Label: "Draft PR"}},
		})

		// Ask the user to choose, completing the session via an elicitation, and
		// remember what was offered so only a pending thread's follow-up is
		// interpreted as an answer.
		if shouldAsk(ev.Message.Text) {
			_, err := linearAccess.PostElicitation(ctx, ev.Thread.ID(), linear.ElicitationInput{
				Body:           "Which environment should I target?",
				Signal:         "select",
				SignalMetadata: linear.SelectSignalMetadata{Options: []linear.SelectOption{{Value: "staging"}, {Value: "prod"}}},
			})
			if err == nil {
				pending.set(ev.Thread.ID(), []string{"staging", "prod"})
			}
			return err
		}

		_, err := ev.Thread.Post(ctx, chat.Markdown(
			"**hello from Linear app actor**\n\nI subscribed to this agent session. Send a follow-up prompt to test the subscribed route.",
		))
		if err != nil {
			// Ending a failed session cleanly with an error completion signal.
			_, _ = linearAccess.PostError(ctx, ev.Thread.ID(), linear.ErrorInput{Body: "failed to post response: " + err.Error()})
			return errors.Join(err, ev.Thread.Unsubscribe(context.WithoutCancel(ctx)))
		}
		return nil
	}
}

func shouldAsk(text string) bool {
	return text == "deploy"
}

func newWebhookServer(addr string, linearWebhook http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/webhooks/linear", linearWebhook)

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		panic(name + " is required")
	}
	return value
}
