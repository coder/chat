package slackapi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

func TestReactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		call   func(context.Context, *slackapi.Client, slackapi.ReactionRequest) error
	}{
		{
			name:   "AddReaction",
			method: "reactions.add",
			call: func(ctx context.Context, c *slackapi.Client, req slackapi.ReactionRequest) error {
				return c.AddReaction(ctx, req)
			},
		},
		{
			name:   "RemoveReaction",
			method: "reactions.remove",
			call: func(ctx context.Context, c *slackapi.Client, req slackapi.ReactionRequest) error {
				return c.RemoveReaction(ctx, req)
			},
		},
	}
	req := slackapi.ReactionRequest{Channel: "C1", Timestamp: "1700000000.000100", Name: "eyes"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			t.Run("Request", func(t *testing.T) {
				t.Parallel()
				srv := slackapitest.NewServer(t)
				srv.Respond(tt.method, json.RawMessage(`{"ok":true}`))

				if err := tt.call(t.Context(), methodsNewClient(srv), req); err != nil {
					t.Fatalf("%s: %v", tt.name, err)
				}
				methodsCheckJSONBody(t, srv, tt.method, `{
					"channel": "C1",
					"timestamp": "1700000000.000100",
					"name": "eyes"
				}`)
			})
		})
	}
}
