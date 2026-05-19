package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewWebhookServer(t *testing.T) {
	server := newWebhookServer(":0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if server.Addr != ":0" {
		t.Fatalf("addr = %q, want :0", server.Addr)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("read header timeout = %s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 10*time.Second {
		t.Fatalf("read timeout = %s", server.ReadTimeout)
	}
	if server.WriteTimeout != 10*time.Second {
		t.Fatalf("write timeout = %s", server.WriteTimeout)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("idle timeout = %s", server.IdleTimeout)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", nil)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestMustAllowDemoMemoryStateRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("CHAT_DEMO_IN_MEMORY_STATE", "")

	defer func() {
		if recovered := recover(); recovered != "CHAT_DEMO_IN_MEMORY_STATE=1 is required" {
			t.Fatalf("panic = %v, want CHAT_DEMO_IN_MEMORY_STATE=1 is required", recovered)
		}
	}()

	mustAllowDemoMemoryState()
}

func TestMustAllowDemoMemoryStateAcceptsExplicitOptIn(t *testing.T) {
	t.Setenv("CHAT_DEMO_IN_MEMORY_STATE", "1")

	mustAllowDemoMemoryState()
}
