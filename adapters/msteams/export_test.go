package msteams

import "github.com/coder/chat"

// EncodeThreadIDForTest builds an opaque conversationReference Thread ID for tests
// in the msteams_test package (posting and round-trip tests).
func EncodeThreadIDForTest(serviceURL, conversationID, tenantID, channelID, conversationType string, isGroup bool) chat.ThreadID {
	id, err := encodeThreadID(conversationReference{
		ServiceURL:       serviceURL,
		ConversationID:   conversationID,
		TenantID:         tenantID,
		ChannelID:        channelID,
		ConversationType: conversationType,
		IsGroup:          isGroup,
	})
	if err != nil {
		panic(err)
	}
	return id
}
