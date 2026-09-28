package slackapi_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// The test vector from Slack's "Verifying requests from Slack" guide.
const (
	coreVectorSecret    = "8f742231b10e8888abcd99yyyzzz85a5"
	coreVectorTimestamp = int64(1531420618)
	coreVectorSignature = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
	coreVectorBody      = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
)

// coreVectorHeader returns the signature headers of the Slack test vector.
func coreVectorHeader() http.Header {
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(coreVectorTimestamp, 10))
	h.Set("X-Slack-Signature", coreVectorSignature)
	return h
}

func TestVerifyRequest(t *testing.T) {
	t.Parallel()

	signedAt := time.Unix(coreVectorTimestamp, 0)
	tests := []struct {
		name      string
		header    func() http.Header
		body      string
		secret    string
		now       time.Time
		tolerance time.Duration
		wantErr   string
	}{
		{name: "slack test vector", header: coreVectorHeader, now: signedAt},
		{name: "at the tolerance edge", header: coreVectorHeader, now: signedAt.Add(slackapi.DefaultSignatureTolerance)},
		{name: "wrong secret", header: coreVectorHeader, secret: "wrong", now: signedAt, wantErr: "slack: signature mismatch"},
		{name: "tampered body", header: coreVectorHeader, body: coreVectorBody + "&x=1", now: signedAt, wantErr: "slack: signature mismatch"},
		{name: "stale timestamp", header: coreVectorHeader, now: signedAt.Add(slackapi.DefaultSignatureTolerance + time.Second), wantErr: "slack: signature timestamp outside tolerance"},
		{name: "future timestamp", header: coreVectorHeader, now: signedAt.Add(-slackapi.DefaultSignatureTolerance - time.Second), wantErr: "slack: signature timestamp outside tolerance"},
		{name: "custom tolerance", header: coreVectorHeader, now: signedAt.Add(2 * time.Second), tolerance: time.Second, wantErr: "slack: signature timestamp outside tolerance"},
		{name: "negative tolerance uses default", header: coreVectorHeader, now: signedAt.Add(time.Minute), tolerance: -time.Second},
		{
			name: "missing timestamp",
			header: func() http.Header {
				h := coreVectorHeader()
				h.Del("X-Slack-Request-Timestamp")
				return h
			},
			now:     signedAt,
			wantErr: "slack: missing signature timestamp",
		},
		{
			name: "missing signature",
			header: func() http.Header {
				h := coreVectorHeader()
				h.Del("X-Slack-Signature")
				return h
			},
			now:     signedAt,
			wantErr: "slack: missing signature",
		},
		{
			name: "non-integer timestamp",
			header: func() http.Header {
				h := coreVectorHeader()
				h.Set("X-Slack-Request-Timestamp", "1531420618.5")
				return h
			},
			now:     signedAt,
			wantErr: "slack: invalid signature timestamp",
		},
		{name: "empty secret", header: coreVectorHeader, secret: "-", now: signedAt, wantErr: "slack: signing secret is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := tt.body
			if body == "" {
				body = coreVectorBody
			}
			secret := tt.secret
			switch secret {
			case "":
				secret = coreVectorSecret
			case "-":
				secret = ""
			}
			err := slackapi.VerifyRequest(tt.header(), []byte(body), secret, tt.now, tt.tolerance)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyRequest: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestSignRequestRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("slack test vector", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(coreVectorBody))
		slackapitest.SignRequest(req, []byte(coreVectorBody), coreVectorSecret, time.Unix(coreVectorTimestamp, 0))
		if got := req.Header.Get("X-Slack-Signature"); got != coreVectorSignature {
			t.Fatalf("signature = %q, want %q", got, coreVectorSignature)
		}
	})

	t.Run("verify", func(t *testing.T) {
		t.Parallel()

		body := []byte(`{"type":"event_callback","team_id":"T1"}`)
		now := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
		slackapitest.SignRequest(req, body, "secret", now)
		if err := slackapi.VerifyRequest(req.Header, body, "secret", now, 0); err != nil {
			t.Fatalf("VerifyRequest: %v", err)
		}
		if err := slackapi.VerifyRequest(req.Header, body, "other", now, 0); err == nil {
			t.Fatal("VerifyRequest accepted a different secret")
		}
	})
}
