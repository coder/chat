package slackapi

import (
	"context"
	"encoding/json"
	"errors"
)

// maxPayloadBytes is the largest inbound payload that Webhook and Listen
// accept.
const maxPayloadBytes = 1 << 20

// DeliveryType is the kind of payload in a Delivery. The values are the
// Socket Mode envelope types.
type DeliveryType string

// Delivery types.
const (
	// DeliveryEventsAPI is an Events API payload: the same JSON body that
	// Slack posts to the Events API request URL, which ParseEnvelope decodes.
	DeliveryEventsAPI DeliveryType = "events_api"
	// DeliverySlashCommands is a slash command: a JSON object with one string
	// value for each command field, for example "command", "text", and
	// "team_id".
	DeliverySlashCommands DeliveryType = "slash_commands"
	// DeliveryInteractive is an interactivity payload, for example a
	// block_actions payload.
	DeliveryInteractive DeliveryType = "interactive"
)

// Delivery is one inbound payload from Slack. It is the same for both
// transports: Webhook makes one from an HTTP request, and Listen makes one
// from a Socket Mode envelope.
type Delivery struct {
	// Type is the kind of payload. Listen also passes an envelope type that
	// is not one of the DeliveryType constants.
	Type DeliveryType
	// Payload is the JSON payload. Its shape depends on Type.
	Payload json.RawMessage
	// RetryAttempt is the number of the redelivery. It is 0 on the first
	// delivery.
	RetryAttempt int
	// RetryReason is the reason that Slack gives for a redelivery, for example
	// "timeout". It is empty on the first delivery.
	RetryReason string
}

// Handler handles one Delivery and gives the acknowledgement. A nil error
// acknowledges the delivery. When response is not nil, it is the
// acknowledgement body, encoded as JSON. A non-nil error does not acknowledge
// the delivery:
//
//   - ErrBusy: Webhook answers 503. Listen sends no ack.
//   - ErrInvalidPayload: Webhook answers 400. Listen sends no ack.
//   - Another error: Webhook answers 500. Listen sends no ack.
//
// Slack redelivers an Events API payload that it does not get an ack for.
// Slack expects the acknowledgement within 3 seconds, so a Handler must not
// do slow work before it returns.
type Handler func(ctx context.Context, d *Delivery) (response any, err error)

var (
	// ErrBusy is the error that a Handler returns, alone or wrapped, when it
	// cannot accept a delivery now. Slack can redeliver it later.
	ErrBusy = errors.New("slack: busy")
	// ErrInvalidPayload is the error that a Handler returns, alone or
	// wrapped, when a payload is malformed or not valid for the app.
	ErrInvalidPayload = errors.New("slack: invalid payload")
)
