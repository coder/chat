package slackapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// coreNewClient returns a client for srv with token xoxb-core and opts applied.
func coreNewClient(srv *slackapitest.Server, opts slackapi.Options) *slackapi.Client {
	opts.BaseURL = srv.URL()
	opts.HTTPClient = srv.Client()
	if opts.Token == "" {
		opts.Token = "xoxb-core"
	}
	return slackapi.New(opts)
}

// coreFastPolicy retries with millisecond delays so tests stay fast.
func coreFastPolicy(maxAttempts int) slackapi.RetryPolicy {
	return slackapi.RetryPolicy{
		MaxAttempts: maxAttempts,
		MaxElapsed:  time.Second,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
	}
}

type coreEvent struct {
	name  chat.ObservationName
	attrs []chat.Attr
}

// coreObserver records every Event.
type coreObserver struct {
	mu     sync.Mutex
	events []coreEvent
}

func (o *coreObserver) Event(_ context.Context, name chat.ObservationName, attrs ...chat.Attr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, coreEvent{name: name, attrs: attrs})
}

func (o *coreObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, coreSpan{}
}

func (o *coreObserver) count(name chat.ObservationName) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, ev := range o.events {
		if ev.name == name {
			n++
		}
	}
	return n
}

func (o *coreObserver) all() []coreEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

type coreSpan struct{}

func (coreSpan) End(chat.DispatchOutcome, ...chat.Attr) {}

// coreRoundTripper fails every request with err.
type coreRoundTripper struct{ err error }

func (rt coreRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, rt.err
}

func TestCallEncodesPayloadAndDecodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		payload     any
		body        string
		contentType string
		form        url.Values
	}{
		{
			name: "struct as JSON",
			payload: struct {
				Channel string `json:"channel"`
				Text    string `json:"text"`
			}{Channel: "C1", Text: "hi"},
			body:        `{"channel":"C1","text":"hi"}`,
			contentType: "application/json",
		},
		{
			name:        "url.Values as form",
			payload:     url.Values{"channel": {"C1"}, "text": {"hi"}},
			body:        "channel=C1&text=hi",
			contentType: "application/x-www-form-urlencoded",
			form:        url.Values{"channel": {"C1"}, "text": {"hi"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("chat.postMessage", map[string]any{"ok": true, "channel": "C1", "ts": "111.222"})
			client := coreNewClient(srv, slackapi.Options{})

			var dest struct {
				Channel string `json:"channel"`
				TS      string `json:"ts"`
			}
			if err := client.Call(t.Context(), "chat.postMessage", tc.payload, &dest); err != nil {
				t.Fatalf("Call: %v", err)
			}
			if dest.Channel != "C1" || dest.TS != "111.222" {
				t.Fatalf("dest = %+v, want channel C1 and ts 111.222", dest)
			}

			calls := srv.Calls("chat.postMessage")
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			call := calls[0]
			if got := string(call.Body); got != tc.body {
				t.Fatalf("body = %s, want %s", got, tc.body)
			}
			if got := call.Header.Get("Authorization"); got != "Bearer xoxb-core" {
				t.Fatalf("Authorization = %q", got)
			}
			if got := call.Header.Get("Content-Type"); got != tc.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if !reflect.DeepEqual(call.Form, tc.form) {
				t.Fatalf("Form = %v, want %v", call.Form, tc.form)
			}
		})
	}
}

