package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ConcurrencyStrategy int

const (
	ConcurrencyDrop ConcurrencyStrategy = iota
)

type RuntimeOptions struct {
	DedupeTTL     time.Duration
	ThreadLockTTL time.Duration
	Concurrency   ConcurrencyStrategy
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		DedupeTTL:     24 * time.Hour,
		ThreadLockTTL: 2 * time.Minute,
		Concurrency:   ConcurrencyDrop,
	}
}

type Option func(*config)

type config struct {
	state    State
	adapters []Adapter
	logger   *slog.Logger
	options  RuntimeOptions
}

func WithState(state State) Option {
	return func(cfg *config) {
		cfg.state = state
	}
}

func WithAdapter(adapter Adapter) Option {
	return func(cfg *config) {
		cfg.adapters = append(cfg.adapters, adapter)
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		cfg.logger = logger
	}
}

func WithRuntimeOptions(options RuntimeOptions) Option {
	return func(cfg *config) {
		cfg.options = options
	}
}

type Chat struct {
	state    State
	adapters map[string]Adapter
	logger   *slog.Logger
	options  RuntimeOptions

	handlersMu        sync.RWMutex
	newMention        MessageHandler
	subscribedMessage MessageHandler
	shutdownMu        sync.Mutex
	shutdown          bool
}

func New(ctx context.Context, opts ...Option) (*Chat, error) {
	cfg := config{
		logger:  slog.Default(),
		options: DefaultRuntimeOptions(),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("chat: nil option")
		}
		opt(&cfg)
	}
	if cfg.state == nil {
		return nil, errors.New("chat: runtime state is required")
	}
	if len(cfg.adapters) == 0 {
		return nil, errors.New("chat: at least one adapter is required")
	}
	if cfg.logger == nil {
		return nil, errors.New("chat: logger is required")
	}
	if err := validateRuntimeOptions(cfg.options); err != nil {
		return nil, err
	}

	chat := &Chat{
		state:    cfg.state,
		adapters: map[string]Adapter{},
		logger:   cfg.logger,
		options:  cfg.options,
	}
	for _, adapter := range cfg.adapters {
		if adapter == nil {
			return nil, errors.New("chat: nil adapter")
		}
		name := adapter.Name()
		if name == "" {
			return nil, errors.New("chat: adapter name is required")
		}
		if _, exists := chat.adapters[name]; exists {
			return nil, fmt.Errorf("chat: adapter %q registered more than once", name)
		}
		if err := adapter.Init(ctx); err != nil {
			return nil, fmt.Errorf("chat: initialize adapter %q: %w", name, err)
		}
		chat.adapters[name] = adapter
	}
	return chat, nil
}

func validateRuntimeOptions(options RuntimeOptions) error {
	if options.DedupeTTL <= 0 {
		return errors.New("chat: dedupe ttl must be positive")
	}
	if options.ThreadLockTTL <= 0 {
		return errors.New("chat: thread lock ttl must be positive")
	}
	if options.Concurrency != ConcurrencyDrop {
		return errors.New("chat: only drop concurrency is implemented")
	}
	return nil
}

// OnNewMention installs or atomically replaces the single new-mention handler.
// This intentionally differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnNewMention(handler MessageHandler) {
	assert(c != nil, "OnNewMention called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.newMention = handler
}

// OnSubscribedMessage installs or atomically replaces the single subscribed-message handler.
// This intentionally differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnSubscribedMessage(handler MessageHandler) {
	assert(c != nil, "OnSubscribedMessage called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.subscribedMessage = handler
}

func (c *Chat) Webhook(adapterName string) (http.Handler, error) {
	assert(c != nil, "Webhook called on nil runtime")
	adapter, ok := c.adapters[adapterName]
	if !ok {
		return nil, fmt.Errorf("chat: adapter %q is not registered", adapterName)
	}
	return adapter.Webhook(c.dispatch), nil
}

func (c *Chat) Thread(ctx context.Context, id ThreadID) (*Thread, error) {
	assert(c != nil, "Thread called on nil runtime")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := adapterNameFromThreadID(id)
	if err != nil {
		return nil, err
	}
	adapter, ok := c.adapters[name]
	if !ok {
		return nil, fmt.Errorf("chat: adapter %q is not registered", name)
	}
	ref, err := adapter.ValidateThreadID(id)
	if err != nil {
		return nil, fmt.Errorf("chat: validate thread id: %w", err)
	}
	return c.newThread(adapter, ref), nil
}

