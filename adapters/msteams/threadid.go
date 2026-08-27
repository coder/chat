package msteams

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/chat"
)

const (
	adapterName    = "msteams"
	threadIDPrefix = "msteams:v1:"
)

// conversationReference is the minimal subset of the Bot Framework
// conversationReference serialized into the opaque Thread ID -- enough to post back
// later (reply or proactive). Unlike Slack's short channel+ts id, it must carry
// serviceUrl (proactive posting can't reconstruct it) and tenantId (tenant scoping).
type conversationReference struct {
	ServiceURL       string `json:"service_url"`
	ConversationID   string `json:"conversation_id"`
	TenantID         string `json:"tenant_id"`
	BotID            string `json:"bot_id,omitempty"`
	ChannelID        string `json:"channel_id,omitempty"`
	ConversationType string `json:"conversation_type,omitempty"`
	IsGroup          bool   `json:"is_group,omitempty"`
}

// direct reports personal (DM) scope. Teams conversationType is the authoritative
// signal ("personal" vs "groupChat"/"channel"); isGroup is the fallback when the
// type is absent.
func (r conversationReference) direct() bool {
	if r.ConversationType != "" {
		return r.ConversationType == "personal"
	}
	return !r.IsGroup
}

// encodeThreadID serializes a conversationReference into a versioned, opaque Thread
// ID. The required fields (serviceUrl, conversation id, tenant id) are exactly what
// later posting needs; missing any of them is a programming error surfaced here.
func encodeThreadID(ref conversationReference) (chat.ThreadID, error) {
	if ref.ServiceURL == "" {
		return "", errors.New("msteams: thread service url is required")
	}
	if ref.ConversationID == "" {
		return "", errors.New("msteams: thread conversation id is required")
	}
	if ref.TenantID == "" {
		return "", errors.New("msteams: thread tenant id is required")
	}
	body, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return chat.ThreadID(threadIDPrefix + base64.RawURLEncoding.EncodeToString(body)), nil
}

// decodeThreadID reverses encodeThreadID, rejecting a wrong adapter prefix or any
// reference missing a load-bearing field.
func decodeThreadID(id chat.ThreadID) (conversationReference, error) {
	if !strings.HasPrefix(string(id), threadIDPrefix) {
		return conversationReference{}, fmt.Errorf("msteams: malformed thread id %q", id)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(id), threadIDPrefix))
	if err != nil {
		return conversationReference{}, fmt.Errorf("msteams: decode thread id: %w", err)
	}
	var ref conversationReference
	if err := json.Unmarshal(body, &ref); err != nil {
		return conversationReference{}, fmt.Errorf("msteams: parse thread id: %w", err)
	}
	if ref.ServiceURL == "" || ref.ConversationID == "" || ref.TenantID == "" {
		return conversationReference{}, fmt.Errorf("msteams: invalid thread id %q", id)
	}
	return ref, nil
}
