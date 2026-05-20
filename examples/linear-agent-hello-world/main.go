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

	bot.OnNewMention(newMentionHandler(linearAccess))

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
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

type linearThoughtPoster interface {
	PostThought(context.Context, chat.ThreadID, string) (*chat.SentMessage, error)
}

func newMentionHandler(linearAccess linearThoughtPoster) chat.MessageHandler {
	return func(ctx context.Context, ev *chat.MessageEvent) error {
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		_, _ = linearAccess.PostThought(ctx, ev.Thread.ID(), "Thinking...")
		_, err := ev.Thread.Post(ctx, chat.Markdown(
			"**hello from Linear app actor**\n\nI subscribed to this agent session. Send a follow-up prompt to test the subscribed route.",
		))
		if err != nil {
			return errors.Join(err, ev.Thread.Unsubscribe(context.WithoutCancel(ctx)))
		}
		return nil
	}
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
