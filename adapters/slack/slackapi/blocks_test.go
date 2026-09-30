package slackapi_test

import (
	"encoding/json"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func TestBlocksMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block slackapi.Block
		want  string
	}{
		{
			name:  "MarkdownBlock",
			block: slackapi.MarkdownBlock{Text: "**hi**"},
			want:  `{"type":"markdown","text":"**hi**"}`,
		},
		{
			name:  "MarkdownBlockWithBlockID",
			block: slackapi.MarkdownBlock{Text: "hi", BlockID: "b1"},
			want:  `{"type":"markdown","text":"hi","block_id":"b1"}`,
		},
		{
			name: "ActionsBlock",
			block: slackapi.ActionsBlock{Elements: []slackapi.ButtonElement{
				{Text: "Open", URL: "https://example.com/chat/1", ActionID: "open", Style: "primary"},
				{Text: "Stop"},
			}},
			want: `{"type":"actions","elements":[` +
				`{"type":"button","text":{"type":"plain_text","text":"Open"},"url":"https://example.com/chat/1","action_id":"open","style":"primary"},` +
				`{"type":"button","text":{"type":"plain_text","text":"Stop"}}` +
				`]}`,
		},
		{
			name:  "ActionsBlockWithoutElements",
			block: slackapi.ActionsBlock{},
			want:  `{"type":"actions","elements":[]}`,
		},
		{
			name: "ActionsBlockWithBlockID",
			block: slackapi.ActionsBlock{
				Elements: []slackapi.ButtonElement{{Text: "Stop", ActionID: "stop", Style: "danger"}},
				BlockID:  "actions",
			},
			want: `{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Stop"},"action_id":"stop","style":"danger"}],"block_id":"actions"}`,
		},
		{
			name:  "ImageBlock",
			block: slackapi.ImageBlock{ImageURL: "https://example.com/a.png", AltText: "a chart"},
			want:  `{"type":"image","image_url":"https://example.com/a.png","alt_text":"a chart"}`,
		},
		{
			name: "ImageBlockWithTitleAndBlockID",
			block: slackapi.ImageBlock{
				ImageURL: "https://example.com/a.png",
				AltText:  "a chart",
				Title:    "Usage",
				BlockID:  "img",
			},
			want: `{"type":"image","image_url":"https://example.com/a.png","alt_text":"a chart","title":{"type":"plain_text","text":"Usage"},"block_id":"img"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			formatsAssertJSON(t, tt.block, tt.want)
		})
	}
}

func TestBlockType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		block slackapi.Block
		want  string
	}{
		{slackapi.MarkdownBlock{}, "markdown"},
		{slackapi.ActionsBlock{}, "actions"},
		{slackapi.ImageBlock{}, "image"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.block.BlockType(); got != tt.want {
				t.Fatalf("BlockType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func formatsAssertJSON(t *testing.T, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != want {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
