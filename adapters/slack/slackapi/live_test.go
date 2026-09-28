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

package slackapi_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
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
		timeout = 4 * time.Minute
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

	var jsonReplies struct {
		Messages []slackapi.Message `json:"messages"`
	}
	err = client.Call(ctx, "conversations.replies", map[string]any{"channel": channel, "ts": parent.TS, "limit": 10}, &jsonReplies)
	if err != nil {
		t.Logf("step 10 conversations.replies as JSON (diagnostic): rejected: %s", liveErr(err))
	} else {
		firstIsParent := len(jsonReplies.Messages) > 0 && jsonReplies.Messages[0].TS == parent.TS
		jsonReply := slices.ContainsFunc(jsonReplies.Messages, func(m slackapi.Message) bool { return m.TS == reply.TS })
		t.Logf("step 10 conversations.replies as JSON (diagnostic): ok, %d messages, first is parent %t, reply found %t", len(jsonReplies.Messages), firstIsParent, jsonReply)
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
		t.Fatalf("step 11 UploadFile: %s", liveErr(err))
	}
	liveDeleteFileOnCleanup(t, client, uploaded.ID)
	t.Logf("step 11 UploadFile (upload URL accepted raw bytes without a token): file %s, completed file has url_private_download %t", uploaded.ID, uploaded.URLPrivateDownload != "")

	shareTS, shared, found := liveFindThreadFile(ctx, t, client, channel, parent.TS, uploaded.ID)
	if found {
		liveDeleteMessageOnCleanup(t, client, channel, shareTS)
	}
	downloadURL := cmp.Or(uploaded.URLPrivateDownload, shared.URLPrivateDownload)
	if downloadURL == "" {
		t.Fatalf("step 11: file %s has no url_private_download", uploaded.ID)
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
		t.Fatalf("step 11 DownloadFile: %s (redirects %v)", liveErr(err), redirects)
	}
	if !bytes.Equal(downloaded.Bytes(), content) {
		t.Fatalf("step 11 DownloadFile: got %q, want %q", downloaded.Bytes(), content)
	}
	t.Logf("step 11 DownloadFile: %d bytes match the upload, %d redirects %v", n, len(redirects), redirects)

	large, err := client.PostMessage(ctx, slackapi.PostMessageRequest{
		Channel:  channel,
		ThreadTS: parent.TS,
		Text:     "Two markdown blocks of 7,000 characters each",
		Blocks: []slackapi.Block{
			slackapi.MarkdownBlock{Text: strings.Repeat("block one ", 700)},
			slackapi.MarkdownBlock{Text: strings.Repeat("block two ", 700)},
		},
	})
	if apiErr, ok := errors.AsType[*slackapi.APIError](err); ok {
		t.Logf("step 12 PostMessage 2 x 7,000 characters: per-message limit: rejected with %s (detail %q)", apiErr.Code, apiErr.Detail)
	} else if err != nil {
		t.Fatalf("step 12 PostMessage 2 x 7,000 characters: %s", liveErr(err))
	} else {
		liveDeleteMessageOnCleanup(t, client, channel, large.TS)
		t.Logf("step 12 PostMessage 2 x 7,000 characters: per-block limit: accepted, ts %s", large.TS)
	}
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
			t.Fatalf("step 11 ConversationReplies: %s", liveErr(err))
		}
		if ts != "" {
			t.Logf("step 11 ConversationReplies: file %s shared in message %s after %d attempts", fileID, ts, attempt)
			return ts, file, true
		}
		if liveWait(pollCtx, time.Second) != nil {
			t.Logf("step 11 ConversationReplies: file %s not in the thread after %d attempts", fileID, attempt)
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
