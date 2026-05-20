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
