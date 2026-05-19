package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/internal/tokens"
)

type State struct {
	mu         sync.Mutex
	subscribed map[chat.ThreadID]bool
	events     map[string]time.Time
	locks      map[string]lockRecord
	closed     atomic.Bool
	now        func() time.Time
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
		now:        time.Now,
	}
}

func (s *State) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if err := s.lockOperation(ctx); err != nil {
		return false, err
	}
	defer s.mu.Unlock()
	return s.subscribed[id], nil
}

func (s *State) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("memory state: thread id is required")
	}
	if err := s.lockOperation(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	s.subscribed[id] = true
	return nil
}

func (s *State) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("memory state: thread id is required")
	}
	if err := s.lockOperation(ctx); err != nil {
		return err
	}
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
	if err := s.lockOperation(ctx); err != nil {
		return false, err
	}
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredEvents(now)
	if _, ok := s.events[id]; ok {
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
	if err := s.checkOperation(ctx); err != nil {
		return chat.LockLease{}, false, err
	}

	token, err := tokens.New()
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("memory state: create lock token: %w", err)
	}

	if err := s.lockOperationAfterCheck(ctx); err != nil {
		return chat.LockLease{}, false, err
	}
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocks(now)
	if _, ok := s.locks[key]; ok {
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
	if err := s.lockOperation(ctx); err != nil {
		return false, err
	}
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocks(now)
	held, ok := s.locks[lease.Key]
	if !ok || held.token != lease.Token {
		return false, nil
	}
	s.locks[lease.Key] = lockRecord{token: lease.Token, expiry: now.Add(ttl)}
	return true, nil
}

func (s *State) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("memory state: lock lease is required")
	}
	if err := s.lockOperation(ctx); err != nil {
		return false, err
	}
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocks(now)
	held, ok := s.locks[lease.Key]
	if !ok || held.token != lease.Token {
		return false, nil
	}
	delete(s.locks, lease.Key)
	return true, nil
}

func (s *State) Shutdown(context.Context) error {
	s.closed.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil
}

func (s *State) lockOperation(ctx context.Context) error {
	if err := s.checkOperation(ctx); err != nil {
		return err
	}
	return s.lockOperationAfterCheck(ctx)
}

func (s *State) lockOperationAfterCheck(ctx context.Context) error {
	s.mu.Lock()
	if err := s.checkOperation(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *State) checkOperation(ctx context.Context) error {
	if s == nil {
		return errors.New("memory state: nil state")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errors.New("memory state: closed")
	}
	return nil
}

func (s *State) pruneExpiredEvents(now time.Time) {
	for id, expiry := range s.events {
		if !expiry.After(now) {
			delete(s.events, id)
		}
	}
}

func (s *State) pruneExpiredLocks(now time.Time) {
	for key, held := range s.locks {
		if !held.expiry.After(now) {
			delete(s.locks, key)
		}
	}
}
