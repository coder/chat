package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

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

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		_, _ = linearAccess.PostThought(ctx, ev.Thread.ID(), "Thinking...")
		_, err := ev.Thread.Post(ctx, chat.Markdown(
			"**hello from Linear app actor**\n\nI subscribed to this agent session. Send a follow-up prompt to test the subscribed route.",
		))
		return err
	})

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, _ = linearAccess.PostThought(ctx, ev.Thread.ID(), "Reading your follow-up...")
		_, err := ev.Thread.Post(ctx, chat.Text("Follow-up received: "+ev.Message.Text))
		return err
	})

	linearWebhook, err := bot.Webhook("linear")
	if err != nil {
		panic(err)
	}
	http.Handle("/webhooks/linear", linearWebhook)

	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		panic(err)
	}
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		panic(name + " is required")
	}
	return value
}
