package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/coder/chat/internal/statetest"
	chatredis "github.com/coder/chat/state/redis"
)

func TestStateConformance(t *testing.T) {
	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()

		server := miniredis.RunT(t)
		client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
		t.Cleanup(func() {
			_ = client.Close()
		})

		state, err := chatredis.New(context.Background(), chatredis.Options{
			Client: client,
			Prefix: "test",
		})
		if err != nil {
			t.Fatalf("new redis state: %v", err)
		}
		return statetest.Harness{
			State:       state,
			AdvanceTime: server.FastForward,
		}
	})
}

func TestStateConformanceWithRedisContainer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7.2-alpine")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis container connection string: %v", err)
	}
	redisOptions, err := goredis.ParseURL(uri)
	if err != nil {
		t.Fatalf("parse redis URL: %v", err)
	}

	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()

		options := *redisOptions
		client := goredis.NewClient(&options)
		t.Cleanup(func() {
			_ = client.Close()
		})

		state, err := chatredis.New(context.Background(), chatredis.Options{
			Client: client,
			Prefix: "test:" + t.Name(),
		})
		if err != nil {
			t.Fatalf("new redis state: %v", err)
		}
		return statetest.Harness{
			State:      state,
			ShortTTL:   500 * time.Millisecond,
			ExpiryWait: 750 * time.Millisecond,
		}
	})
}
