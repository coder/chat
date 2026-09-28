package slackapi

import (
	"strings"
	"unicode/utf8"
)

// MarkdownBlockLimit is the maximum number of characters that Slack accepts
// in the text of a markdown block and in the markdown_text field of a
// message.
const MarkdownBlockLimit = 12000

const (
	fenceMarker = "```"
	fenceClose  = "\n" + fenceMarker
)

// SplitMarkdown splits text into chunks of at most limit runes each, so that
// each chunk fits in one MarkdownBlock. A limit of zero or less means
// MarkdownBlockLimit. Empty text returns nil. Text within the limit returns
// one chunk equal to text.
//
// SplitMarkdown splits each chunk at the last blank line within the limit.
// If there is no blank line, it splits at the last newline, then at the last
// space, and then at a rune boundary. It drops the blank line, newline, or
// space at the split point. It does not remove or reorder other text.
//
// A line that starts with three backticks opens or closes a fenced code
// block. If a chunk ends inside a fenced code block, SplitMarkdown closes the
// fence at the end of that chunk and opens it again with the same opening
// line at the start of the next chunk. The added fence lines count toward
// the limit. If limit is less than twice the length of the longest fence
// line plus six runes, the chunks cannot hold the added fence lines, and
// SplitMarkdown splits the text without this fence repair.
//
// SplitMarkdown is not a Markdown converter. It does not change the text in
// any other way.
func SplitMarkdown(text string, limit int) []string {
	if limit <= 0 {
		limit = MarkdownBlockLimit
	}
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	// In the worst case a chunk holds a reopened fence line and its newline,
	// a newline and a fence line, and the added closing fence. With less room,
	// a split can fall inside a fence line.
	repairFences := limit >= 2*longestFenceLine(text)+6

	var chunks []string
	// reopen is the opening line of the fenced code block that the previous
	// chunk closed, or "" when the previous chunk ended outside a fence.
	reopen := ""
	for start := 0; start < len(text); {
		prefix := ""
		if reopen != "" {
			prefix = reopen + "\n"
		}
		budget := limit - utf8.RuneCountInString(prefix)
		rest := text[start:]
		if runeOffset(rest, budget) == len(rest) {
			chunks = append(chunks, prefix+rest)
			break
		}

		end, next := splitPoint(rest, budget)
		open := ""
		if repairFences {
			open = openFenceAt(text, start, start+end, reopen)
			if open != "" {
				end, next = splitPoint(rest, budget-len(fenceClose))
				open = openFenceAt(text, start, start+end, reopen)
			}
		}
		chunk := prefix + rest[:end]
		if open != "" {
			chunk += fenceClose
		}
		chunks = append(chunks, chunk)
		reopen = open
		start += next
	}
	return chunks
}

// splitPoint returns where to split s so that s[:end] holds at most budget
// runes. The text in s[end:next] is the dropped separator. The caller must
// make sure that s holds more than budget runes and that budget is positive.
func splitPoint(s string, budget int) (end, next int) {
	w := runeOffset(s, budget)
	if i := strings.LastIndex(s[:min(w+2, len(s))], "\n\n"); i > 0 {
		return i, i + 2
	}
	if i := strings.LastIndexByte(s[:w+1], '\n'); i > 0 {
		return i, i + 1
	}
	if i := strings.LastIndexByte(s[:w+1], ' '); i > 0 {
		return i, i + 1
	}
	return w, w
}

// runeOffset returns the byte offset in s after the first n runes, or len(s)
// if s holds n runes or fewer.
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// openFenceAt returns the opening line of the fenced code block that is open
// at byte offset to of text, or "" if no fence is open there. open is the
// opening line of the fence that is open at byte offset from, or "".
func openFenceAt(text string, from, to int, open string) string {
	p := from
	if p > 0 && text[p-1] != '\n' {
		i := strings.IndexByte(text[p:], '\n')
		if i < 0 {
			return open
		}
		p += i + 1
	}
	for p < to {
		line, _, _ := strings.Cut(text[p:], "\n")
		if strings.HasPrefix(line, fenceMarker) {
			if open == "" {
				open = line
			} else {
				open = ""
			}
		}
		p += len(line) + 1
	}
	return open
}

// longestFenceLine returns the length in runes of the longest line in text
// that starts with three backticks, or zero if there is no such line.
func longestFenceLine(text string) int {
	longest := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, fenceMarker) {
			longest = max(longest, utf8.RuneCountInString(line))
		}
	}
	return longest
}
