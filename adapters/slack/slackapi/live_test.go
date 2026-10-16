// TestLive is a smoke test against a real Slack workspace. It skips unless
// these variables are set:
//
//	SLACKAPI_LIVE_TOKEN        bot token (xoxb-...)
//	SLACKAPI_LIVE_CHANNEL      ID of a public channel that the bot is a member of
//	SLACKAPI_LIVE_STATUS_WAIT  optional; "1" adds a manual check of the
//	                           2-minute assistant thread status timeout (about 3 minutes)
//
// Required bot scopes: chat:write, reactions:write, users:read, channels:read,
// channels:history, files:read, files:write.
//
// Run from the repository root:
//
//	SLACKAPI_LIVE_TOKEN=xoxb-... SLACKAPI_LIVE_CHANNEL=C... go test -run TestLive -v -count=1 -timeout 10m ./adapters/slack/slackapi/
//
// For the status timeout check, also set SLACKAPI_LIVE_STATUS_WAIT=1 and watch
// the test thread in Slack.
//
// TestLiveSocketMode is a smoke test of Socket Mode. It also needs:
//
//	SLACKAPI_LIVE_APP_TOKEN    app-level token (xapp-...) with connections:write
//
// The app needs Socket Mode on, the bot event message.channels, and the bot
// scopes chat:write and channels:history. Run it with:
//
//	SLACKAPI_LIVE_TOKEN=xoxb-... SLACKAPI_LIVE_APP_TOKEN=xapp-... SLACKAPI_LIVE_CHANNEL=C... go test -run TestLiveSocketMode -v -count=1 -timeout 3m ./adapters/slack/slackapi/

package slackapi_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat/adapters/slack/slackapi"
)

const liveParentMarkdown = "**slackapi live test**\n\n- a list item\n- another list item\n\n```go\nfmt.Println(\"hello from slackapi\")\n```"

