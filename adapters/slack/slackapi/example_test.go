package slackapi_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/coder/websocket"

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

func ExampleClient_Listen() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A stand-in for Slack: it sends hello and one envelope, prints the ack,
	// and then stops Listen.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("POST /apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"url":"ws%s/link/"}`, strings.TrimPrefix(srv.URL, "http"))
	})
	mux.HandleFunc("/link/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","num_connections":1}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"e1","payload":{"type":"event_callback"}}`))
		_, ack, _ := conn.Read(ctx)
		fmt.Println("ack:", string(ack))
		cancel()
		// Read until the close frame of Listen.
		_, _, _ = conn.Read(context.Background())
	})

	api := slackapi.New(slackapi.Options{BaseURL: srv.URL})
	err := api.WithToken("xapp-example").Listen(ctx, func(_ context.Context, d *slackapi.Delivery) (any, error) {
		fmt.Println("delivery:", d.Type, string(d.Payload))
		return nil, nil
	}, slackapi.ListenOptions{
		OnError: func(err error) { log.Println("slack:", err) },
	})
	fmt.Println("listen:", err)
	// Output:
	// delivery: events_api {"type":"event_callback"}
	// ack: {"envelope_id":"e1"}
	// listen: <nil>
}
