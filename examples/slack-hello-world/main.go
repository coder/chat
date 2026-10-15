package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mustAllowDemoMemoryState()

	opts := slack.Options{
		AppToken:      os.Getenv("SLACK_APP_TOKEN"),
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BotToken:      mustEnv("SLACK_BOT_TOKEN"),
		Logger:        slog.Default(),
	}
	bot, err := newBot(ctx, opts)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			slog.Error("chat shutdown failed", "error", err)
		}
	}()

	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	if err := serve(ctx, bot, opts, addr); err != nil {
		panic(err)
	}
}

func newBot(ctx context.Context, opts slack.Options) (*chat.Chat, error) {
	slackAdapter, err := slack.New(ctx, opts)
	if err != nil {
		return nil, err
	}

	bot, err := chat.New(
		ctx,
		chat.WithState(memory.New()),
		chat.WithAdapter(slackAdapter),
		chat.WithLogger(opts.Logger),
	)
	if err != nil {
		return nil, err
	}

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Markdown("**hello** _world_"))
		return err
	})
	return bot, nil
}

// serve receives Slack events until ctx is done or a transport fails. It runs
// Socket Mode when opts has an app token, and the webhook server on addr when
// opts has a signing secret. When one transport stops, serve stops the other.
func serve(ctx context.Context, bot *chat.Chat, opts slack.Options, addr string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var transports []func() error
	if opts.AppToken != "" {
		transports = append(transports, func() error {
			return bot.Listen(ctx, "slack")
		})
	}
	if opts.SigningSecret != "" {
		slackWebhook, err := bot.Webhook("slack")
		if err != nil {
			return err
		}
		server := newWebhookServer(addr, slackWebhook)
		transports = append(transports, func() error {
			return runWebhookServer(ctx, server)
		})
	}

	errs := make(chan error, len(transports))
	for _, transport := range transports {
		go func() {
			defer cancel()
			errs <- transport()
		}()
	}
	var err error
	for range transports {
		err = errors.Join(err, <-errs)
	}
	return err
}

// runWebhookServer serves until ctx is done, and then shuts the server down.
func runWebhookServer(ctx context.Context, server *http.Server) error {
	slog.Info("serving Slack webhooks", "addr", server.Addr)
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
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
