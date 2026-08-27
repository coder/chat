package nats

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/coder/chat"
	"github.com/coder/chat/internal/tokens"
)

// Options configures a JetStream-backed State. Expiry is governed by the
// per-bucket TTLs set here, not by the per-call ttl arguments to
// MarkEvent/AcquireLock/ExtendLock, which are validated but otherwise ignored;
// set DedupeTTL and ThreadLockTTL to match the runtime's options.
type Options struct {
	Conn          *natsgo.Conn
	Prefix        string
	DedupeTTL     time.Duration
	ThreadLockTTL time.Duration
}

type State struct {
	nc    *natsgo.Conn
	sub   jetstream.KeyValue
	event jetstream.KeyValue
	lock  jetstream.KeyValue
	once  sync.Once
}

var (
	_ chat.State      = (*State)(nil)
	_ chat.LockForcer = (*State)(nil)
)

func New(ctx context.Context, opts Options) (*State, error) {
	if opts.Conn == nil {
		return nil, errors.New("nats state: conn is required")
	}
	prefix := strings.TrimRight(strings.TrimSpace(opts.Prefix), "_")
	if prefix == "" {
		prefix = "chat"
	}
	if !validBucketToken(prefix) {
		return nil, fmt.Errorf("nats state: prefix %q contains invalid characters", prefix)
	}
	dedupeTTL := opts.DedupeTTL
	if dedupeTTL <= 0 {
		dedupeTTL = 24 * time.Hour
	}
	lockTTL := opts.ThreadLockTTL
	if lockTTL <= 0 {
		lockTTL = 2 * time.Minute
	}

	js, err := jetstream.New(opts.Conn)
	if err != nil {
		return nil, fmt.Errorf("nats state: jetstream: %w", err)
	}
	if _, err := js.AccountInfo(ctx); err != nil {
		return nil, fmt.Errorf("nats state: jetstream account: %w", err)
	}

	// History 1 keeps a single revision per key so MaxAge expiry clears the subject.
	s := &State{nc: opts.Conn}
	for _, b := range []struct {
		dst  *jetstream.KeyValue
		name string
		ttl  time.Duration
	}{
		{&s.sub, "sub", 0},
		{&s.event, "event", dedupeTTL},
		{&s.lock, "lock", lockTTL},
	} {
		kv, err := ensureBucket(ctx, js, jetstream.KeyValueConfig{
			Bucket:  prefix + "_" + b.name,
			History: 1,
			TTL:     b.ttl,
		})
		if err != nil {
			return nil, fmt.Errorf("nats state: setup %s bucket: %w", b.name, err)
		}
		*b.dst = kv
	}
	return s, nil
}

// ensureBucket creates cfg's bucket, opens it if it exists with a matching
// config, or fails if it exists with a different TTL or history (an existing
// bucket is never mutated).
func ensureBucket(ctx context.Context, js jetstream.JetStream, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := js.CreateKeyValue(ctx, cfg)
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, err
	}
	kv, err = js.KeyValue(ctx, cfg.Bucket)
	if err != nil {
		return nil, err
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return nil, err
	}
	if status.TTL() != cfg.TTL {
		return nil, fmt.Errorf("bucket %q exists with TTL %s but %s was configured; delete the bucket to change its TTL", cfg.Bucket, status.TTL(), cfg.TTL)
	}
	if status.History() != int64(cfg.History) {
		return nil, fmt.Errorf("bucket %q exists with history %d but %d was configured; delete the bucket to change its history", cfg.Bucket, status.History(), cfg.History)
	}
	return kv, nil
}

func (s *State) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if id == "" {
		return false, errors.New("nats state: thread id is required")
	}
	_, err := s.sub.Get(ctx, encodeKey(string(id)))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nats state: get subscription: %w", err)
	}
	return true, nil
}

