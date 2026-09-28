package slackapi

import (
	"strings"
	"unicode/utf8"
)

// MarkdownBlockLimit is the maximum number of characters that Slack accepts
// in the text of a markdown block and in the markdown_text field of a
// message.
const MarkdownBlockLimit = 12000

const fenceMarker = "```"

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
// A line that starts with three or more backticks opens a fenced code block.
// In a fenced code block, a line of at least as many backticks as the opening
// line, followed only by optional whitespace, closes it. Other lines in the
// block, for example a line of three backticks and an info string, are code.
// If a chunk ends inside a fenced code block, SplitMarkdown closes the fence
// at the end of that chunk with the backticks of the opening line, and opens
// it again with the same opening line at the start of the next chunk. The
// added fence lines count toward the limit. A chunk does not end inside or
// right after the line that opens a fenced code block, because that leaves an
// empty code block: SplitMarkdown splits before that line, or, when no text
// comes before it, in the code after it. If limit is less than twice the
// length of the longest fence line plus six runes, the chunks cannot hold the
// added fence lines, and SplitMarkdown splits the text without this fence
// repair.
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

		var end, next int
		open := ""
		if repairFences {
			end, next, open = splitFenced(text, start, budget, reopen)
		} else {
			end, next = splitPoint(rest, budget)
		}
		chunk := prefix + rest[:end]
		if open != "" {
			chunk += closingFence(open)
		}
		chunks = append(chunks, chunk)
		reopen = open
		start += next
	}
	return chunks
}

// splitFenced returns where to split text[start:] for a chunk of at most
// budget runes, like splitPoint, and the opening line of the fenced code
// block that is open at the split, or "". It leaves room for the closing
// fence of that block, and it does not split inside or right after the line
// that opens that block. reopen is the opening line of the fenced code block
// that is open at start, or "".
func splitFenced(text string, start, budget int, reopen string) (end, next int, open string) {
	rest := text[start:]
	from := 0
	// This loop runs at most twice. The second split starts after the opening
	// line at the start of rest, so it cannot end inside or right after that
	// line, and a later opening line has that line before it.
	for {
		var pos int
		end, next, open, pos = splitReserved(text, start, from, budget, reopen)
		if open == "" || pos < 0 || start+end > pos+len(open) {
			return end, next, open
		}
		if end, next := splitBefore(rest, pos-start); end > 0 {
			return end, next, ""
		}
		from = pos - start + len(open) + 1
	}
}

// splitReserved returns where to split text[start:], at or after byte offset
// from of text[start:], for a chunk of at most budget runes that includes the
// closing fence that SplitMarkdown adds. It also returns the opening line of
// the fenced code block that is open at the split and the byte offset of that
// line in text, as openFenceAt does.
func splitReserved(text string, start, from, budget int, reopen string) (end, next int, open string, pos int) {
	rest := text[start:]
	budget -= utf8.RuneCountInString(rest[:from])
	for reserve := 0; ; {
		e, n := splitPoint(rest[from:], budget-reserve)
		end, next = from+e, from+n
		open, pos = openFenceAt(text, start, start+end, reopen)
		need := 0
		if open != "" {
			need = utf8.RuneCountInString(closingFence(open))
		}
		if need <= reserve {
			return end, next, open, pos
		}
		reserve = need
	}
}

// splitBefore returns a split of s that ends before the line at byte offset
// line, and drops the newline or the blank line before that line. It returns
// end 0 when no text comes before that line.
func splitBefore(s string, line int) (end, next int) {
	if line == 0 {
		return 0, 0
	}
	end = line - 1
	if end > 0 && s[end-1] == '\n' {
		end--
	}
	return end, line
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
// opening line of the fence that is open at byte offset from, or "". The
// returned offset is the byte offset in text of the returned line, or -1 when
// no fence is open or the fence was already open at from.
func openFenceAt(text string, from, to int, open string) (string, int) {
	pos := -1
	p := from
	if p > 0 && text[p-1] != '\n' {
		i := strings.IndexByte(text[p:], '\n')
		if i < 0 {
			return open, pos
		}
		p += i + 1
	}
	for p < to {
		line, _, _ := strings.Cut(text[p:], "\n")
		switch {
		case open == "":
			if fenceRun(line) > 0 {
				open, pos = line, p
			}
		case closesFence(line, open):
			open, pos = "", -1
		}
		p += len(line) + 1
	}
	return open, pos
}

// fenceRun returns the number of backticks at the start of line when there
// are three or more, else zero.
func fenceRun(line string) int {
	n := len(line) - len(strings.TrimLeft(line, "`"))
	if n < len(fenceMarker) {
		return 0
	}
	return n
}

// closesFence reports whether line closes the fenced code block that the
// line open opens.
func closesFence(line, open string) bool {
	n := fenceRun(line)
	return n > 0 && n >= fenceRun(open) && strings.TrimSpace(line[n:]) == ""
}

// closingFence returns a newline and the backticks that close the fenced code
// block that the line open opens.
func closingFence(open string) string {
	return "\n" + open[:fenceRun(open)]
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
