package linear

import "github.com/coder/chat"

// EncodeAgentSessionThreadIDForTest builds an opaque agent-session Thread ID for
// out-of-webhook (Thread Handle reconstruction) tests in the linear_test package.
func EncodeAgentSessionThreadIDForTest(org, issue, session string) chat.ThreadID {
	id, err := encodeThreadID(threadPayload{
		Organization: org,
		Issue:        issue,
		Session:      session,
		Kind:         threadKindAgentSession,
	})
	if err != nil {
		panic(err)
	}
	return id
}
