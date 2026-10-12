package slackapi_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func TestSplitMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{name: "Empty", text: "", limit: 10, want: nil},
		{name: "WithinLimit", text: "hello\n\nworld", limit: 12, want: []string{"hello\n\nworld"}},
		{name: "BlankLineBeforeNewline", text: "aaa\n\nbbb\nccc ddd", limit: 12, want: []string{"aaa", "bbb\nccc ddd"}},
		{name: "LastBlankLine", text: "a\n\nb\n\nc\n\nlong tail", limit: 9, want: []string{"a\n\nb\n\nc", "long tail"}},
		{name: "NewlineBeforeSpace", text: "aaa\nbbb ccc", limit: 9, want: []string{"aaa", "bbb ccc"}},
		{name: "LastNewline", text: "a\nb\nc\nlong tail", limit: 9, want: []string{"a\nb\nc", "long tail"}},
		{name: "Space", text: "aaa bbb ccc", limit: 7, want: []string{"aaa bbb", "ccc"}},
		{name: "SeparatorJustPastLimit", text: "abc def", limit: 3, want: []string{"abc", "def"}},
		{name: "NoSpaces", text: "abcdefghij", limit: 3, want: []string{"abc", "def", "ghi", "j"}},
		{name: "LeadingSeparatorNotUsed", text: " abcdef", limit: 3, want: []string{" ab", "cde", "f"}},
		{name: "ExtraNewlineKept", text: "aaa\n\n\nbbb", limit: 5, want: []string{"aaa\n", "bbb"}},
		{name: "Emoji", text: "😀😁😂🤣😃", limit: 2, want: []string{"😀😁", "😂🤣", "😃"}},
		{name: "EmojiAtBoundary", text: "ab😀cd", limit: 3, want: []string{"ab😀", "cd"}},
		{name: "CJK", text: "你好世界你好", limit: 4, want: []string{"你好世界", "你好"}},
		{name: "CJKWithSpace", text: "你好 世界你好", limit: 5, want: []string{"你好", "世界你好"}},
		{name: "LimitOne", text: "ab c", limit: 1, want: []string{"a", "b", "c"}},
		{
			name:  "FenceWithInfoString",
			text:  "```go\nfmt.Println(1)\nfmt.Println(2)\n```",
			limit: 25,
			want: []string{
				"```go\nfmt.Println(1)\n```",
				"```go\nfmt.Println(2)\n```",
			},
		},
		{
			name:  "FenceSpansThreeChunks",
			text:  "```go\nline1\nline2\nline3\n```",
			limit: 20,
			want: []string{
				"```go\nline1\n```",
				"```go\nline2\n```",
				"```go\nline3\n```",
			},
		},
		{
			name:  "FenceWithSurroundingText",
			text:  "Intro text.\n\n```go\nx := 1\ny := 2\n```\n\nOutro.",
			limit: 20,
			want: []string{
				"Intro text.",
				"```go\nx := 1\n```",
				"```go\ny := 2\n```",
				"Outro.",
			},
		},
		{
			name:  "FenceClosedInChunk",
			text:  "```\na\n```\nbbbbbbbbbbbbbbbbbbbb",
			limit: 20,
			want:  []string{"```\na\n```", "bbbbbbbbbbbbbbbbbbbb"},
		},
		{
			name:  "FenceLineInMiddleOfLineIgnored",
			text:  "say ```go\nnow and then again",
			limit: 20,
			want:  []string{"say ```go", "now and then again"},
		},
		{
			name:  "UnclosedFenceAtEnd",
			text:  "```go\nline1\nline2",
			limit: 16,
			want:  []string{"```go\nline1\n```", "```go\nline2"},
		},
		{
			name:  "LimitTooSmallForFenceRepair",
			text:  "```go\nab\n```",
			limit: 5,
			want:  []string{"```go", "ab", "```"},
		},
		{
			name:  "FenceLineWithInfoStringInsideFence",
			text:  "```\nline one\n```go\nline two\n```",
			limit: 25,
			want: []string{
				"```\nline one\n```go\n```",
				"```\nline two\n```",
			},
		},
		{
			name:  "FourBacktickFenceWithThreeBacktickLine",
			text:  "````\nabc\n```\ndef\n````",
			limit: 20,
			want: []string{
				"````\nabc\n```\n````",
				"````\ndef\n````",
			},
		},
		{
			name:  "NoEmptyBlockAfterOpeningFence",
			text:  "```go\n" + strings.Repeat("x", 100) + "\n```\n",
			limit: 20,
			want:  slices.Repeat([]string{"```go\n" + strings.Repeat("x", 10) + "\n```"}, 10),
		},
		{
			name:  "SplitBeforeOpeningFence",
			text:  "Intro.\n```go\n" + strings.Repeat("x", 30) + "\n```",
			limit: 20,
			want:  append([]string{"Intro."}, slices.Repeat([]string{"```go\n" + strings.Repeat("x", 10) + "\n```"}, 3)...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := slackapi.SplitMarkdown(tt.text, tt.limit)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("SplitMarkdown(%q, %d)\n got: %q\nwant: %q", tt.text, tt.limit, got, tt.want)
			}
		})
	}
}

func TestSplitMarkdownDefaultLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			t.Parallel()

			atLimit := strings.Repeat("a", slackapi.MarkdownBlockLimit)
			if got := slackapi.SplitMarkdown(atLimit, limit); len(got) != 1 || got[0] != atLimit {
				t.Fatalf("text at MarkdownBlockLimit: got %d chunks, want 1 chunk equal to the input", len(got))
			}

			got := slackapi.SplitMarkdown(atLimit+"b", limit)
			if len(got) != 2 || got[0] != atLimit || got[1] != "b" {
				t.Fatalf("text over MarkdownBlockLimit: got %d chunks, want the input split after MarkdownBlockLimit runes", len(got))
			}
		})
	}
}

func TestSplitMarkdownProperties(t *testing.T) {
	t.Parallel()

	paragraph := "The quick brown fox jumps over the lazy dog. 😀 你好世界。"
	texts := []string{
		strings.Repeat(paragraph+"\n\n", 20),
		strings.Repeat(paragraph+"\n", 20),
		strings.Repeat("word ", 100),
		strings.Repeat("x", 500),
		strings.Repeat("😀", 300),
		strings.Repeat("你好世界", 100),
		"Intro.\n\n```go\n" + strings.Repeat("fmt.Println(\"hello, world\")\n", 30) + "```\n\nOutro.",
		"```\n" + strings.Repeat("code line\n\n", 30) + "```\n" + strings.Repeat("text ", 40),
		strings.Repeat("a paragraph of prose.\n\n```python\nprint(1)\nprint(2)\n```\n\n", 10),
		"```sh\n" + strings.Repeat("echo 😀 你好\n", 40) + "```",
		"````md\n" + strings.Repeat("```go\nfmt.Println(1)\n```\n", 10) + "````\n\nafter",
		"```\n" + strings.Repeat("```go\ninner line\n", 10) + "```",
		"```go\n" + strings.Repeat("x", 100) + "\n```\n",
		"Intro.\n```go\n" + strings.Repeat("x", 300) + "\n```",
	}
	for i, text := range texts {
		for limit := 1; limit <= 120; limit++ {
			t.Run(fmt.Sprintf("text%d/limit%d", i, limit), func(t *testing.T) {
				t.Parallel()
				chunks := slackapi.SplitMarkdown(text, limit)
				if len(chunks) == 0 {
					t.Fatal("got no chunks")
				}
				for j, chunk := range chunks {
					if chunk == "" {
						t.Fatalf("chunk %d is empty", j)
					}
					if !utf8.ValidString(chunk) {
						t.Fatalf("chunk %d is not valid UTF-8: %q", j, chunk)
					}
					if n := utf8.RuneCountInString(chunk); n > limit {
						t.Fatalf("chunk %d has %d runes, limit %d: %q", j, n, limit, chunk)
					}
				}

				repair := limit >= 2*formatsLongestFenceLine(text)+6
				if !repair {
					if got, want := formatsStripSpace(strings.Join(chunks, "")), formatsStripSpace(text); got != want {
						t.Fatalf("content changed without fence repair\n got: %q\nwant: %q", got, want)
					}
					return
				}
				if got, want := formatsStripFencesAndSpace(chunks...), formatsStripFencesAndSpace(text); got != want {
					t.Fatalf("content changed\n got: %q\nwant: %q", got, want)
				}
				for j, chunk := range chunks {
					want := j == len(chunks)-1 && formatsFenceOpenAtEnd(text)
					if got := formatsFenceOpenAtEnd(chunk); got != want {
						t.Fatalf("chunk %d ends inside a fenced code block = %t, want %t: %q", j, got, want, chunk)
					}
				}
			})
		}
	}
}

func formatsStripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func formatsStripFencesAndSpace(texts ...string) string {
	var b strings.Builder
	for _, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if !strings.HasPrefix(line, "```") {
				b.WriteString(formatsStripSpace(line))
			}
		}
	}
	return b.String()
}

// formatsFenceOpenAtEnd reports whether a fenced code block is open at the end
// of text. A line of three or more backticks opens a block, and a line of at
// least as many backticks followed only by whitespace closes it.
func formatsFenceOpenAtEnd(text string) bool {
	open := 0
	for line := range strings.SplitSeq(text, "\n") {
		n := len(line) - len(strings.TrimLeft(line, "`"))
		switch {
		case open == 0 && n >= 3:
			open = n
		case open > 0 && n >= open && strings.TrimSpace(line[n:]) == "":
			open = 0
		}
	}
	return open > 0
}

func formatsLongestFenceLine(text string) int {
	longest := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "```") {
			longest = max(longest, utf8.RuneCountInString(line))
		}
	}
	return longest
}
