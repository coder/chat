package msteams_test

import (
	"testing"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/msteams"
)

func TestThreadIDRoundTrip(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, newFakeBotConnector(t), nil)

	id := msteams.EncodeThreadIDForTest(testServiceURL, testConvID, testTenant, "msteams", "channel", true)
	ref, err := a.ValidateThreadID(id)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ref.ID != id || ref.Adapter != "msteams" || ref.Tenant != testTenant || ref.Channel != testConvID {
		t.Fatalf("ref = %#v", ref)
	}
	if ref.Direct {
		t.Fatal("channel thread should not be direct")
	}
	if _, ok := ref.Raw.(any); !ok || ref.Raw == nil {
		t.Fatal("thread ref should carry the conversation reference as Raw")
	}
}

func TestThreadIDDirectFromPersonalScope(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, newFakeBotConnector(t), nil)

	id := msteams.EncodeThreadIDForTest(testServiceURL, "19:dm", testTenant, "msteams", "personal", false)
	ref, err := a.ValidateThreadID(id)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !ref.Direct {
		t.Fatal("personal scope should decode as direct")
	}
}

func TestThreadIDRejectsInvalid(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, newFakeBotConnector(t), nil)

	bad := []chat.ThreadID{
		"",
		"slack:v1:abc",                // wrong adapter prefix
		"msteams:v1:!!!not-base64!!!", // undecodable
		"msteams:v1:e30",              // base64 of "{}" -> reference missing required fields
	}
	for _, id := range bad {
		if _, err := a.ValidateThreadID(id); err == nil {
			t.Fatalf("ValidateThreadID(%q) succeeded, want error", id)
		}
	}
}
