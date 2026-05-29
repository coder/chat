package slack

import "github.com/coder/chat"

// ResponseURLForTest exposes the preserved response_url from a Command.Raw /
// Interaction.Raw escape hatch for tests in the slack_test package.
func ResponseURLForTest(raw any) string {
	return responseURLFromRaw(raw)
}

// TriggerIDForTest exposes the preserved trigger_id from a command escape hatch.
func TriggerIDForTest(raw any) string {
	switch v := raw.(type) {
	case commandForm:
		return v.TriggerID
	case *commandForm:
		return v.TriggerID
	case interactionPayload:
		return v.TriggerID
	case *interactionPayload:
		return v.TriggerID
	default:
		return ""
	}
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