func AdapterAs[T any](c *Chat, adapterName string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	adapter, ok := c.adapters[adapterName]
	if !ok {
		return zero, false
	}
	typed, ok := adapter.(T)
	return typed, ok
}

func (c *Chat) Shutdown(ctx context.Context) error {
	assert(c != nil, "Shutdown called on nil runtime")
	c.shutdownMu.Lock()
	if c.shutdown {
		c.shutdownMu.Unlock()
		return nil
	}
	c.shutdown = true
	c.shutdownMu.Unlock()

	var errs []error
	for name, adapter := range c.adapters {
		if err := adapter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown adapter %q: %w", name, err))
		}
	}
	if err := c.state.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown state: %w", err))
	}
	return errors.Join(errs...)
}

func (c *Chat) dispatch(ctx context.Context, event *Event) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	adapter, ok := c.adapters[event.Adapter]
	if !ok {
		return fmt.Errorf("chat: event adapter %q is not registered", event.Adapter)
	}

	firstSeen, err := c.state.MarkEvent(ctx, event.ID, c.options.DedupeTTL)
	if err != nil {
		return fmt.Errorf("chat: mark event: %w", err)
	}
	if !firstSeen {
		c.logger.Info("chat duplicate event dropped", "adapter", event.Adapter, "event_id", event.ID)
		return nil
	}

	lease, acquired, err := c.state.AcquireLock(ctx, string(event.ThreadID), c.options.ThreadLockTTL)
	if err != nil {
		return fmt.Errorf("chat: acquire thread lock: %w", err)
	}
	if !acquired {
		c.logger.Info("chat lock conflict dropped", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
		return nil
	}
	defer func() {
		if released, err := c.state.ReleaseLock(context.WithoutCancel(ctx), lease); err != nil {
			c.logger.Error("chat release thread lock failed", "error", err, "thread_id", event.ThreadID)
		} else if !released {
			c.logger.Warn("chat thread lock was not released", "thread_id", event.ThreadID)
		}
	}()

	ref, err := adapter.ValidateThreadID(event.ThreadID)
	if err != nil {
		return fmt.Errorf("chat: validate event thread id: %w", err)
	}
	thread := c.newThread(adapter, ref)
	if event.Message == nil {
		c.logger.Info("chat ignored non-message event", "adapter", event.Adapter, "event_id", event.ID)
		return nil
	}
	if isSelfMessage(event.Message.Author, adapter.BotActor()) {
		c.logger.Debug("chat ignored self message", "adapter", event.Adapter, "event_id", event.ID)
		return nil
	}

	handler, route, err := c.route(ctx, event)
	if err != nil {
		return err
	}
	if handler == nil {
		c.logger.Info("chat ignored unrouted message", "adapter", event.Adapter, "event_id", event.ID)
		return nil
	}

	msgEvent := &MessageEvent{
		Event:   event,
		Thread:  thread,
		Message: event.Message,
	}
	if err := handler(ctx, msgEvent); err != nil {
		c.logger.Error("chat handler failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "route", route)
	}
	return nil
}

func validateEvent(event *Event) error {
	if event == nil {
		return errors.New("chat: nil event")
	}
	if event.ID == "" {
		return errors.New("chat: event id is required")
	}
	if event.Adapter == "" {
		return errors.New("chat: event adapter is required")
	}
	if event.ThreadID == "" {
		return errors.New("chat: event thread id is required")
	}
	return nil
}

func (c *Chat) route(ctx context.Context, event *Event) (MessageHandler, string, error) {
	subscribed, err := c.state.IsThreadSubscribed(ctx, event.ThreadID)
	if err != nil {
		return nil, "", fmt.Errorf("chat: check subscription: %w", err)
	}

	c.handlersMu.RLock()
	defer c.handlersMu.RUnlock()

	if subscribed {
		return c.subscribedMessage, "subscribed-message", nil
	}
	if event.DirectMessage || event.Message.Mentioned {
		return c.newMention, "new-mention", nil
	}
	return nil, "", nil
}

func adapterNameFromThreadID(id ThreadID) (string, error) {
	name, _, ok := strings.Cut(string(id), ":")
	if !ok || name == "" {
		return "", fmt.Errorf("chat: malformed thread id %q", id)
	}
	return name, nil
}

func isSelfMessage(author Actor, bot Actor) bool {
	return author.BotKind == BotBot &&
		author.Adapter == bot.Adapter &&
		author.Tenant == bot.Tenant &&
		author.ID == bot.ID
}