func TestLive(t *testing.T) {
	t.Parallel()

	token := os.Getenv("SLACKAPI_LIVE_TOKEN")
	channel := os.Getenv("SLACKAPI_LIVE_CHANNEL")
	if token == "" || channel == "" {
		t.Skip("set SLACKAPI_LIVE_TOKEN and SLACKAPI_LIVE_CHANNEL to run the live Slack smoke test")
	}
	statusWait := os.Getenv("SLACKAPI_LIVE_STATUS_WAIT") == "1"
	timeout := time.Minute
	if statusWait {
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	client := slackapi.New(slackapi.Options{Token: token})

	auth, err := client.AuthTest(ctx)
	if err != nil {
		t.Fatalf("step 1 AuthTest: %s", liveErr(err))
	}
	t.Logf("step 1 AuthTest: team %q, team ID %s, user ID %s, bot ID %s", auth.Team, auth.TeamID, auth.UserID, auth.BotID)

	parent, err := client.PostMessage(ctx, slackapi.PostMessageRequest{
		Channel:     channel,
		Text:        "slackapi live test: parent message",
		Blocks:      []slackapi.Block{slackapi.MarkdownBlock{Text: liveParentMarkdown}},
		UnfurlLinks: new(false),
		UnfurlMedia: new(false),
	})
	if err != nil {
		t.Fatalf("step 2 PostMessage parent: %s", liveErr(err))
	}
	liveDeleteMessageOnCleanup(t, client, channel, parent.TS)
	t.Logf("step 2 PostMessage parent: channel %s, ts %s", parent.Channel, parent.TS)

	reply, err := client.PostMessage(ctx, slackapi.PostMessageRequest{
		Channel:      channel,
		ThreadTS:     parent.TS,
		MarkdownText: "Thread reply sent with **markdown_text**.",
	})
	if err != nil {
		t.Fatalf("step 3 PostMessage reply: %s", liveErr(err))
	}
	liveDeleteMessageOnCleanup(t, client, channel, reply.TS)
	t.Logf("step 3 PostMessage reply: ts %s", reply.TS)

	reaction := slackapi.ReactionRequest{Channel: channel, Timestamp: parent.TS, Name: "eyes"}
	if err := client.AddReaction(ctx, reaction); err != nil {
		t.Fatalf("step 4 AddReaction: %s", liveErr(err))
	}
	if err := client.RemoveReaction(ctx, reaction); err != nil {
		t.Fatalf("step 4 RemoveReaction: %s", liveErr(err))
	}
	t.Logf("step 4 AddReaction and RemoveReaction %q: ok", reaction.Name)

	clearStatus := slackapi.SetAssistantThreadStatusRequest{ChannelID: channel, ThreadTS: parent.TS}
	setStatus := clearStatus
	setStatus.Status = "is running the live test..."
	setStatus.LoadingMessages = []string{"Calling Slack...", "Still calling Slack..."}
	statusCall := "set"
	statusErr := client.SetAssistantThreadStatus(ctx, setStatus)
	if statusErr == nil {
		statusCall = "clear"
		statusErr = client.SetAssistantThreadStatus(ctx, clearStatus)
	}
	if statusErr != nil {
		t.Logf("step 5 SetAssistantThreadStatus %s: %s", statusCall, liveErr(statusErr))
		// Deferred so that the remaining steps run and the failure shows at the end.
		defer t.Errorf("step 5 SetAssistantThreadStatus %s: %s", statusCall, liveErr(statusErr))
	} else {
		t.Logf("step 5 SetAssistantThreadStatus: set with loading messages, then cleared: ok")
	}

	if statusWait {
		if statusErr != nil {
			t.Logf("status wait: skipped because step 5 failed")
		} else {
			liveStatusWait(ctx, t, client, setStatus, clearStatus)
		}
	}

	updated, err := client.UpdateMessage(ctx, slackapi.UpdateMessageRequest{
		Channel: channel,
		TS:      parent.TS,
		Text:    "slackapi live test: parent message (updated)",
		Blocks:  []slackapi.Block{slackapi.MarkdownBlock{Text: "**slackapi live test** (updated)\n\nThe thread holds the test replies."}},
	})
	if err != nil {
		t.Fatalf("step 6 UpdateMessage: %s", liveErr(err))
	}
	t.Logf("step 6 UpdateMessage: ts %s, text %q", updated.TS, updated.Text)

	user, err := client.UserInfo(ctx, slackapi.UserInfoRequest{User: auth.UserID})
	if err != nil {
		t.Fatalf("step 7 UserInfo (form POST): %s", liveErr(err))
	}
	t.Logf("step 7 UserInfo (form POST accepted): ID %s, display name %q, is bot %t", user.ID, user.DisplayName(), user.IsBot)

	conversation, err := client.ConversationInfo(ctx, slackapi.ConversationInfoRequest{Channel: channel})
	if err != nil {
		t.Fatalf("step 8 ConversationInfo (form POST): %s", liveErr(err))
	}
	t.Logf("step 8 ConversationInfo (form POST accepted): name %q, is member %t", conversation.Name, conversation.IsMember)

	foundParent := false
	cursor := ""
	for page := 1; page <= 2; page++ {
		history, err := client.ConversationHistory(ctx, slackapi.ConversationHistoryRequest{
			Channel:   channel,
			Oldest:    parent.TS,
			Inclusive: true,
			Limit:     1,
			Cursor:    cursor,
		})
		if err != nil {
			t.Fatalf("step 9 ConversationHistory (form POST) page %d: %s", page, liveErr(err))
		}
		foundParent = foundParent || slices.ContainsFunc(history.Messages, func(m slackapi.Message) bool { return m.TS == parent.TS })
		t.Logf("step 9 ConversationHistory (form POST accepted) page %d: %d messages, has more %t, parent found %t", page, len(history.Messages), history.HasMore, foundParent)
		if !history.HasMore || history.NextCursor == "" {
			break
		}
		cursor = history.NextCursor
	}
	if !foundParent {
		t.Fatalf("step 9 ConversationHistory: parent %s not found", parent.TS)
	}

	foundParent, foundReply := false, false
	cursor = ""
	for page := 1; page <= 10 && !foundReply; page++ {
		replies, err := client.ConversationReplies(ctx, slackapi.ConversationRepliesRequest{
			Channel: channel,
			TS:      parent.TS,
			Limit:   1,
			Cursor:  cursor,
		})
		if err != nil {
			t.Fatalf("step 9 ConversationReplies (form POST) page %d: %s", page, liveErr(err))
		}
		for _, m := range replies.Messages {
			foundParent = foundParent || m.TS == parent.TS
			foundReply = foundReply || m.TS == reply.TS
		}
		t.Logf("step 9 ConversationReplies (form POST accepted) page %d: %d messages, has more %t, parent found %t, reply found %t", page, len(replies.Messages), replies.HasMore, foundParent, foundReply)
		if !replies.HasMore || replies.NextCursor == "" {
			break
		}
		cursor = replies.NextCursor
	}
	if !foundParent || !foundReply {
		t.Fatalf("step 9 ConversationReplies: parent found %t, reply found %t", foundParent, foundReply)
	}

	content := []byte("slackapi live test snippet\nsecond line\n")
	uploaded, err := client.UploadFile(ctx, slackapi.UploadFileRequest{
		Filename:    "slackapi-live.txt",
		Title:       "slackapi live test snippet",
		Content:     content,
		SnippetType: "text",
		ChannelID:   channel,
		ThreadTS:    parent.TS,
	})
	if err != nil {
		t.Fatalf("step 10 UploadFile: %s", liveErr(err))
	}
	liveDeleteFileOnCleanup(t, client, uploaded.ID)
	t.Logf("step 10 UploadFile (upload URL accepted raw bytes without a token): file %s, completed file has url_private_download %t", uploaded.ID, uploaded.URLPrivateDownload != "")

	shareTS, shared, found := liveFindThreadFile(ctx, t, client, channel, parent.TS, uploaded.ID)
	if found {
		liveDeleteMessageOnCleanup(t, client, channel, shareTS)
	}
	downloadURL := cmp.Or(uploaded.URLPrivateDownload, shared.URLPrivateDownload)
	if downloadURL == "" {
		t.Fatalf("step 10: file %s has no url_private_download", uploaded.ID)
	}
	// The HTTP client records redirect origins so the log shows whether the download redirected.
	var redirects []string
	downloader := slackapi.New(slackapi.Options{
		Token: token,
		HTTPClient: &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			redirects = append(redirects, req.URL.Scheme+"://"+req.URL.Host)
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}},
	})
	var downloaded bytes.Buffer
	n, err := downloader.DownloadFile(ctx, downloadURL, &downloaded, 1<<20)
	if err != nil {
		t.Fatalf("step 10 DownloadFile: %s (redirects %v)", liveErr(err), redirects)
	}
	if !bytes.Equal(downloaded.Bytes(), content) {
		t.Fatalf("step 10 DownloadFile: got %q, want %q", downloaded.Bytes(), content)
	}
	t.Logf("step 10 DownloadFile: %d bytes match the upload, %d redirects %v", n, len(redirects), redirects)
}

