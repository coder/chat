package main

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

// newTestEvent builds a MessageEvent backed by the recording fake adapter so
// the worked examples that post through ev.Thread are exercised end to end.
func newTestEvent(t *testing.T, adapter *testLinearAdapter, msg *chat.Message) *chat.MessageEvent {
	t.Helper()
	ctx := context.Background()
	bot, err := chat.New(ctx, chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat: %v", err)
	}
	t.Cleanup(func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown chat: %v", err)
		}
	})
	thread, err := bot.Thread(ctx, chat.ThreadID("linear:v1:thread-1"))
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	return &chat.MessageEvent{Thread: thread, Message: msg}
}

func TestStartProactiveSessionPostsFirstThought(t *testing.T) {
	adapter := &testLinearAdapter{}

	threadID, err := startProactiveSession(context.Background(), adapter, "ENG-123", "https://example.com/run/1")
	if err != nil {
		t.Fatalf("start proactive session: %v", err)
	}
	if threadID != chat.ThreadID("linear:v1:new-session") {
		t.Fatalf("thread id = %q", threadID)
	}
	if len(adapter.sessionCreates) != 1 || adapter.sessionCreates[0].IssueID != "ENG-123" {
		t.Fatalf("session creates = %#v", adapter.sessionCreates)
	}
	if urls := adapter.sessionCreates[0].ExternalURLs; len(urls) != 1 || urls[0].URL != "https://example.com/run/1" {
		t.Fatalf("external urls = %#v", urls)
	}
	if len(adapter.thoughts) != 1 || adapter.thoughts[0] != "Investigating this issue." {
		t.Fatalf("thoughts = %#v", adapter.thoughts)
	}
}

func TestChooseRepositoryProceedsOnHighConfidence(t *testing.T) {
	adapter := &testLinearAdapter{suggestions: []linear.RepositorySuggestion{
		{Hostname: "github.com", RepositoryFullName: "acme/frontend", Confidence: 0.4},
		{Hostname: "github.com", RepositoryFullName: "acme/backend", Confidence: 0.9},
	}}
	ev := newTestEvent(t, adapter, &chat.Message{Text: "fix the bug"})

	if err := chooseRepository(context.Background(), adapter, ev, newPendingSelections(), []linear.CandidateRepository{{Hostname: "github.com", RepositoryFullName: "acme/backend"}}); err != nil {
		t.Fatalf("choose repository: %v", err)
	}
	if len(adapter.thoughts) != 1 || adapter.thoughts[0] != "Working in acme/backend." {
		t.Fatalf("thoughts = %#v", adapter.thoughts)
	}
	if len(adapter.elicitations) != 0 {
		t.Fatalf("unexpected elicitations = %#v", adapter.elicitations)
	}
}

func TestChooseRepositoryElicitsSelectOnLowConfidence(t *testing.T) {
	adapter := &testLinearAdapter{suggestions: []linear.RepositorySuggestion{
		{Hostname: "github.com", RepositoryFullName: "acme/backend", Confidence: 0.42},
		{Hostname: "github.com", RepositoryFullName: "acme/frontend", Confidence: 0.35},
	}}
	ev := newTestEvent(t, adapter, &chat.Message{Text: "fix the bug"})
	pending := newPendingSelections()

	if err := chooseRepository(context.Background(), adapter, ev, pending, []linear.CandidateRepository{{Hostname: "github.com", RepositoryFullName: "acme/backend"}}); err != nil {
		t.Fatalf("choose repository: %v", err)
	}
	if len(adapter.elicitations) != 1 {
		t.Fatalf("elicitations = %#v", adapter.elicitations)
	}
	elicitation := adapter.elicitations[0]
	if elicitation.Signal != "select" {
		t.Fatalf("signal = %q", elicitation.Signal)
	}
	metadata, ok := elicitation.SignalMetadata.(linear.SelectSignalMetadata)
	if !ok || len(metadata.Options) != 2 {
		t.Fatalf("signal metadata = %#v", elicitation.SignalMetadata)
	}
	if metadata.Options[0].Value != "github.com/acme/backend" || metadata.Options[1].Value != "github.com/acme/frontend" {
		t.Fatalf("options = %#v", metadata.Options)
	}
	// The offered values become this thread's pending selection so the next
	// follow-up is interpreted as the answer.
	sel, ok := pending.take(ev.Thread.ID())
	if !ok || sel.Kind != selectionKindRepository || len(sel.Values) != 2 || sel.Values[0] != "github.com/acme/backend" {
		t.Fatalf("pending = %#v, %v", sel, ok)
	}
}

