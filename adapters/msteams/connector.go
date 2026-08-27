package msteams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/chat"
)

// ErrBotNotInstalled is the explicit, unwrappable error for a Connector 403
// ForbiddenOperationException: the bot is not installed in the target conversation,
// so callers can distinguish "not installed" from a transient failure.
var ErrBotNotInstalled = errors.New("msteams: bot not installed in target conversation")

// ErrMessageWritesBlocked is the explicit, unwrappable error for a Connector 403
// MessageWritesBlocked: the user has blocked or uninstalled the bot.
var ErrMessageWritesBlocked = errors.New("msteams: message writes blocked by user")

// ConnectorError carries a non-throttling Connector failure: the HTTP status and
// the Bot Framework error envelope (code/message) as a Platform Escape Hatch. Known
// 403 codes set Err to one of the sentinels above so callers can errors.Is them.
type ConnectorError struct {
	Status  int
	Code    string
	Message string
	Raw     any
	Err     error
}

func (e *ConnectorError) Error() string {
	msg := fmt.Sprintf("msteams: connector status %d", e.Status)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *ConnectorError) Unwrap() error { return e.Err }

// PostMessage maps Thread.Post to the Connector "send to conversation" REST call,
// using the serviceUrl from the opaque Thread ID as the base URI. It is also the
// proactive-post path, so its 403s surface as ErrBotNotInstalled /
// ErrMessageWritesBlocked. The conversationReference is decoded from the Thread ID
// (the authoritative source), not trusted from ThreadRef.Raw.
func (a *Adapter) PostMessage(ctx context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	ref, err := decodeThreadID(thread.ID)
	if err != nil {
		return nil, err
	}
	textFormat, err := textFormatFor(msg.Format)
	if err != nil {
		return nil, err
	}
	payload := outboundActivity{
		Type:         "message",
		Text:         msg.Text,
		TextFormat:   textFormat,
		From:         outboundAccount{ID: a.botID, Name: a.botName},
		Conversation: outboundConversation{ID: ref.ConversationID},
	}
	endpoint := fmt.Sprintf("%s/v3/conversations/%s/activities",
		strings.TrimRight(ref.ServiceURL, "/"), url.PathEscape(ref.ConversationID))

	var resp resourceResponse
	if err := a.connectorCall(ctx, http.MethodPost, endpoint, payload, &resp); err != nil {
		return nil, err
	}
	return &chat.SentMessage{ID: resp.ID, ThreadID: thread.ID, Raw: resp}, nil
}

func textFormatFor(format chat.MessageFormat) (string, error) {
	switch format {
	case chat.MessageFormatText:
		return "plain", nil
	case chat.MessageFormatMarkdown:
		return "markdown", nil
	default:
		return "", fmt.Errorf("msteams: unsupported message format %d", format)
	}
}

// connectorCall authorizes with the cached outbound token and sends through the
// bounded rate-limit retry loop (ADR 0005).
func (a *Adapter) connectorCall(ctx context.Context, method, endpoint string, payload, dest any) error {
	token, err := a.tokens.get(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("msteams: encode connector request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return a.doWithRetry(ctx, endpoint, req, dest)
}

// connectorErrorFor builds a ConnectorError from a non-2xx, non-429 response, lifting
// the Bot Framework error envelope and mapping the documented 403 codes to sentinels.
func connectorErrorFor(status int, body []byte) *ConnectorError {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	ce := &ConnectorError{
		Status:  status,
		Code:    env.Error.Code,
		Message: env.Error.Message,
		Raw:     json.RawMessage(append([]byte(nil), body...)),
	}
	switch env.Error.Code {
	case "ForbiddenOperationException":
		ce.Err = ErrBotNotInstalled
	case "MessageWritesBlocked":
		ce.Err = ErrMessageWritesBlocked
	}
	return ce
}

type outboundActivity struct {
	Type         string               `json:"type"`
	Text         string               `json:"text"`
	TextFormat   string               `json:"textFormat,omitempty"`
	From         outboundAccount      `json:"from"`
	Conversation outboundConversation `json:"conversation"`
}

type outboundAccount struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type outboundConversation struct {
	ID string `json:"id"`
}

type resourceResponse struct {
	ID string `json:"id"`
}
