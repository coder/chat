package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/coder/chat"
)

type Options struct {
	Client redis.UniversalClient
	Prefix string
}

type State struct {
	client redis.UniversalClient
	prefix string
	once   sync.Once
}

func New(ctx context.Context, opts Options) (*State, error) {
	if opts.Client == nil {
		return nil, errors.New("redis state: client is required")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "chat"
	}
	state := &State{
		client: opts.Client,
		prefix: strings.TrimRight(prefix, ":"),
	}
	if err := state.client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis state: ping: %w", err)
	}
	return state, nil
}

func (s *State) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if id == "" {
		return false, errors.New("redis state: thread id is required")
	}
	count, err := s.client.Exists(ctx, s.key("sub", string(id))).Result()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func (s *State) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("redis state: thread id is required")
	}
	return s.client.Set(ctx, s.key("sub", string(id)), "1", 0).Err()
}

func (s *State) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("redis state: thread id is required")
	}
	return s.client.Del(ctx, s.key("sub", string(id))).Err()
}

func (s *State) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if id == "" {
		return false, errors.New("redis state: event id is required")
	}
	if ttl <= 0 {
		return false, errors.New("redis state: dedupe ttl must be positive")
	}
	return s.client.SetNX(ctx, s.key("event", id), "1", ttl).Result()
}

func (s *State) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	if key == "" {
		return chat.LockLease{}, false, errors.New("redis state: lock key is required")
	}
	if ttl <= 0 {
		return chat.LockLease{}, false, errors.New("redis state: lock ttl must be positive")
	}
	token, err := newToken()
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("redis state: create lock token: %w", err)
	}
	acquired, err := s.client.SetNX(ctx, s.key("lock", key), token, ttl).Result()
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
		return false, errors.New("redis state: lock lease is required")
	}
	if ttl <= 0 {
		return false, errors.New("redis state: lock ttl must be positive")
	}
	result, err := s.client.Eval(ctx, extendScript, []string{s.key("lock", lease.Key)}, lease.Token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (s *State) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("redis state: lock lease is required")
	}
	result, err := s.client.Eval(ctx, releaseScript, []string{s.key("lock", lease.Key)}, lease.Token).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (s *State) Shutdown(context.Context) error {
	var err error
	s.once.Do(func() {
		err = s.client.Close()
	})
	return err
}

func (s *State) key(kind string, value string) string {
	return s.prefix + ":" + kind + ":" + value
}

func newToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

const extendScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`

const releaseScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`