func TestOfferRepositoryChoiceKeepsSameNameReposOnDifferentHostsDistinct(t *testing.T) {
	adapter := &testLinearAdapter{}
	pending := newPendingSelections()
	threadID := chat.ThreadID("linear:v1:thread-1")

	err := offerRepositoryChoice(context.Background(), adapter, threadID, pending, []linear.RepositorySuggestion{
		{Hostname: "github.com", RepositoryFullName: "acme/backend", Confidence: 0.4},
		{Hostname: "gitlab.example.com", RepositoryFullName: "acme/backend", Confidence: 0.4},
		{RepositoryFullName: "acme/orphan", Confidence: 0.1}, // hostname unresolved
	})
	if err != nil {
		t.Fatalf("offer repository choice: %v", err)
	}
	metadata, ok := adapter.elicitations[0].SignalMetadata.(linear.SelectSignalMetadata)
	if !ok || len(metadata.Options) != 3 {
		t.Fatalf("signal metadata = %#v", adapter.elicitations[0].SignalMetadata)
	}
	if metadata.Options[0].Value != "github.com/acme/backend" ||
		metadata.Options[1].Value != "gitlab.example.com/acme/backend" ||
		metadata.Options[2].Value != "acme/orphan" {
		t.Fatalf("options = %#v", metadata.Options)
	}
	sel, ok := pending.take(threadID)
	if !ok || sel.Kind != selectionKindRepository || len(sel.Values) != 3 || sel.Values[1] != "gitlab.example.com/acme/backend" {
		t.Fatalf("pending = %#v, %v", sel, ok)
	}
}

func TestFollowUpHandlerClearsPendingSelectionOnStop(t *testing.T) {
	adapter := &testLinearAdapter{}
	pending := newPendingSelections()
	ev := newTestEvent(t, adapter, &chat.Message{Text: "stop", Raw: &linear.RawMessage{Signal: "stop"}})
	pending.set(ev.Thread.ID(), pendingSelection{Kind: selectionKindDeploy, Values: []string{"staging", "prod"}})

	if err := newFollowUpHandler(adapter, pending)(context.Background(), ev); err != nil {
		t.Fatalf("follow-up handler: %v", err)
	}
	if len(adapter.posted) != 1 {
		t.Fatalf("posted = %#v", adapter.posted)
	}
	// The stopped session will not answer its elicitation: a later "staging"
	// message must not be misread as a choice.
	if _, ok := pending.take(ev.Thread.ID()); ok {
		t.Fatal("pending selection survived the stop")
	}
}

func TestPendingSelectionsAreTakeOnce(t *testing.T) {
	pending := newPendingSelections()
	threadID := chat.ThreadID("linear:v1:thread-1")

	if _, ok := pending.take(threadID); ok {
		t.Fatal("take on empty registry reported a pending selection")
	}
	pending.set(threadID, pendingSelection{Kind: selectionKindDeploy, Values: []string{"staging", "prod"}})
	sel, ok := pending.take(threadID)
	if !ok || sel.Kind != selectionKindDeploy || len(sel.Values) != 2 || sel.Values[0] != "staging" {
		t.Fatalf("take = %#v, %v", sel, ok)
	}
	// The next follow-up no longer sees a pending selection: a bare "staging"
	// message must not be consumed as a choice.
	if _, ok := pending.take(threadID); ok {
		t.Fatal("pending selection survived take")
	}
}

