package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coder/chat/internal/statetest"
	chatpostgres "github.com/coder/chat/state/postgres"
)

func TestStateConformance(t *testing.T) {
	databaseURL := os.Getenv("CHAT_POSTGRES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set CHAT_POSTGRES_TEST_DATABASE_URL to run postgres state conformance tests")
	}

	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			defer func() {
				_ = state.Shutdown(context.Background())
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cleanupNamespace(ctx, t, pool, namespace)
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

func cleanupNamespace(ctx context.Context, t *testing.T, pool *pgxpool.Pool, namespace string) {
	t.Helper()

	for _, table := range []string{
		"chat_runtime_subscriptions",
		"chat_runtime_events",
		"chat_runtime_locks",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE namespace = $1", namespace); err != nil {
			t.Fatalf("cleanup postgres namespace %q in %s: %v", namespace, table, err)
		}
	}
}
