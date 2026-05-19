package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx := context.Background()
	slogLogger := slog.Default()
	mustAllowDemoMemoryState()

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
		_, err := ev.Thread.Post(ctx, chat.Markdown("**hello** _world_"))
		return err
	})

	slackWebhook, err := bot.Webhook("slack")
	if err != nil {
		panic(err)
	}

	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	slog.Info("listening", "addr", addr)
	server := newWebhookServer(addr, slackWebhook)
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}

func newWebhookServer(addr string, slackWebhook http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/webhooks/slack", slackWebhook)

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

func mustAllowDemoMemoryState() {
	if os.Getenv("CHAT_DEMO_IN_MEMORY_STATE") != "1" {
		panic("CHAT_DEMO_IN_MEMORY_STATE=1 is required")
	}
}