func TestMentionHandlerRegistersPendingSelectionOnDeployElicitation(t *testing.T) {
	adapter := &testLinearAdapter{}
	pending := newPendingSelections()
	ev := newTestEvent(t, adapter, &chat.Message{Text: "deploy"})

	if err := newMentionHandler(adapter, pending)(context.Background(), ev); err != nil {
		t.Fatalf("mention handler: %v", err)
	}
	if len(adapter.elicitations) != 1 || adapter.elicitations[0].Signal != "select" {
		t.Fatalf("elicitations = %#v", adapter.elicitations)
	}
	sel, ok := pending.take(ev.Thread.ID())
	if !ok || sel.Kind != selectionKindDeploy || len(sel.Values) != 2 {
		t.Fatalf("pending = %#v, %v", sel, ok)
	}
}

func TestChooseRepositoryAsksFreeFormWithoutSuggestions(t *testing.T) {
	adapter := &testLinearAdapter{}
	ev := newTestEvent(t, adapter, &chat.Message{Text: "fix the bug"})
	pending := newPendingSelections()

	if err := chooseRepository(context.Background(), adapter, ev, pending, []linear.CandidateRepository{{Hostname: "github.com", RepositoryFullName: "acme/backend"}}); err != nil {
		t.Fatalf("choose repository: %v", err)
	}
	if len(adapter.elicitations) != 1 || adapter.elicitations[0].Signal != "" {
		t.Fatalf("elicitations = %#v", adapter.elicitations)
	}
	// A free-form question offers no option values, so nothing is pending.
	if _, ok := pending.take(ev.Thread.ID()); ok {
		t.Fatal("free-form elicitation registered a pending selection")
	}
}

func TestHandleSelectionMatchesOptionValueOrFallsThrough(t *testing.T) {
	adapter := &testLinearAdapter{}
	deploy := pendingSelection{Kind: selectionKindDeploy, Values: []string{"staging", "prod"}}
	ev := newTestEvent(t, adapter, &chat.Message{Text: "staging"})

	handled, err := handleSelection(context.Background(), ev, deploy)
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if len(adapter.posted) != 1 || adapter.posted[0] != "Deploying to **staging** — I'll report back here." {
		t.Fatalf("posted = %#v", adapter.posted)
	}

	// Free text is not consumed: it falls through to normal prompt handling.
	free := newTestEvent(t, adapter, &chat.Message{Text: "actually, roll back instead"})
	handled, err = handleSelection(context.Background(), free, deploy)
	if err != nil || handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if len(adapter.posted) != 1 {
		t.Fatalf("free text posted = %#v", adapter.posted)
	}
}

func TestHandleSelectionRoutesRepositoryChoicesToRepositoryHandling(t *testing.T) {
	adapter := &testLinearAdapter{}
	ev := newTestEvent(t, adapter, &chat.Message{Text: "github.com/acme/backend"})

	handled, err := handleSelection(context.Background(), ev, pendingSelection{
		Kind:   selectionKindRepository,
		Values: []string{"github.com/acme/backend", "github.com/acme/frontend"},
	})
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if len(adapter.posted) != 1 || adapter.posted[0] != "Working in **github.com/acme/backend** — I'll open a pull request here when I'm done." {
		t.Fatalf("posted = %#v", adapter.posted)
	}
}

