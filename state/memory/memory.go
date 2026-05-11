package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/internal/tokens"
)

type State struct {
	mu         sync.Mutex
	subscribed map[chat.ThreadID]bool
	events     map[string]time.Time
	locks      map[string]lockRecord
	closed     bool
}

type lockRecord struct {
	token  string
	expiry time.Time
}

func New() *State {
	return &State{
		subscribed: map[chat.ThreadID]bool{},
		events:     map[string]time.Time{},
		locks:      map[string]lockRecord{},
	}
}

func (s *State) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if err := s.beforeOperation(ctx); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribed[id], nil
}

func (s *State) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("memory state: thread id is required")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribed[id] = true
	return nil
}

func (s *State) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("memory state: thread id is required")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribed, id)
	return nil
}

func (s *State) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if id == "" {
		return false, errors.New("memory state: event id is required")
	}
	if ttl <= 0 {
		return false, errors.New("memory state: dedupe ttl must be positive")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return false, err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if expiry, ok := s.events[id]; ok && expiry.After(now) {
		return false, nil
	}
	s.events[id] = now.Add(ttl)
	return true, nil
}

func (s *State) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	if key == "" {
		return chat.LockLease{}, false, errors.New("memory state: lock key is required")
	}
	if ttl <= 0 {
		return chat.LockLease{}, false, errors.New("memory state: lock ttl must be positive")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return chat.LockLease{}, false, err
	}

	token, err := tokens.New()
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("memory state: create lock token: %w", err)
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if held, ok := s.locks[key]; ok && held.expiry.After(now) {
		return chat.LockLease{}, false, nil
	}
	lease := chat.LockLease{Key: key, Token: token}
	s.locks[key] = lockRecord{token: token, expiry: now.Add(ttl)}
	return lease, true, nil
}

func (s *State) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("memory state: lock lease is required")
	}
	if ttl <= 0 {
		return false, errors.New("memory state: lock ttl must be positive")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return false, err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	held, ok := s.locks[lease.Key]
	if !ok || held.token != lease.Token || !held.expiry.After(now) {
		return false, nil
	}
	s.locks[lease.Key] = lockRecord{token: lease.Token, expiry: now.Add(ttl)}
	return true, nil
}

func (s *State) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("memory state: lock lease is required")
	}
	if err := s.beforeOperation(ctx); err != nil {
		return false, err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	held, ok := s.locks[lease.Key]
	if !ok || held.token != lease.Token || !held.expiry.After(now) {
		return false, nil
	}
	delete(s.locks, lease.Key)
	return true, nil
}

func (s *State) Shutdown(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *State) beforeOperation(ctx context.Context) error {
	if s == nil {
		return errors.New("memory state: nil state")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("memory state: closed")
	}
	return nil
}
