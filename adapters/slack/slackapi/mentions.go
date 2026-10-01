package slackapi

import (
	"slices"
	"strings"
)

// UserMentions returns the user IDs of the user mentions in text, in order of
// first appearance and without duplicates. A user mention has the form
// <@U123> or <@U123|label>, where the ID starts with U or W and continues
// with uppercase ASCII letters or digits. Channel mentions such as <#C123>,
// special mentions such as <!here>, and links are not user mentions.
// UserMentions returns nil if text has no user mentions.
func UserMentions(text string) []string {
	var ids []string
	for {
		_, end, id, ok := nextUserMention(text)
		if !ok {
			return ids
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		text = text[end:]
	}
}

// ReplaceUserMentions returns text with each user mention replaced by "@"
// and the name that name returns for the user ID. If name returns false,
// the mention stays as it is, including any label. ReplaceUserMentions calls
// name once for each mention. See UserMentions for the mention forms.
func ReplaceUserMentions(text string, name func(id string) (string, bool)) string {
	var b strings.Builder
	rest := text
	for {
		start, end, id, ok := nextUserMention(rest)
		if !ok {
			break
		}
		b.WriteString(rest[:start])
		if n, ok := name(id); ok {
			b.WriteString("@")
			b.WriteString(n)
		} else {
			b.WriteString(rest[start:end])
		}
		rest = rest[end:]
	}
	if b.Len() == 0 {
		return text
	}
	b.WriteString(rest)
	return b.String()
}

// nextUserMention returns the byte offsets and the user ID of the first user
// mention in s.
func nextUserMention(s string) (start, end int, id string, ok bool) {
	for offset := 0; ; {
		i := strings.Index(s[offset:], "<@")
		if i < 0 {
			return 0, 0, "", false
		}
		start = offset + i
		if n, id, ok := parseUserMention(s[start:]); ok {
			return start, start + n, id, true
		}
		offset = start + len("<@")
	}
}

// parseUserMention parses the user mention at the start of s. It returns the
// length of the mention in bytes and the user ID.
func parseUserMention(s string) (n int, id string, ok bool) {
	rest, _ := strings.CutPrefix(s, "<@")
	if rest == "" || (rest[0] != 'U' && rest[0] != 'W') {
		return 0, "", false
	}
	i := 1
	for i < len(rest) && isIDByte(rest[i]) {
		i++
	}
	if i == 1 || i == len(rest) {
		return 0, "", false
	}
	id = rest[:i]
	switch rest[i] {
	case '>':
		return len("<@") + i + 1, id, true
	case '|':
		j := strings.IndexAny(rest[i+1:], "<>")
		if j < 0 || rest[i+1+j] != '>' {
			return 0, "", false
		}
		return len("<@") + i + 1 + j + 1, id, true
	default:
		return 0, "", false
	}
}

func isIDByte(c byte) bool {
	return ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}
