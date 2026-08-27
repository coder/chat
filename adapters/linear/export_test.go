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

// EncodeCommentThreadIDForTest builds an opaque issue-comment Thread ID for
// out-of-webhook (Thread Handle reconstruction) tests in the linear_test package.
func EncodeCommentThreadIDForTest(org, issue, comment string) chat.ThreadID {
	id, err := encodeThreadID(threadPayload{
		Organization: org,
		Issue:        issue,
		Comment:      comment,
		Kind:         threadKindComment,
	})
	if err != nil {
		panic(err)
	}
	return id
}
