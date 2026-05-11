package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx := context.Background()
	slogLogger := slog.Default()

	slackAdapter, err := slack.New(ctx, slack.Options{
		SigningSecret: mustEnv("SLACK_SIGNING_SECRET"),
		BotToken:      mustEnv("SLACK_BOT_TOKEN"),
		Logger:        slogLogger,
	})
	if err != nil {
		panic(err)
	}

	bot, err := chat.New(
		ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(slackAdapter),
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

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err = ev.Thread.Post(ctx, chat.Markdown("**hello** _world_"))
		return err
	})

	slackWebhook, err := bot.Webhook("slack")
	if err != nil {
		panic(err)
	}

	http.Handle("/webhooks/slack", slackWebhook)

	addr := ":" + envDefault("PORT", "8080")
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

func envDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}
