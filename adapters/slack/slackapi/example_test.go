package slackapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func ExampleClient_PostMessage() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"channel":"C123","ts":"1700000000.000100"}`)
	}))
	defer srv.Close()

	client := slackapi.New(slackapi.Options{
		Token:   "xoxb-example",
		BaseURL: srv.URL,
	})
	resp, err := client.PostMessage(context.Background(), slackapi.PostMessageRequest{
		Channel:  "C123",
		ThreadTS: "1700000000.000001",
		Text:     "Build finished",
		Blocks: []slackapi.Block{
			slackapi.MarkdownBlock{Text: "**Build finished** in 42s"},
		},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(resp.Channel, resp.TS)
	// Output: C123 1700000000.000100
}
