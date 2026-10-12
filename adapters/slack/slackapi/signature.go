package slackapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// DefaultSignatureTolerance is the maximum clock skew that VerifyRequest accepts
// between the request timestamp and now when the tolerance argument is zero or
// less.
const DefaultSignatureTolerance = 5 * time.Minute

// VerifyRequest checks the Slack v0 request signature of an inbound webhook. It
// computes the HMAC-SHA256 of "v0:<X-Slack-Request-Timestamp>:<body>" with the
// signing secret and compares it to the X-Slack-Signature header. It rejects an
// empty secret, a missing header, a timestamp that is not an integer, and a
// timestamp more than tolerance away from now in either direction. A tolerance of
// zero or less uses DefaultSignatureTolerance.
func VerifyRequest(h http.Header, body []byte, secret string, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return errors.New("slack: signing secret is required")
	}
	timestampHeader := h.Get("X-Slack-Request-Timestamp")
	if timestampHeader == "" {
		return errors.New("slack: missing signature timestamp")
	}
	signature := h.Get("X-Slack-Signature")
	if signature == "" {
		return errors.New("slack: missing signature")
	}
	timestamp, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return errors.New("slack: invalid signature timestamp")
	}
	if tolerance <= 0 {
		tolerance = DefaultSignatureTolerance
	}
	skew := now.Sub(time.Unix(timestamp, 0))
	if skew > tolerance || skew < -tolerance {
		return errors.New("slack: signature timestamp outside tolerance")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestampHeader + ":"))
	_, _ = mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return errors.New("slack: signature mismatch")
	}
	return nil
}
