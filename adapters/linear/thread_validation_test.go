package linear

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/coder/chat"
)

func TestTenantEnforcementForThreadIDThoughtAndPostMessage(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{
		apiBaseURL:     "https://linear.example",
		client:         &http.Client{Transport: failingLinearTransport{}},
		organizationID: "ORG1",
		botUserID:      "APP1",
	}
	otherOrgThread, err := encodeThreadID(threadPayload{Organization: "ORG2", Issue: "ISSUE1", Session: "S1"})
	if err != nil {
		t.Fatalf("encode thread id: %v", err)
	}
	wantErr := `linear: thread organization "ORG2" does not match initialized organization`

	if _, err := adapter.ValidateThreadID(otherOrgThread); err == nil || err.Error() != wantErr {
		t.Fatalf("validate thread err = %v, want %q", err, wantErr)
	}
	if _, err := adapter.PostThought(context.Background(), otherOrgThread, "Thinking..."); err == nil || err.Error() != wantErr {
		t.Fatalf("thought err = %v, want %q", err, wantErr)
	}
	if _, err := adapter.PostMessage(context.Background(), chat.ThreadRef{ID: otherOrgThread, Adapter: adapterName}, chat.Text("Done")); err == nil || err.Error() != wantErr {
		t.Fatalf("post message err = %v, want %q", err, wantErr)
	}
}

type failingLinearTransport struct{}

func (f failingLinearTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected Linear API request")
}

func TestThreadIDKindRoundTrip(t *testing.T) {
	t.Parallel()

	agent := threadPayload{Organization: "ORG1", Issue: "ISSUE1", Session: "S1", Kind: threadKindAgentSession}
	agentID, err := encodeThreadID(agent)
	if err != nil {
		t.Fatalf("encode agent: %v", err)
	}
	gotAgent, err := decodeThreadID(agentID)
	if err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if gotAgent.kind() != threadKindAgentSession || gotAgent.Session != "S1" {
		t.Fatalf("agent payload = %#v", gotAgent)
	}

	comment := threadPayload{Organization: "ORG1", Issue: "ISSUE1", Comment: "CM1", Kind: threadKindComment}
	commentID, err := encodeThreadID(comment)
	if err != nil {
		t.Fatalf("encode comment: %v", err)
	}
	gotComment, err := decodeThreadID(commentID)
	if err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if gotComment.kind() != threadKindComment || gotComment.Comment != "CM1" {
		t.Fatalf("comment payload = %#v", gotComment)
	}
}

func TestThreadIDBackwardCompatibleAgentSession(t *testing.T) {
	t.Parallel()

	// A Thread ID minted before the kind discriminator carries no "kind" field.
	legacy, err := encodeThreadID(threadPayload{Organization: "ORG1", Issue: "ISSUE1", Session: "S1"})
	if err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	payload, err := decodeThreadID(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if payload.Kind != "" {
		t.Fatalf("legacy kind = %q, want empty", payload.Kind)
	}
	if payload.kind() != threadKindAgentSession {
		t.Fatalf("legacy effective kind = %q", payload.kind())
	}
}

func TestEncodeThreadIDValidation(t *testing.T) {
	t.Parallel()

	if _, err := encodeThreadID(threadPayload{Organization: "ORG1", Issue: "ISSUE1", Kind: threadKindComment}); err == nil {
		t.Fatal("expected comment thread without comment id to fail")
	}
	if _, err := encodeThreadID(threadPayload{Organization: "ORG1", Issue: "ISSUE1"}); err == nil {
		t.Fatal("expected agent-session thread without session to fail")
	}
	if _, err := encodeThreadID(threadPayload{Organization: "ORG1", Issue: "ISSUE1", Kind: "bogus"}); err == nil {
		t.Fatal("expected unknown kind to fail")
	}
}

func TestDecodeThreadIDRejectsMalformed(t *testing.T) {
	t.Parallel()

	if _, err := decodeThreadID(chat.ThreadID("slack:v1:abc")); err == nil {
		t.Fatal("expected wrong-prefix id to fail")
	}
	if _, err := decodeThreadID(chat.ThreadID("linear:v1:not-base64!!")); err == nil {
		t.Fatal("expected non-base64 id to fail")
	}
}
