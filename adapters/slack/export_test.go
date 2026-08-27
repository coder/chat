package slack

import "github.com/coder/chat"

// ResponseURLForTest exposes the preserved response_url from a Command.Raw /
// Interaction.Raw escape hatch for tests in the slack_test package.
func ResponseURLForTest(raw any) string {
	return responseURLFromRaw(raw)
}

// EncodeChannelThreadIDForTest builds an opaque channel-rooted Thread ID for
// native-posting tests.
func EncodeChannelThreadIDForTest(team, channel string) chat.ThreadID {
	id, err := encodeThreadID(threadPayload{Team: team, Channel: channel, Root: channel})
	if err != nil {
		panic(err)
	}
	return id
}

// EncodeThreadReplyThreadIDForTest builds an opaque thread-rooted Thread ID (a
// channel message with a distinct root ts) for history-read tests.
func EncodeThreadReplyThreadIDForTest(team, channel, root string) chat.ThreadID {
	id, err := encodeThreadID(threadPayload{Team: team, Channel: channel, Root: root})
	if err != nil {
		panic(err)
	}
	return id
}

// EncodeDirectThreadIDForTest builds an opaque direct-message Thread ID for
// history-read tests that exercise the conversations.history path.
func EncodeDirectThreadIDForTest(team, channel string) chat.ThreadID {
	id, err := encodeThreadID(threadPayload{Team: team, Channel: channel, Direct: true})
	if err != nil {
		panic(err)
	}
	return id
}
