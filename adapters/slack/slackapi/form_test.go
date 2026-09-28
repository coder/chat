package slackapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const (
	methodsToken    = "xoxb-methods"
	methodsJSONType = "application/json"
	methodsFormType = "application/x-www-form-urlencoded"
)

// methodsNewClient returns a client for srv with token methodsToken.
func methodsNewClient(srv *slackapitest.Server) *slackapi.Client {
	return slackapi.New(slackapi.Options{
		Token:      methodsToken,
		BaseURL:    srv.URL(),
		HTTPClient: srv.Client(),
	})
}

// methodsSlackError returns an ok:false response body with the error code.
func methodsSlackError(code string) json.RawMessage {
	return json.RawMessage(`{"ok":false,"error":"` + code + `"}`)
}

// methodsOnlyCall returns the only request to method on srv and checks its
// bearer token and content type.
func methodsOnlyCall(t *testing.T, srv *slackapitest.Server, method, contentType string) slackapitest.Call {
	t.Helper()
	calls := srv.Calls(method)
	if len(calls) != 1 {
		t.Fatalf("%s calls = %d, want 1", method, len(calls))
	}
	call := calls[0]
	if got, want := call.Header.Get("Authorization"), "Bearer "+methodsToken; got != want {
		t.Errorf("%s Authorization = %q, want %q", method, got, want)
	}
	if got := call.Header.Get("Content-Type"); got != contentType {
		t.Errorf("%s Content-Type = %q, want %q", method, got, contentType)
	}
	return call
}

// methodsCheckJSONBody checks that the only request to method on srv is JSON
// with a body equal to wantBody.
func methodsCheckJSONBody(t *testing.T, srv *slackapitest.Server, method, wantBody string) {
	t.Helper()
	call := methodsOnlyCall(t, srv, method, methodsJSONType)
	var got, want any
	if err := json.Unmarshal(call.Body, &got); err != nil {
		t.Fatalf("decode %s body %s: %v", method, call.Body, err)
	}
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatalf("decode want body %s: %v", wantBody, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s body = %s, want %s", method, call.Body, wantBody)
	}
}

// methodsCheckForm checks that the only request to method on srv is form
// encoded with exactly the values in want.
func methodsCheckForm(t *testing.T, srv *slackapitest.Server, method string, want url.Values) {
	t.Helper()
	call := methodsOnlyCall(t, srv, method, methodsFormType)
	if !reflect.DeepEqual(call.Form, want) {
		t.Errorf("%s form = %v, want %v", method, call.Form, want)
	}
}

// methodsCheckEqual checks that got deeply equals want and prints both as JSON
// when they differ.
func methodsCheckEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Errorf("%s = %s, want %s", what, gotJSON, wantJSON)
	}
}

// methodsCheckAPIError checks that err is an *slackapi.APIError for method
// with the error code.
func methodsCheckAPIError(t *testing.T, err error, method, code string) {
	t.Helper()
	apiErr, ok := errors.AsType[*slackapi.APIError](err)
	if !ok {
		t.Fatalf("error = %v, want *slackapi.APIError", err)
	}
	if apiErr.Method != method || apiErr.Code != code {
		t.Errorf("APIError method, code = %q, %q, want %q, %q", apiErr.Method, apiErr.Code, method, code)
	}
}

func TestFormMethodsOmitZeroFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		call   func(context.Context, *slackapi.Client) error
		want   url.Values
	}{
		{
			name:   "UserInfo",
			method: "users.info",
			call: func(ctx context.Context, c *slackapi.Client) error {
				_, err := c.UserInfo(ctx, slackapi.UserInfoRequest{User: "U1"})
				return err
			},
			want: url.Values{"user": {"U1"}},
		},
		{
			name:   "ConversationInfo",
			method: "conversations.info",
			call: func(ctx context.Context, c *slackapi.Client) error {
				_, err := c.ConversationInfo(ctx, slackapi.ConversationInfoRequest{Channel: "C1"})
				return err
			},
			want: url.Values{"channel": {"C1"}},
		},
		{
			name:   "ConversationHistory",
			method: "conversations.history",
			call: func(ctx context.Context, c *slackapi.Client) error {
				_, err := c.ConversationHistory(ctx, slackapi.ConversationHistoryRequest{Channel: "C1"})
				return err
			},
			want: url.Values{"channel": {"C1"}},
		},
		{
			name:   "ConversationReplies",
			method: "conversations.replies",
			call: func(ctx context.Context, c *slackapi.Client) error {
				_, err := c.ConversationReplies(ctx, slackapi.ConversationRepliesRequest{
					Channel: "C1",
					TS:      "1700000000.000001",
				})
				return err
			},
			want: url.Values{"channel": {"C1"}, "ts": {"1700000000.000001"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := slackapitest.NewServer(t)
			srv.Respond(tt.method, json.RawMessage(`{"ok":true}`))
			if err := tt.call(t.Context(), methodsNewClient(srv)); err != nil {
				t.Fatalf("%s: %v", tt.method, err)
			}
			methodsCheckForm(t, srv, tt.method, tt.want)
		})
	}
}
