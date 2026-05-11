package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	chatpostgres "github.com/coder/chat/state/postgres"
)

func main() {
	ctx := context.Background()
	slogLogger := slog.Default()

	pool, err := pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		panic(err)
	}

	postgresState, err := chatpostgres.New(ctx, chatpostgres.Options{
		Pool:      pool,
		Namespace: "slack-example",
	})
	if err != nil {
		panic(err)
	}

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
		chat.WithState(postgresState),
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
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		_, err := ev.Thread.Post(ctx, chat.Markdown("**hello** _world_ from Postgres state. This thread is now subscribed."))
		return err
	})

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Markdown("Postgres remembered this subscribed thread."))
		return err
	})

	slackWebhook, err := bot.Webhook("slack")
	if err != nil {
		panic(err)
	}

	http.Handle("/webhooks/slack", slackWebhook)

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
