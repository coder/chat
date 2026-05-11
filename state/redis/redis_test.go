package redis_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

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