func TestFollowUpHandlerRetainsPendingChoiceWhenPostFails(t *testing.T) {
	postErr := errors.New("post failed")
	adapter := &testLinearAdapter{postErr: postErr}
	pending := newPendingSelections()
	ev := newTestEvent(t, adapter, &chat.Message{Text: "staging"})
	deploy := pendingSelection{Kind: selectionKindDeploy, Values: []string{"staging", "prod"}}
	pending.set(ev.Thread.ID(), deploy)

	err := newFollowUpHandler(adapter, pending)(context.Background(), ev)
	if !errors.Is(err, postErr) {
		t.Fatalf("handler error = %v, want %v", err, postErr)
	}
	// The acknowledgement never posted, so the user's retry must still be
	// interpreted as an answer: the choice stays pending.
	sel, ok := pending.take(ev.Thread.ID())
	if !ok || sel.Kind != selectionKindDeploy {
		t.Fatalf("pending after failed post = %#v, %v", sel, ok)
	}
}

func TestRequireAccountLinkSendsAuthSignal(t *testing.T) {
	adapter := &testLinearAdapter{}

	if err := requireAccountLink(context.Background(), adapter, chat.ThreadID("linear:v1:thread-1"), "https://auth.example.com/oauth", "USER1"); err != nil {
		t.Fatalf("require account link: %v", err)
	}
	if len(adapter.elicitations) != 1 || adapter.elicitations[0].Signal != "auth" {
		t.Fatalf("elicitations = %#v", adapter.elicitations)
	}
	metadata, ok := adapter.elicitations[0].SignalMetadata.(linear.AuthSignalMetadata)
	if !ok || metadata.URL != "https://auth.example.com/oauth" || metadata.UserID != "USER1" {
		t.Fatalf("signal metadata = %#v", adapter.elicitations[0].SignalMetadata)
	}
}

func TestResumeAfterAccountLinkPostsThoughtThenResponse(t *testing.T) {
	ctx := context.Background()
	adapter := &testLinearAdapter{}
	bot, err := chat.New(ctx, chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat: %v", err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown chat: %v", err)
		}
	}()

	if err := resumeAfterAccountLink(ctx, bot, adapter, chat.ThreadID("linear:v1:thread-1")); err != nil {
		t.Fatalf("resume after account link: %v", err)
	}
	if len(adapter.thoughts) != 1 || adapter.thoughts[0] != "Account linked — resuming." {
		t.Fatalf("thoughts = %#v", adapter.thoughts)
	}
	if len(adapter.posted) != 1 {
		t.Fatalf("posted = %#v", adapter.posted)
	}
}

func TestPublishPullRequestAddsExternalURL(t *testing.T) {
	adapter := &testLinearAdapter{}

	if err := publishPullRequest(context.Background(), adapter, chat.ThreadID("linear:v1:thread-1"), "https://github.com/acme/backend/pull/7"); err != nil {
		t.Fatalf("publish pull request: %v", err)
	}
	if len(adapter.sessionUpdates) != 1 {
		t.Fatalf("session updates = %#v", adapter.sessionUpdates)
	}
	added := adapter.sessionUpdates[0].AddExternalURLs
	if len(added) != 1 || added[0].URL != "https://github.com/acme/backend/pull/7" || added[0].Label != "Pull request" {
		t.Fatalf("added external urls = %#v", added)
	}
}

func TestConfirmStopConfirmsOnlyOnStopSignal(t *testing.T) {
	adapter := &testLinearAdapter{}
	stop := newTestEvent(t, adapter, &chat.Message{Text: "stop", Raw: &linear.RawMessage{Signal: "stop"}})

	stopped, err := confirmStop(context.Background(), stop)
	if err != nil || !stopped {
		t.Fatalf("stopped = %v, err = %v", stopped, err)
	}
	if len(adapter.posted) != 1 {
		t.Fatalf("posted = %#v", adapter.posted)
	}

	normal := newTestEvent(t, adapter, &chat.Message{Text: "keep going", Raw: &linear.RawMessage{}})
	stopped, err = confirmStop(context.Background(), normal)
	if err != nil || stopped {
		t.Fatalf("stopped = %v, err = %v", stopped, err)
	}
	if len(adapter.posted) != 1 {
		t.Fatalf("posted after non-stop = %#v", adapter.posted)
	}
}