func TestCallNilPayloadAndDest(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("auth.test", json.RawMessage(`{"ok":true}`))
	client := coreNewClient(srv, slackapi.Options{})

	if err := client.Call(t.Context(), "auth.test", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	calls := srv.Calls("auth.test")
	if len(calls) != 1 || string(calls[0].Body) != "{}" {
		t.Fatalf("calls = %+v, want one call with body {}", calls)
	}
}

func TestCallAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantCode   string
		wantDetail string
		wantText   string
	}{
		{
			name:       "needed scope",
			body:       `{"ok":false,"error":"missing_scope","needed":"chat:write","provided":"users:read"}`,
			wantCode:   "missing_scope",
			wantDetail: "chat:write",
			wantText:   "slack: chat.postMessage failed: missing_scope",
		},
		{
			name:       "response metadata",
			body:       `{"ok":false,"error":"invalid_blocks","response_metadata":{"messages":["[ERROR] a","[ERROR] b"]}}`,
			wantCode:   "invalid_blocks",
			wantDetail: "[ERROR] a; [ERROR] b",
			wantText:   "slack: chat.postMessage failed: invalid_blocks",
		},
		{
			name:     "no detail",
			body:     `{"ok":false,"error":"channel_not_found"}`,
			wantCode: "channel_not_found",
			wantText: "slack: chat.postMessage failed: channel_not_found",
		},
		{
			name:     "no code",
			body:     `{"ok":false}`,
			wantText: "slack: chat.postMessage failed: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("chat.postMessage", json.RawMessage(tt.body))
			client := coreNewClient(srv, slackapi.Options{})

			var dest struct {
				TS string `json:"ts"`
			}
			err := client.Call(t.Context(), "chat.postMessage", map[string]string{"channel": "C1"}, &dest)
			apiErr, ok := errors.AsType[*slackapi.APIError](err)
			if !ok {
				t.Fatalf("err = %v, want *slackapi.APIError", err)
			}
			if apiErr.Method != "chat.postMessage" || apiErr.Code != tt.wantCode || apiErr.Detail != tt.wantDetail {
				t.Fatalf("APIError = %+v, want code %q detail %q", apiErr, tt.wantCode, tt.wantDetail)
			}
			if apiErr.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, want 200", apiErr.StatusCode)
			}
			if string(apiErr.Raw) != tt.body {
				t.Fatalf("Raw = %s, want %s", apiErr.Raw, tt.body)
			}
			if err.Error() != tt.wantText {
				t.Fatalf("Error() = %q, want %q", err.Error(), tt.wantText)
			}
			if got := len(srv.Calls("chat.postMessage")); got != 1 {
				t.Fatalf("calls = %d, want 1 (an API error is not retried)", got)
			}
		})
	}
}

func TestCallHTTPStatusError(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", slackapitest.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       []byte("upstream failure"),
	})
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: coreFastPolicy(3)})

	err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil)
	apiErr, ok := errors.AsType[*slackapi.APIError](err)
	if !ok {
		t.Fatalf("err = %v, want *slackapi.APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError || apiErr.Code != "" || string(apiErr.Raw) != "upstream failure" {
		t.Fatalf("APIError = %+v", apiErr)
	}
	if got, want := err.Error(), "slack: chat.postMessage status 500"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := len(srv.Calls("chat.postMessage")); got != 1 {
		t.Fatalf("calls = %d, want 1 (a non-2xx status is not retried)", got)
	}
}

func TestAPIErrorText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  slackapi.APIError
		want string
	}{
		{name: "code only", err: slackapi.APIError{Method: "m", Code: "c"}, want: "slack: m failed: c"},
		{name: "ok false", err: slackapi.APIError{Method: "m", StatusCode: 200, Code: "c"}, want: "slack: m failed: c"},
		{name: "server error", err: slackapi.APIError{Method: "m", StatusCode: 503}, want: "slack: m status 503"},
		{name: "redirect", err: slackapi.APIError{Method: "m", StatusCode: 302}, want: "slack: m status 302"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCallDecodeError(t *testing.T) {
	t.Parallel()

	t.Run("envelope", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("auth.test", []byte("<html>not json</html>"))
		client := coreNewClient(srv, slackapi.Options{})

		err := client.Call(t.Context(), "auth.test", nil, nil)
		if err == nil || !strings.HasPrefix(err.Error(), "slack: decode auth.test response: ") {
			t.Fatalf("err = %v, want a decode error", err)
		}
	})

	t.Run("dest", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("auth.test", json.RawMessage(`{"ok":true,"team_id":42}`))
		client := coreNewClient(srv, slackapi.Options{})

		var dest struct {
			TeamID string `json:"team_id"`
		}
		err := client.Call(t.Context(), "auth.test", nil, &dest)
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok || !strings.HasPrefix(err.Error(), "slack: decode auth.test response: ") {
			t.Fatalf("err = %v, want a wrapped *json.UnmarshalTypeError", err)
		}
	})
}

