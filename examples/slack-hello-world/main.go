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
// opts has a signing secret. Then it shuts down the webhook server, and the
// runtime last. The webhook server goes first because Runtime Shutdown does
// not wait for webhook requests in the sync Dispatch Mode. Runtime Shutdown
// stops acking new Socket Mode events before it closes the connection.
func serve(ctx context.Context, bot *chat.Chat, opts slack.Options, addr string) error {
	var server *http.Server
	if opts.SigningSecret != "" {
		slackWebhook, err := bot.Webhook("slack")
		if err != nil {
			return err
		}
		server = newWebhookServer(addr, slackWebhook)
	}

	errs := make(chan error, 2)
	running := 0
	if opts.AppToken != "" {
		running++
		// Only Shutdown stops Listen, so the connection closes after
		// admission closes.
		go func() { errs <- bot.Listen(context.Background(), "slack") }()
	}
	if server != nil {
		running++
		go func() { errs <- serveWebhooks(server) }()
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errs:
		running--
	}
	if server != nil {
		err = errors.Join(err, shutdown(server.Shutdown))
	}
	err = errors.Join(err, shutdown(bot.Shutdown))
	for range running {
		err = errors.Join(err, <-errs)
	}
	return err
}

// shutdown runs stop with a context that ends after 10 seconds.
func shutdown(stop func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return stop(ctx)
}

// serveWebhooks serves until the server shuts down.
func serveWebhooks(server *http.Server) error {
	slog.Info("serving Slack webhooks", "addr", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
