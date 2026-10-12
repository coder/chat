package slackapitest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// SignRequest sets the X-Slack-Request-Timestamp and X-Slack-Signature headers
// of r with the Slack v0 signature of body for secret at now, so that
// slackapi.VerifyRequest accepts the request.
func SignRequest(r *http.Request, body []byte, secret string, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(body)
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("X-Slack-Request-Timestamp", timestamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
}
