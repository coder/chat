package slackapitest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// coreRecordingTB records Errorf calls instead of failing the test.
type coreRecordingTB struct {
	testing.TB

	mu     sync.Mutex
	errors []string
}

func (tb *coreRecordingTB) Errorf(format string, args ...any) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.errors = append(tb.errors, format)
}

func (tb *coreRecordingTB) errorCount() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return len(tb.errors)
}

// corePost sends body to method on srv and returns the response and its body.
func corePost(t *testing.T, srv *slackapitest.Server, method, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL()+"/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer xoxb-test")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", method, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	return resp, string(payload)
}

func TestRespond(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp any
		want string
	}{
		{name: "value", resp: map[string]any{"ok": true, "ts": "1.2"}, want: `{"ok":true,"ts":"1.2"}`},
		{name: "raw message", resp: json.RawMessage(`{"ok": true}`), want: `{"ok": true}`},
		{name: "bytes", resp: []byte("ok"), want: "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("chat.postMessage", tt.resp)
			for range 2 {
				resp, body := corePost(t, srv, "chat.postMessage", "application/json", `{}`)
				if resp.StatusCode != http.StatusOK || body != tt.want {
					t.Fatalf("response = %d %q, want 200 %q", resp.StatusCode, body, tt.want)
				}
				if got := resp.Header.Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q", got)
				}
			}
		})
	}
}

func TestHandleResponse(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Handle("chat.postMessage", func(call slackapitest.Call) any {
		if call.Header.Get("Authorization") == "" {
			return slackapitest.Response{StatusCode: http.StatusUnauthorized}
		}
		return slackapitest.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"7"}},
			Body:       map[string]any{"ok": false, "error": "ratelimited"},
		}
	})

	resp, body := corePost(t, srv, "chat.postMessage", "application/json", `{}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	if body != `{"error":"ratelimited","ok":false}` {
		t.Fatalf("body = %q", body)
	}
}

func TestCalls(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", json.RawMessage(`{"ok":true}`))
	srv.Respond("files.getUploadURLExternal", json.RawMessage(`{"ok":true}`))

	corePost(t, srv, "chat.postMessage", "application/json", `{"channel":"C1"}`)
	corePost(t, srv, "files.getUploadURLExternal", "application/x-www-form-urlencoded; charset=utf-8", "filename=a.txt&length=3")

	posts := srv.Calls("chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("chat.postMessage calls = %d, want 1", len(posts))
	}
	post := posts[0]
	if post.Method != "chat.postMessage" || string(post.Body) != `{"channel":"C1"}` || post.Form != nil {
		t.Fatalf("JSON call = %+v", post)
	}
	if got := post.Header.Get("Authorization"); got != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q", got)
	}

	uploads := srv.Calls("files.getUploadURLExternal")
	if len(uploads) != 1 {
		t.Fatalf("files.getUploadURLExternal calls = %d, want 1", len(uploads))
	}
	want := url.Values{"filename": {"a.txt"}, "length": {"3"}}
	if got := uploads[0].Form; got.Encode() != want.Encode() {
		t.Fatalf("Form = %v, want %v", got, want)
	}
	if string(uploads[0].Body) != "filename=a.txt&length=3" {
		t.Fatalf("form Body = %q", uploads[0].Body)
	}

	if got := srv.Calls("auth.test"); len(got) != 0 {
		t.Fatalf("auth.test calls = %v, want none", got)
	}
}

func TestCallsConcurrent(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("reactions.add", json.RawMessage(`{"ok":true}`))

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			corePost(t, srv, "reactions.add", "application/json", `{}`)
			_ = srv.Calls("reactions.add")
		})
	}
	wg.Wait()
	if got := len(srv.Calls("reactions.add")); got != n {
		t.Fatalf("calls = %d, want %d", got, n)
	}
}

func TestUnknownMethod(t *testing.T) {
	t.Parallel()

	tb := &coreRecordingTB{TB: t}
	srv := slackapitest.NewServer(tb)

	resp, body := corePost(t, srv, "chat.unknown", "application/json", `{}`)
	if resp.StatusCode != http.StatusOK || body != `{"ok":false,"error":"unknown_method"}` {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if got := tb.errorCount(); got != 1 {
		t.Fatalf("Errorf calls = %d, want 1", got)
	}
	if got := len(srv.Calls("chat.unknown")); got != 1 {
		t.Fatalf("calls = %d, want 1 (unknown calls are recorded)", got)
	}
}
