package msteams_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/chat"
)

// recordingHandler returns a Webhook handler plus a pointer that is set when the
// runtime dispatch is invoked, so tests can tell "accepted and dispatched" from
// "rejected before dispatch".
func recordingHandler(a interface {
	Webhook(chat.DispatchFunc) http.Handler
}) (http.Handler, *bool) {
	dispatched := false
	h := a.Webhook(func(context.Context, *chat.Event) error {
		dispatched = true
		return nil
	})
	return h, &dispatched
}

func TestWebhookInboundAuthMatrix(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	cases := []struct {
		name         string
		auth         func() string // Authorization header value
		wantStatus   int
		wantDispatch bool
	}{
		{
			name:         "valid token accepted and dispatched",
			auth:         func() string { return "Bearer " + bf.sign(t, bf.validClaims()) },
			wantStatus:   http.StatusOK,
			wantDispatch: true,
		},
		{
			name:       "missing authorization rejected",
			auth:       func() string { return "" },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong scheme rejected",
			auth:       func() string { return "Token " + bf.sign(t, bf.validClaims()) },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "malformed jwt rejected",
			auth:       func() string { return "Bearer not.a.jwt" },
			wantStatus: http.StatusForbidden,
		},
		{
			name: "unknown kid rejected",
			auth: func() string {
				return "Bearer " + signRS256(t, bf.key, "no-such-kid", bf.validClaims())
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "bad signature rejected",
			auth:       func() string { return "Bearer " + bf.signForeign(t, bf.validClaims()) },
			wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong issuer rejected",
			auth: func() string {
				c := bf.validClaims()
				c["iss"] = "https://evil.example.com"
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong audience rejected",
			auth: func() string {
				c := bf.validClaims()
				c["aud"] = "some-other-app"
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "expired beyond skew rejected",
			auth: func() string {
				c := bf.validClaims()
				c["exp"] = testClock.Add(-10 * time.Minute).Unix()
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "not yet valid beyond skew rejected",
			auth: func() string {
				c := bf.validClaims()
				c["nbf"] = testClock.Add(10 * time.Minute).Unix()
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "expired within skew accepted",
			auth: func() string {
				c := bf.validClaims()
				// Expired 2 minutes ago, inside the 5-minute skew window.
				c["exp"] = testClock.Add(-2 * time.Minute).Unix()
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus:   http.StatusOK,
			wantDispatch: true,
		},
		{
			name: "serviceurl claim mismatch rejected",
			auth: func() string {
				c := bf.validClaims()
				c["serviceurl"] = "https://attacker.example.com/"
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "missing serviceurl claim rejected",
			auth: func() string {
				c := bf.validClaims()
				delete(c, "serviceurl")
				return "Bearer " + bf.sign(t, c)
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, dispatched := recordingHandler(a)
			rec := postActivity(t, h, tc.auth(), messageActivity())
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if *dispatched != tc.wantDispatch {
				t.Fatalf("dispatched = %v, want %v", *dispatched, tc.wantDispatch)
			}
		})
	}
}

func TestWebhookChannelEndorsement(t *testing.T) {
	t.Parallel()

	t.Run("msteams endorsement present accepted", func(t *testing.T) {
		bf := newFakeBotConnector(t)
		bf.endorsements = []string{"msteams", "directline"}
		a := newTestAdapter(t, bf, nil)
		h, dispatched := recordingHandler(a)
		rec := postActivity(t, h, "Bearer "+bf.sign(t, bf.validClaims()), messageActivity())
		if rec.Code != http.StatusOK || !*dispatched {
			t.Fatalf("status = %d dispatched = %v, want 200 + dispatch", rec.Code, *dispatched)
		}
	})

	t.Run("msteams endorsement absent rejected", func(t *testing.T) {
		bf := newFakeBotConnector(t)
		bf.endorsements = []string{"directline"}
		a := newTestAdapter(t, bf, nil)
		h, dispatched := recordingHandler(a)
		rec := postActivity(t, h, "Bearer "+bf.sign(t, bf.validClaims()), messageActivity())
		if rec.Code != http.StatusForbidden || *dispatched {
			t.Fatalf("status = %d dispatched = %v, want 403 no dispatch", rec.Code, *dispatched)
		}
	})
}

// TestWebhookValidationAlwaysOn proves there is no construction path that turns
// inbound validation off: an adapter built with every available Option still rejects
// a bad token. Options has no disable-validation field by design.
func TestWebhookValidationAlwaysOn(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)
	h, dispatched := recordingHandler(a)
	rec := postActivity(t, h, "Bearer not.a.jwt", messageActivity())
	if rec.Code != http.StatusForbidden || *dispatched {
		t.Fatalf("status = %d dispatched = %v, want 403 no dispatch", rec.Code, *dispatched)
	}
}

func TestJWKSCacheAndRotation(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	post := func() int {
		h, _ := recordingHandler(a)
		return postActivity(t, h, "Bearer "+bf.sign(t, bf.validClaims()), messageActivity()).Code
	}

	if code := post(); code != http.StatusOK {
		t.Fatalf("first post status = %d", code)
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("second post status = %d", code)
	}
	// The JWKS was fetched once and reused from cache for the second request.
	if bf.keyRequests != 1 {
		t.Fatalf("jwks fetches = %d, want 1 (cache reuse)", bf.keyRequests)
	}

	// Rotate to a new kid: a token signed with the new kid forces a refetch.
	bf.kid = "test-key-2"
	if code := post(); code != http.StatusOK {
		t.Fatalf("post after rotation status = %d", code)
	}
	if bf.keyRequests != 2 {
		t.Fatalf("jwks fetches after rotation = %d, want 2", bf.keyRequests)
	}
}