func (s *State) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("nats state: thread id is required")
	}
	if _, err := s.sub.Put(ctx, encodeKey(string(id)), []byte{'1'}); err != nil {
		return fmt.Errorf("nats state: subscribe: %w", err)
	}
	return nil
}

func (s *State) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if id == "" {
		return errors.New("nats state: thread id is required")
	}
	if err := s.sub.Delete(ctx, encodeKey(string(id))); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("nats state: unsubscribe: %w", err)
	}
	return nil
}

func (s *State) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if id == "" {
		return false, errors.New("nats state: event id is required")
	}
	if ttl <= 0 {
		return false, errors.New("nats state: dedupe ttl must be positive")
	}
	_, err := s.event.Create(ctx, encodeKey(id), []byte{'1'})
	if errors.Is(err, jetstream.ErrKeyExists) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nats state: mark event: %w", err)
	}
	return true, nil
}

func (s *State) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	if key == "" {
		return chat.LockLease{}, false, errors.New("nats state: lock key is required")
	}
	if ttl <= 0 {
		return chat.LockLease{}, false, errors.New("nats state: lock ttl must be positive")
	}
	token, err := tokens.New()
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("nats state: create lock token: %w", err)
	}
	_, err = s.lock.Create(ctx, encodeKey(key), []byte(token))
	if errors.Is(err, jetstream.ErrKeyExists) {
		return chat.LockLease{}, false, nil
	}
	if err != nil {
		return chat.LockLease{}, false, fmt.Errorf("nats state: acquire lock: %w", err)
	}
	return chat.LockLease{Key: key, Token: token}, true, nil
}

func (s *State) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("nats state: lock lease is required")
	}
	if ttl <= 0 {
		return false, errors.New("nats state: lock ttl must be positive")
	}
	key := encodeKey(lease.Key)
	entry, err := s.lock.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nats state: extend lock get: %w", err)
	}
	if string(entry.Value()) != lease.Token {
		return false, nil
	}
	// A fresh write renews the lease by resetting the entry's age under the
	// bucket TTL; the revision gate fails it if we no longer hold the lock.
	if _, err := s.lock.Update(ctx, key, []byte(lease.Token), entry.Revision()); err != nil {
		if isWrongLastSequence(err) {
			return false, nil
		}
		return false, fmt.Errorf("nats state: extend lock update: %w", err)
	}
	return true, nil
}

// ForceReleaseLock invalidates the current lock for key regardless of owner
// (chat.LockForcer). The delete is gated by the revision observed in this
// call, so only the lease seen here is invalidated: a lease that changes hands
// between the get and the delete survives (reported as false), matching the
// single-statement atomicity of the Redis and Postgres implementations. The
// previous holder's ExtendLock and ReleaseLock fail cleanly on the vanished
// entry.
func (s *State) ForceReleaseLock(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("nats state: lock key is required")
	}
	encoded := encodeKey(key)
	entry, err := s.lock.Get(ctx, encoded)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nats state: force release lock get: %w", err)
	}
	if err := s.lock.Delete(ctx, encoded, jetstream.LastRevision(entry.Revision())); err != nil {
		if isWrongLastSequence(err) || errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("nats state: force release lock: %w", err)
	}
	return true, nil
}

func (s *State) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if lease.Key == "" || lease.Token == "" {
		return false, errors.New("nats state: lock lease is required")
	}
	key := encodeKey(lease.Key)
	entry, err := s.lock.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nats state: release lock get: %w", err)
	}
	if string(entry.Value()) != lease.Token {
		return false, nil
	}
	if err := s.lock.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
		if isWrongLastSequence(err) || errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("nats state: release lock: %w", err)
	}
	return true, nil
}

func (s *State) Shutdown(context.Context) error {
	s.once.Do(s.nc.Close)
	return nil
}

// encodeKey encodes arbitrary IDs (which may contain ':') into the NATS KV key
// charset, which forbids ':' and treats '.' as a subject separator.
func encodeKey(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func isWrongLastSequence(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

func validBucketToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
