package slackapi_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
)

func TestMessageDecodesRepliesMessage(t *testing.T) {
	t.Parallel()

	raw := `{
		"client_msg_id": "8d3b6c1e-2f4a-4c1b-9e7d-5a6b7c8d9e0f",
		"type": "message",
		"subtype": "file_share",
		"text": "here is the log, see line 12",
		"user": "U061F7AUR",
		"ts": "1700000300.000400",
		"thread_ts": "1700000000.000100",
		"parent_user_id": "U061F7AUR",
		"team": "T0001",
		"user_team": "T0001",
		"source_team": "T0002",
		"blocks": [{"type": "rich_text", "block_id": "x9Y8z", "elements": []}],
		"files": [{
			"id": "F0FILE1234",
			"created": 1700000300,
			"name": "build.log",
			"title": "build.log",
			"mimetype": "text/plain",
			"filetype": "text",
			"pretty_type": "Plain Text",
			"user": "U061F7AUR",
			"size": 2048,
			"mode": "snippet",
			"is_external": false,
			"url_private": "https://files.slack.com/files-pri/T0001-F0FILE1234/build.log",
			"url_private_download": "https://files.slack.com/files-pri/T0001-F0FILE1234/download/build.log"
		}],
		"edited": {"user": "U061F7AUR", "ts": "1700000350.000000"},
		"reactions": [{"name": "eyes", "users": ["U0BOT1234"], "count": 1}]
	}`
	var got slackapi.Message
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	want := slackapi.Message{
		Type:       "message",
		Subtype:    slackapi.SubtypeFileShare,
		User:       "U061F7AUR",
		Text:       "here is the log, see line 12",
		TS:         "1700000300.000400",
		ThreadTS:   "1700000000.000100",
		Team:       "T0001",
		UserTeam:   "T0001",
		SourceTeam: "T0002",
		Files: []slackapi.File{{
			ID:                 "F0FILE1234",
			Name:               "build.log",
			Title:              "build.log",
			MIMEType:           "text/plain",
			FileType:           "text",
			Size:               2048,
			URLPrivateDownload: "https://files.slack.com/files-pri/T0001-F0FILE1234/download/build.log",
		}},
		Edited: &slackapi.Edited{User: "U061F7AUR", TS: "1700000350.000000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message mismatch\ngot:  %+v\nwant: %+v", got, want)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	const wantJSON = `{"type":"message","subtype":"file_share","user":"U061F7AUR","text":"here is the log, see line 12","ts":"1700000300.000400","thread_ts":"1700000000.000100","team":"T0001","user_team":"T0001","source_team":"T0002","files":[{"id":"F0FILE1234","name":"build.log","title":"build.log","mimetype":"text/plain","filetype":"text","size":2048,"url_private_download":"https://files.slack.com/files-pri/T0001-F0FILE1234/download/build.log"}],"edited":{"user":"U061F7AUR","ts":"1700000350.000000"}}`
	if string(encoded) != wantJSON {
		t.Fatalf("marshal mismatch\ngot:  %s\nwant: %s", encoded, wantJSON)
	}
}

func TestMessageMarshalOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "zero message", value: slackapi.Message{}, want: `{}`},
		{name: "zero file", value: slackapi.File{}, want: `{}`},
		{name: "zero edited", value: slackapi.Edited{}, want: `{}`},
		{
			name:  "text only",
			value: slackapi.Message{Type: "message", User: "U061F7AUR", Text: "hi", TS: "1700000200.000300"},
			want:  `{"type":"message","user":"U061F7AUR","text":"hi","ts":"1700000200.000300"}`,
		},
		{
			name:  "empty files",
			value: slackapi.Message{TS: "1700000200.000300", Files: []slackapi.File{}},
			want:  `{"ts":"1700000200.000300"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
