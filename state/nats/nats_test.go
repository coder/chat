package nats_test

import (
	"context"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/coder/chat/internal/statetest"
	chatnats "github.com/coder/chat/state/nats"
)

func TestStateConformance(t *testing.T) {
	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()

		nc := newEmbeddedConn(t)

		// Bucket TTL must equal ShortTTL; ExpiryWait must exceed it plus the
		// JetStream age-reaper interval (~1s) for an expired key to be reapable.
		ttl := 1 * time.Second
		state, err := chatnats.New(context.Background(), chatnats.Options{
			Conn:          nc,
			Prefix:        "test",
			DedupeTTL:     ttl,
			ThreadLockTTL: ttl,
		})
		if err != nil {
			t.Fatalf("new nats state: %v", err)
		}
		t.Cleanup(func() {
			_ = state.Shutdown(context.Background())
		})

		return statetest.Harness{
			State:      state,
			ShortTTL:   ttl,
			ExpiryWait: 3 * time.Second,
		}
	})
}

func TestNewRejectsBucketTTLChange(t *testing.T) {
	nc := newEmbeddedConn(t)
	t.Cleanup(nc.Close)
	ctx := context.Background()

	opts := chatnats.Options{Conn: nc, Prefix: "ttltest", DedupeTTL: time.Second, ThreadLockTTL: time.Second}
	if _, err := chatnats.New(ctx, opts); err != nil {
		t.Fatalf("first New: %v", err)
	}

	if _, err := chatnats.New(ctx, opts); err != nil {
		t.Fatalf("re-New with same TTL: %v", err)
	}

	changed := opts
	changed.DedupeTTL = 2 * time.Second
	_, err := chatnats.New(ctx, changed)
	if err == nil {
		t.Fatal("expected error when reconstructing with a changed DedupeTTL")
	}
	if !strings.Contains(err.Error(), "delete the bucket to change its TTL") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := chatnats.New(ctx, chatnats.Options{Conn: nc, Prefix: "ttltest"}); err == nil {
		t.Fatal("expected error when reconstructing with default TTLs over short-TTL buckets")
	}
}

// newEmbeddedConn returns a connection to an in-process JetStream server; the
// connection is left open for State.Shutdown to close.
func newEmbeddedConn(t *testing.T) *natsgo.Conn {
	t.Helper()

	ns, err := natsserver.NewServer(&natsserver.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready for connections")
	}

	nc, err := natsgo.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect to nats server: %v", err)
	}
	return nc
}