func TestLiveSocketMode(t *testing.T) {
	t.Parallel()

	token := os.Getenv("SLACKAPI_LIVE_TOKEN")
	appToken := os.Getenv("SLACKAPI_LIVE_APP_TOKEN")
	channel := os.Getenv("SLACKAPI_LIVE_CHANNEL")
	if token == "" || appToken == "" || channel == "" {
		t.Skip("set SLACKAPI_LIVE_TOKEN, SLACKAPI_LIVE_APP_TOKEN, and SLACKAPI_LIVE_CHANNEL to run the live Socket Mode smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	run := time.Now().UnixNano()
	resendText := fmt.Sprintf("slackapi live Socket Mode test %d: not acked at first", run)
	ackText := fmt.Sprintf("slackapi live Socket Mode test %d: acked", run)
	resends := make(chan liveEnvelope, 10)
	acks := make(chan liveEnvelope, 10)
	hellos := make(chan slackapi.Hello, 10)
	recovered := make(chan error, 10)
	var nacked atomic.Bool
	handler := func(_ context.Context, d *slackapi.Delivery) (any, error) {
		switch {
		case d.Type != slackapi.DeliveryEventsAPI:
		case bytes.Contains(d.Payload, []byte(resendText)):
			liveOffer(resends, liveEnvelope{d, time.Now()})
			if !nacked.Swap(true) {
				return nil, errors.New("not acked, so that Slack resends it")
			}
		case bytes.Contains(d.Payload, []byte(ackText)):
			liveOffer(acks, liveEnvelope{d, time.Now()})
		}
		return nil, nil
	}

	// logs is read only after Listen returns.
	var logs bytes.Buffer
	app := slackapi.New(slackapi.Options{
		Token:  appToken,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	listenCtx, stop := context.WithCancel(ctx)
	listenDone := make(chan struct{})
	var listenErr error
	go func() {
		defer close(listenDone)
		listenErr = app.Listen(listenCtx, handler, slackapi.ListenOptions{
			OnConnect: func(h slackapi.Hello) { liveOffer(hellos, h) },
			OnError:   func(err error) { liveOffer(recovered, err) },
		})
	}()
	t.Cleanup(func() {
		stop()
		<-listenDone
		t.Logf("Listen logs:\n%s", logs.String())
	})

	select {
	case hello := <-hellos:
		if hello.AppID == "" {
			t.Fatalf("step 1 Listen: hello has no app ID")
		}
		t.Logf("step 1 Listen: connected, app %s, host %s, %d connections", hello.AppID, hello.Host, hello.NumConnections)
	case <-listenDone:
		t.Fatalf("step 1 Listen: %s", liveErr(listenErr))
	case <-ctx.Done():
		t.Fatalf("step 1 Listen: no hello: %v", ctx.Err())
	}

	bot := slackapi.New(slackapi.Options{Token: token})
	resendTS := livePost(ctx, t, "step 2", bot, channel, resendText)
	first := liveNextEnvelope(ctx, t, "step 2", resends)
	eventID := liveCheckMessageEvent(t, "step 2", first.delivery, channel, resendTS)
	if first.delivery.RetryAttempt != 0 {
		t.Fatalf("step 2: retry attempt %d, want 0", first.delivery.RetryAttempt)
	}
	t.Logf("step 2 Listen: envelope for message %s, event %s, not acked", resendTS, eventID)

	var resendAfter time.Duration
	for resendAfter == 0 {
		e := liveNextEnvelope(ctx, t, "step 3", resends)
		if id := liveCheckMessageEvent(t, "step 3", e.delivery, channel, resendTS); id != eventID {
			t.Fatalf("step 3: event %s, want %s", id, eventID)
		}
		if e.delivery.RetryAttempt == 0 {
			t.Logf("step 3 Listen: duplicate envelope with retry attempt 0, acked")
			continue
		}
		if e.delivery.RetryAttempt != 1 || e.delivery.RetryReason == "" {
			t.Fatalf("step 3: retry attempt %d, reason %q, want attempt 1 and a reason", e.delivery.RetryAttempt, e.delivery.RetryReason)
		}
		resendAfter = e.at.Sub(first.at)
		t.Logf("step 3 Listen: Slack resent the envelope after %s, retry attempt %d, reason %q, acked", resendAfter.Round(time.Millisecond), e.delivery.RetryAttempt, e.delivery.RetryReason)
	}

	ackTS := livePost(ctx, t, "step 4", bot, channel, ackText)
	acked := liveNextEnvelope(ctx, t, "step 4", acks)
	eventID = liveCheckMessageEvent(t, "step 4", acked.delivery, channel, ackTS)
	if acked.delivery.RetryAttempt != 0 {
		t.Fatalf("step 4: retry attempt %d, want 0", acked.delivery.RetryAttempt)
	}
	t.Logf("step 4 Listen: envelope for message %s, event %s, acked", ackTS, eventID)

	// Step 3 shows that Slack resends an envelope that is not acked, so no
	// resend in this time shows that Slack got the ack.
	quiet := time.NewTimer(3 * resendAfter)
	defer quiet.Stop()
	for waiting := true; waiting; {
		select {
		case e := <-acks:
			if e.delivery.RetryAttempt != 0 {
				t.Fatalf("step 5: Slack resent the acked envelope: retry attempt %d, reason %q", e.delivery.RetryAttempt, e.delivery.RetryReason)
			}
			t.Logf("step 5 Listen: duplicate envelope with retry attempt 0, acked")
		case <-quiet.C:
			t.Logf("step 5 Listen: no resend in %s after the ack", (3 * resendAfter).Round(time.Millisecond))
			waiting = false
		case <-ctx.Done():
			t.Fatalf("step 5: %v", ctx.Err())
		}
	}

	stop()
	<-listenDone
	if listenErr != nil {
		t.Fatalf("step 6 Listen: %s", liveErr(listenErr))
	}
	if bytes.Contains(logs.Bytes(), []byte("slack socket mode close failed")) {
		t.Fatalf("step 6 Listen: close handshake failed, see the logs")
	}
	for len(recovered) > 0 {
		t.Errorf("step 6 Listen: recovered error: %v", <-recovered)
	}
	t.Logf("step 6 Listen: stopped, and Slack answered the close frame")
}

// liveEnvelope is an envelope that the handler of TestLiveSocketMode got, and
// the time that it got it.
type liveEnvelope struct {
	delivery *slackapi.Delivery
	at       time.Time
}

// liveOffer sends v on ch, or drops it when ch is full, so that a callback of
// Listen never blocks.
func liveOffer[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func livePost(ctx context.Context, t *testing.T, step string, bot *slackapi.Client, channel, text string) string {
	t.Helper()
	posted, err := bot.PostMessage(ctx, slackapi.PostMessageRequest{Channel: channel, Text: text})
	if err != nil {
		t.Fatalf("%s PostMessage: %s", step, liveErr(err))
	}
	liveDeleteMessageOnCleanup(t, bot, channel, posted.TS)
	return posted.TS
}

func liveNextEnvelope(ctx context.Context, t *testing.T, step string, envelopes <-chan liveEnvelope) liveEnvelope {
	t.Helper()
	select {
	case e := <-envelopes:
		return e
	case <-ctx.Done():
		t.Fatalf("%s: no envelope: %v", step, ctx.Err())
	}
	return liveEnvelope{}
}

// liveCheckMessageEvent checks that d is the message event for ts in channel,
// and returns its event ID.
func liveCheckMessageEvent(t *testing.T, step string, d *slackapi.Delivery, channel, ts string) string {
	t.Helper()
	var callback struct {
		Type    string `json:"type"`
		EventID string `json:"event_id"`
		Event   struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		} `json:"event"`
	}
	if err := json.Unmarshal(d.Payload, &callback); err != nil {
		t.Fatalf("%s: decode payload: %v", step, err)
	}
	if callback.Type != "event_callback" || callback.Event.Type != "message" || callback.Event.Channel != channel || callback.Event.TS != ts || callback.EventID == "" {
		t.Fatalf("%s: got %s event %s %q in %s at %s, want event_callback with the message %s in %s", step, callback.Type, callback.EventID, callback.Event.Type, callback.Event.Channel, callback.Event.TS, ts, channel)
	}
	return callback.EventID
}

// liveStatusWait sets the thread status, sets it again after 100 s, and waits
// 60 s more, so the user can see whether the second call restarts the
// 2-minute status timeout.
func liveStatusWait(ctx context.Context, t *testing.T, client *slackapi.Client, setStatus, clearStatus slackapi.SetAssistantThreadStatusRequest) {
	t.Helper()
	if err := client.SetAssistantThreadStatus(ctx, setStatus); err != nil {
		t.Fatalf("status wait: set: %s", liveErr(err))
	}
	t.Logf("status wait: status set at 0 s, watch the thread")
	if err := liveWait(ctx, 100*time.Second); err != nil {
		t.Fatalf("status wait: %v", err)
	}
	if err := client.SetAssistantThreadStatus(ctx, setStatus); err != nil {
		t.Fatalf("status wait: set again: %s", liveErr(err))
	}
	t.Logf("status wait: status set again at 100 s")
	if err := liveWait(ctx, 60*time.Second); err != nil {
		t.Fatalf("status wait: %v", err)
	}
	t.Logf("status wait: 160 s: if the status is still visible now, a new call restarts the 2-minute timeout; if it disappeared at about 120 s, it does not")
	if err := client.SetAssistantThreadStatus(ctx, clearStatus); err != nil {
		t.Fatalf("status wait: clear: %s", liveErr(err))
	}
	t.Logf("status wait: status cleared")
}

// liveFindThreadFile polls the thread for up to 15 s, because Slack shares an
// upload asynchronously. It returns the ts of the message that shares the file
// and the file with its url_private_download.
func liveFindThreadFile(ctx context.Context, t *testing.T, client *slackapi.Client, channel, threadTS, fileID string) (string, slackapi.File, bool) {
	t.Helper()
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		ts, file, err := liveThreadFile(pollCtx, client, channel, threadTS, fileID)
		if err != nil && pollCtx.Err() == nil {
			t.Fatalf("step 10 ConversationReplies: %s", liveErr(err))
		}
		if ts != "" {
			t.Logf("step 10 ConversationReplies: file %s shared in message %s after %d attempts", fileID, ts, attempt)
			return ts, file, true
		}
		if liveWait(pollCtx, time.Second) != nil {
			t.Logf("step 10 ConversationReplies: file %s not in the thread after %d attempts", fileID, attempt)
			return "", slackapi.File{}, false
		}
	}
}

func liveThreadFile(ctx context.Context, client *slackapi.Client, channel, threadTS, fileID string) (string, slackapi.File, error) {
	cursor := ""
	for range 10 {
		page, err := client.ConversationReplies(ctx, slackapi.ConversationRepliesRequest{Channel: channel, TS: threadTS, Cursor: cursor})
		if err != nil {
			return "", slackapi.File{}, err
		}
		for _, msg := range page.Messages {
			for _, file := range msg.Files {
				if file.ID == fileID && file.URLPrivateDownload != "" {
					return msg.TS, file, nil
				}
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return "", slackapi.File{}, nil
}

func liveWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func liveDeleteMessageOnCleanup(t *testing.T, client *slackapi.Client, channel, ts string) {
	t.Helper()
	liveCleanup(t, "DeleteMessage "+ts, func(ctx context.Context) error {
		return client.DeleteMessage(ctx, slackapi.DeleteMessageRequest{Channel: channel, TS: ts})
	})
}

func liveDeleteFileOnCleanup(t *testing.T, client *slackapi.Client, fileID string) {
	t.Helper()
	liveCleanup(t, "files.delete "+fileID, func(ctx context.Context) error {
		err := client.Call(ctx, "files.delete", map[string]string{"file": fileID}, nil)
		if apiErr, ok := errors.AsType[*slackapi.APIError](err); ok && (apiErr.Code == "file_deleted" || apiErr.Code == "file_not_found") {
			return nil
		}
		return err
	})
}

// liveCleanup registers fn as a cleanup. Cleanups run in reverse order, so
// replies are deleted before the parent. fn gets a fresh context because the
// test context is done when cleanups run.
func liveCleanup(t *testing.T, name string, fn func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			t.Errorf("cleanup %s: %s", name, liveErr(err))
			return
		}
		t.Logf("cleanup %s: ok", name)
	})
}

// liveErr formats err with the Slack error code and detail when err wraps an
// *slackapi.APIError. It never includes the token.
func liveErr(err error) string {
	if apiErr, ok := errors.AsType[*slackapi.APIError](err); ok {
		return fmt.Sprintf("%v (code %q, detail %q, HTTP status %d)", err, apiErr.Code, apiErr.Detail, apiErr.StatusCode)
	}
	return err.Error()
}
