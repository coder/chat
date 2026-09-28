package slackapi

import (
	"encoding/json"
	"fmt"
	"strings"
)

// APIError is returned when Slack rejects a Web API call. It covers an ok:false
// response and a non-2xx HTTP status that is not a rate limit.
type APIError struct {
	// Method is the Web API method, for example "chat.postMessage".
	Method string
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Code is the Slack error code from the "error" field, for example
	// "channel_not_found". It is empty for a non-2xx response.
	Code string
	// Detail is extra text from the response: the "needed" scope when present,
	// else the response_metadata messages joined with "; ".
	Detail string
	// Raw is the response body.
	Raw json.RawMessage
}

// Error returns "slack: <method> failed: <code>" for an ok:false response and
// "slack: <method> status <status>" for a non-2xx response.
func (e *APIError) Error() string {
	if e.StatusCode != 0 && !successStatus(e.StatusCode) {
		return fmt.Sprintf("slack: %s status %d", e.Method, e.StatusCode)
	}
	return fmt.Sprintf("slack: %s failed: %s", e.Method, e.Code)
}

var _ error = (*APIError)(nil)

// responseEnvelope holds the fields that every Web API response shares.
type responseEnvelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	Needed           string `json:"needed"`
	ResponseMetadata struct {
		Messages []string `json:"messages"`
	} `json:"response_metadata"`
}

func (e responseEnvelope) detail() string {
	if e.Needed != "" {
		return e.Needed
	}
	return strings.Join(e.ResponseMetadata.Messages, "; ")
}

func successStatus(status int) bool {
	return status >= 200 && status <= 299
}
