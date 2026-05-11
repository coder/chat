package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/coder/chat/internal/statetest"
	chatpostgres "github.com/coder/chat/state/postgres"
)

func TestStateConformance(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("chat_sdk_go"),
		tcpostgres.WithUsername("chat_sdk_go"),
		tcpostgres.WithPassword("chat_sdk_go"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	databaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres container connection string: %v", err)
	}

	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		pool, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			t.Fatalf("new postgres pool: %v", err)
		}

		namespace := testNamespace(t)
		state, err := chatpostgres.New(ctx, chatpostgres.Options{
			Pool:      pool,
			Namespace: namespace,
		})
		if err != nil {
			t.Fatalf("new postgres state: %v", err)
		}

		t.Cleanup(func() {
			_ = state.Shutdown(context.Background())
		})

		return statetest.Harness{
			State:      state,
			ShortTTL:   500 * time.Millisecond,
			ExpiryWait: 750 * time.Millisecond,
		}
	})
}

func testNamespace(t *testing.T) string {
	t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(t.Name())
	return "test_" + name + "_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
}
