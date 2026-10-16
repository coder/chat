package slackapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Webhook is the HTTP transport for inbound Slack payloads. It serves the
// Events API request URL, the slash command request URL, and the
// interactivity request URL, and makes a Delivery for Handler from each
// request. It is safe for concurrent use.
//
// ServeHTTP does these steps:
//
//  1. It accepts only POST (else 405) and a body of at most 1 MiB (else
//     413).
//  2. It checks the request signature with VerifyRequest before it parses
//     the body (else 401).
//  3. A form body with a "payload" field becomes a DeliveryInteractive.
//     A form body with a "command" field becomes a DeliverySlashCommands,
//     encoded as a JSON object of strings, which is the payload that Socket
//     Mode sends. Another form body, or a "payload" field that is not valid
//     JSON, is a 400.
//  4. A JSON body of type EnvelopeURLVerification gets the challenge as the
//     answer, and Handler does not see it. Another JSON body becomes a
//     DeliveryEventsAPI. A body that is not valid JSON is a 400.
//  5. The X-Slack-Retry-Num and X-Slack-Retry-Reason headers become
//     Delivery.RetryAttempt and Delivery.RetryReason.
//  6. The result of Handler gives the status, as the Handler documentation
//     shows.
type Webhook struct {
	// SigningSecret is the signing secret of the Slack app. It is required.
	SigningSecret string
	// Handler handles each delivery. It is required.
	Handler Handler
	// Now returns the time for the signature check. Nil uses time.Now.
	Now func() time.Time
	// SignatureTolerance is the maximum clock skew for the signature check.
	// Zero or less uses DefaultSignatureTolerance.
	SignatureTolerance time.Duration
}

var _ http.Handler = (*Webhook)(nil)

// ServeHTTP handles one request from Slack.
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "slack: payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "slack: read body", http.StatusBadRequest)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if err := VerifyRequest(r.Header, body, h.SigningSecret, now(), h.SignatureTolerance); err != nil {
		http.Error(w, "slack: invalid signature", http.StatusUnauthorized)
		return
	}

	d, challenge, err := parseRequest(r.Header, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if d == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, challenge)
		return
	}

	response, err := h.Handler(r.Context(), d)
	switch {
	case err == nil:
		writeResponse(w, response)
	case errors.Is(err, ErrBusy):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, ErrInvalidPayload):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// parseRequest returns the Delivery of a verified request body. For a
// url_verification request it returns a nil Delivery and the challenge.
func parseRequest(header http.Header, body []byte) (*Delivery, string, error) {
	d := &Delivery{RetryReason: header.Get("X-Slack-Retry-Reason")}
	if attempt, err := strconv.Atoi(header.Get("X-Slack-Retry-Num")); err == nil {
		d.RetryAttempt = attempt
	}

	if isFormContentType(header.Get("Content-Type")) {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, "", errors.New("slack: invalid form body")
		}
		switch {
		case form.Has("payload"):
			payload := form.Get("payload")
			if !json.Valid([]byte(payload)) {
				return nil, "", errors.New("slack: invalid interactivity payload")
			}
			d.Type = DeliveryInteractive
			d.Payload = json.RawMessage(payload)
		case form.Has("command"):
			fields := make(map[string]string, len(form))
			for key := range form {
				fields[key] = form.Get(key)
			}
			payload, err := json.Marshal(fields)
			if err != nil {
				return nil, "", err
			}
			d.Type = DeliverySlashCommands
			d.Payload = payload
		default:
			return nil, "", errors.New("slack: unsupported form body")
		}
		return d, "", nil
	}

	var head struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, "", errors.New("slack: invalid JSON body")
	}
	if head.Type == EnvelopeURLVerification {
		return nil, head.Challenge, nil
	}
	d.Type = DeliveryEventsAPI
	d.Payload = json.RawMessage(body)
	return d, "", nil
}

// writeResponse writes a 200 with response as the JSON body, or with no body
// when response is nil.
func writeResponse(w http.ResponseWriter, response any) {
	if response == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "slack: encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func isFormContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}
