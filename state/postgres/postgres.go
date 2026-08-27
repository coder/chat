package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coder/chat"
	"github.com/coder/chat/internal/tokens"
)

type Options struct {
	Pool      *pgxpool.Pool
	Namespace string
}

type State struct {
	pool      *pgxpool.Pool
	namespace string
	once      sync.Once
}

var _ chat.State = (*State)(nil)

func New(ctx context.Context, opts Options) (*State, error) {
	if opts.Pool == nil {
		return nil, errors.New("postgres state: pool is required")
	}
	namespace := strings.TrimSpace(opts.Namespace)
	if namespace == "" {
		namespace = "chat"
	}
	state := &State{
		pool:      opts.Pool,
		namespace: namespace,
	}
	if err := state.pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres state: ping: %w", err)
	}
	if err := state.setup(ctx); err != nil {
		return nil, fmt.Errorf("postgres state: setup: %w", err)
	}
	return state, nil
}

func (s *State) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if id == "" {
		return false, errors.New("postgres state: thread id is required")
	}
	var subscribed bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM chat_runtime_subscriptions
			WHERE namespace = $1 AND thread_id = $2
		)
	`, s.namespace, string(id)).Scan(&subscribed)
	if err != nil {
		return false, err
	}
	return subscribed, nil
}

func (s *State) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("postgres state: thread id is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_runtime_subscriptions (namespace, thread_id)
		VALUES ($1, $2)
		ON CONFLICT (namespace, thread_id) DO NOTHING
	`, s.namespace, string(id))
	return err
}

func (s *State) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("postgres state: thread id is required")
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM chat_runtime_subscriptions
		WHERE namespace = $1 AND thread_id = $2
	`, s.namespace, string(id))
	return err
}

func (s *State) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if id == "" {
		return false, errors.New("postgres state: event id is required")
	}
	ttlMicros, err := positiveMicros(ttl, "dedupe ttl")
	if err != nil {
		return false, err
	}
	var first bool
	err = s.pool.QueryRow(ctx, `
		WITH marked AS (
			INSERT INTO chat_runtime_events (namespace, event_id, expires_at)
			VALUES ($1, $2, now() + ($3::bigint * interval '1 microsecond'))
			ON CONFLICT (namespace, event_id) DO UPDATE
			SET expires_at = EXCLUDED.expires_at
			WHERE chat_runtime_events.expires_at <= now()
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM marked)
	`, s.namespace, id, ttlMicros).Scan(&first)
	if err != nil {
		return false, err
	}
	return first, nil
}

func (s *State) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	if key == "" {
		return chat.LockLease{}, false, errors.New("postgres state: lock key is required")
	}
	ttlMicros, err := positiveMicros(ttl, "lock ttl")
	if err != nil {
		return chat.LockLease{}, false, err
	}
	token, err := tokens.New()
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("postgres state: create lock token: %w", err)
	}
	var acquired bool
	err = s.pool.QueryRow(ctx, `
		WITH acquired AS (
			INSERT INTO chat_runtime_locks (namespace, lock_key, token, expires_at)
			VALUES ($1, $2, $3, now() + ($4::bigint * interval '1 microsecond'))
			ON CONFLICT (namespace, lock_key) DO UPDATE
			SET token = EXCLUDED.token, expires_at = EXCLUDED.expires_at
			WHERE chat_runtime_locks.expires_at <= now()
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM acquired)
	`, s.namespace, key, token, ttlMicros).Scan(&acquired)
	if err != nil {
		return chat.LockLease{}, false, err
	}
	if !acquired {
		return chat.LockLease{}, false, nil
	}
	return chat.LockLease{Key: key, Token: token}, true, nil
}

func (s *State) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("postgres state: lock lease is required")
	}
	ttlMicros, err := positiveMicros(ttl, "lock ttl")
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE chat_runtime_locks
		SET expires_at = now() + ($4::bigint * interval '1 microsecond')
		WHERE namespace = $1 AND lock_key = $2 AND token = $3 AND expires_at > now()
	`, s.namespace, lease.Key, lease.Token, ttlMicros)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *State) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("postgres state: lock lease is required")
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM chat_runtime_locks
		WHERE namespace = $1 AND lock_key = $2 AND token = $3 AND expires_at > now()
	`, s.namespace, lease.Key, lease.Token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *State) Shutdown(context.Context) error {
	s.once.Do(func() {
		s.pool.Close()
	})
	return nil
}

func (s *State) setup(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "chat_runtime_state_schema"); err != nil {
		return err
	}
	for _, statement := range schemaStatements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func positiveMicros(ttl time.Duration, name string) (int64, error) {
	if ttl <= 0 {
		return 0, fmt.Errorf("postgres state: %s must be positive", name)
	}
	micros := ttl.Microseconds()
	if micros == 0 {
		return 1, nil
	}
	return micros, nil
}

var schemaStatements = []string{
	`
	CREATE TABLE IF NOT EXISTS chat_runtime_subscriptions (
		namespace text NOT NULL,
		thread_id text NOT NULL,
		PRIMARY KEY (namespace, thread_id)
	)
	`,
	`
	CREATE TABLE IF NOT EXISTS chat_runtime_events (
		namespace text NOT NULL,
		event_id text NOT NULL,
		expires_at timestamptz NOT NULL,
		PRIMARY KEY (namespace, event_id)
	)
	`,
	`
	CREATE TABLE IF NOT EXISTS chat_runtime_locks (
		namespace text NOT NULL,
		lock_key text NOT NULL,
		token text NOT NULL,
		expires_at timestamptz NOT NULL,
		PRIMARY KEY (namespace, lock_key)
	)
	`,
}