func TestCallEncodeError(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	client := coreNewClient(srv, slackapi.Options{})

	err := client.Call(t.Context(), "chat.postMessage", map[string]any{"bad": func() {}}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "slack: encode chat.postMessage request: ") {
		t.Fatalf("err = %v, want an encode error", err)
	}
	if got := len(srv.Calls("chat.postMessage")); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestCallTransportError(t *testing.T) {
	t.Parallel()

	errDial := errors.New("dial refused")
	client := slackapi.New(slackapi.Options{
		Token:      "xoxb-core",
		HTTPClient: &http.Client{Transport: coreRoundTripper{err: errDial}},
	})

	err := client.Call(t.Context(), "auth.test", nil, nil)
	if !errors.Is(err, errDial) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if !strings.HasPrefix(err.Error(), "slack: auth.test request: ") {
		t.Fatalf("err = %q, want the request prefix", err.Error())
	}
}

func TestWithTokenChangesOnlyToken(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("auth.test", json.RawMessage(`{"ok":true}`))
	obs := &coreObserver{}
	base := coreNewClient(srv, slackapi.Options{Token: "xoxb-one", Observer: obs})
	other := base.WithToken("xoxb-two")

	if err := other.Call(t.Context(), "auth.test", nil, nil); err != nil {
		t.Fatalf("other Call: %v", err)
	}
	if err := base.Call(t.Context(), "auth.test", nil, nil); err != nil {
		t.Fatalf("base Call: %v", err)
	}

	calls := srv.Calls("auth.test")
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if got := calls[0].Header.Get("Authorization"); got != "Bearer xoxb-two" {
		t.Fatalf("copy Authorization = %q", got)
	}
	if got := calls[1].Header.Get("Authorization"); got != "Bearer xoxb-one" {
		t.Fatalf("original Authorization = %q", got)
	}
	if got := obs.count(chat.ObsAdapterCall); got != 2 {
		t.Fatalf("ObsAdapterCall = %d, want 2 (the copy shares the observer)", got)
	}
}

func TestNewTrimsBaseURL(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("auth.test", json.RawMessage(`{"ok":true}`))
	client := slackapi.New(slackapi.Options{
		Token:      "xoxb-core",
		BaseURL:    srv.URL() + "/",
		HTTPClient: srv.Client(),
	})
	if err := client.Call(t.Context(), "auth.test", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := len(srv.Calls("auth.test")); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestNewFileOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		origin  string
		fileURL string
		wantErr bool
	}{
		{name: "case", origin: "HTTPS://Files.Example.COM", fileURL: "https://files.example.com/x"},
		{name: "path and query", origin: "https://files.example.com/p?q", fileURL: "https://files.example.com/x"},
		{name: "https default port", origin: "https://files.example.com:443", fileURL: "https://files.example.com/x"},
		{name: "http default port", origin: "http://files.example.com:80", fileURL: "http://files.example.com/x"},
		{name: "http port on https", origin: "https://files.example.com:80", fileURL: "https://files.example.com/x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transport := filesTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
			})
			client := slackapi.New(slackapi.Options{HTTPClient: &http.Client{Transport: transport}, FileOrigins: []string{tt.origin}})

			_, err := client.DownloadFile(t.Context(), tt.fileURL, &bytes.Buffer{}, 1024)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "not in FileOrigins") {
					t.Fatalf("err = %v, want an origin error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DownloadFile: %v", err)
			}
		})
	}
}

func TestNewLogsDroppedFileOrigins(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slackapi.New(slackapi.Options{
		FileOrigins: []string{"https://files.example.com", "not a url", "/relative"},
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	out := buf.String()
	if got := strings.Count(out, "level=WARN"); got != 2 {
		t.Fatalf("warnings = %d, want 2:\n%s", got, out)
	}
	for _, want := range []string{`origin="not a url"`, "origin=/relative"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log does not contain %s:\n%s", want, out)
		}
	}
}

func TestPostResponseURL(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("hook", []byte("ok"))
		client := coreNewClient(srv, slackapi.Options{})

		if err := client.PostResponseURL(t.Context(), srv.URL()+"/hook", map[string]string{"text": "hi"}); err != nil {
			t.Fatalf("PostResponseURL: %v", err)
		}
		calls := srv.Calls("hook")
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		if got := calls[0].Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want none", got)
		}
		if got := calls[0].Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := string(calls[0].Body); got != `{"text":"hi"}` {
			t.Fatalf("body = %s", got)
		}
	})

	t.Run("status", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("hook", slackapitest.Response{StatusCode: http.StatusNotFound, Body: []byte("expired_url")})
		client := coreNewClient(srv, slackapi.Options{})

		err := client.PostResponseURL(t.Context(), srv.URL()+"/hook", map[string]string{"text": "hi"})
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok || apiErr.Method != "response_url" || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("err = %v, want *slackapi.APIError for response_url with status 404", err)
		}
		if got, want := err.Error(), "slack: response_url status 404"; got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("hook", slackapitest.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"0"}}})
		client := coreNewClient(srv, slackapi.Options{RetryPolicy: coreFastPolicy(2)})

		err := client.PostResponseURL(t.Context(), srv.URL()+"/hook", map[string]string{"text": "hi"})
		limited, ok := errors.AsType[*slackapi.RateLimited](err)
		if !ok || limited.Attempts != 2 {
			t.Fatalf("err = %v, want *slackapi.RateLimited after 2 attempts", err)
		}
		if got := len(srv.Calls("hook")); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
	})
}
