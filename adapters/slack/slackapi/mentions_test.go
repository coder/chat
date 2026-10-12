package slackapi_test

import (
	"slices"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func TestUserMentions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "None", text: "hello", want: nil},
		{name: "Plain", text: "hi <@U123>", want: []string{"U123"}},
		{name: "Labeled", text: "hi <@U123|alice>", want: []string{"U123"}},
		{name: "EmptyLabel", text: "<@U123|>", want: []string{"U123"}},
		{name: "EnterpriseID", text: "<@W0ABC9>", want: []string{"W0ABC9"}},
		{name: "OrderAndRepeats", text: "<@U2> <@U1|a> <@U2|b> <@U1> <@U3>", want: []string{"U2", "U1", "U3"}},
		{name: "Adjacent", text: "<@U1><@U2|b><@U3>", want: []string{"U1", "U2", "U3"}},
		{name: "EmptyID", text: "<@>", want: nil},
		{name: "PrefixOnly", text: "<@U>", want: nil},
		{name: "Unterminated", text: "hi <@U123", want: nil},
		{name: "UnterminatedLabel", text: "hi <@U123|alice", want: nil},
		{name: "UnterminatedBeforeMention", text: "<@U1 <@U2> <@U3|x <@U4|y>", want: []string{"U2", "U4"}},
		{name: "Lowercase", text: "<@u123>", want: nil},
		{name: "WrongPrefix", text: "<@B123> <@C123>", want: nil},
		{name: "InvalidIDByte", text: "<@U12-3>", want: nil},
		{name: "Channel", text: "<#C123> <#C123|general>", want: nil},
		{name: "Special", text: "<!here> <!channel> <!subteam^S123|@team>", want: nil},
		{name: "Link", text: "<https://example.com/@U123> <mailto:a@example.com|a@example.com>", want: nil},
		{name: "AmongOtherForms", text: "<!here> <@U1> <#C1> <https://example.com>", want: []string{"U1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slackapi.UserMentions(tt.text); !slices.Equal(got, tt.want) {
				t.Fatalf("UserMentions(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestReplaceUserMentions(t *testing.T) {
	t.Parallel()

	names := map[string]string{"U1": "alice", "W2": "bob"}
	name := func(id string) (string, bool) {
		n, ok := names[id]
		return n, ok
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "None", text: "hello", want: "hello"},
		{name: "Plain", text: "hi <@U1>!", want: "hi @alice!"},
		{name: "Labeled", text: "hi <@W2|old-name>", want: "hi @bob"},
		{name: "Repeats", text: "<@U1> and <@U1|x>", want: "@alice and @alice"},
		{name: "Adjacent", text: "<@U1><@W2>", want: "@alice@bob"},
		{name: "Unknown", text: "<@U9> <@U9|label> <@U1>", want: "<@U9> <@U9|label> @alice"},
		{name: "Malformed", text: "<@> <@U1 <@U1|x", want: "<@> <@U1 <@U1|x"},
		{name: "NonUserForms", text: "<#C1> <!here> <https://example.com|link>", want: "<#C1> <!here> <https://example.com|link>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slackapi.ReplaceUserMentions(tt.text, name); got != tt.want {
				t.Fatalf("ReplaceUserMentions(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestReplaceUserMentionsCallsNameOncePerMention(t *testing.T) {
	t.Parallel()

	var calls []string
	got := slackapi.ReplaceUserMentions("<@U1> <#C1> <@U2|b> <@U1>", func(id string) (string, bool) {
		calls = append(calls, id)
		return "", false
	})
	if want := "<@U1> <#C1> <@U2|b> <@U1>"; got != want {
		t.Fatalf("ReplaceUserMentions() = %q, want %q", got, want)
	}
	if want := []string{"U1", "U2", "U1"}; !slices.Equal(calls, want) {
		t.Fatalf("name calls = %q, want %q", calls, want)
	}
}
