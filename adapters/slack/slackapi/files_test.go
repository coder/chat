package slackapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// filesNewClient returns a client for srv with token xoxb-files and opts
// applied. Empty opts.FileOrigins allows only srv.FileOrigin.
func filesNewClient(srv *slackapitest.Server, opts slackapi.Options) *slackapi.Client {
	opts.BaseURL = srv.URL()
	opts.HTTPClient = srv.Client()
	if opts.Token == "" {
		opts.Token = "xoxb-files"
	}
	if len(opts.FileOrigins) == 0 {
		opts.FileOrigins = []string{srv.FileOrigin()}
	}
	return slackapi.New(opts)
}

// filesRedirect returns a Response that redirects to location.
func filesRedirect(location string) slackapitest.Response {
	return slackapitest.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {location}}}
}

func TestGetUploadURLExternal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      slackapi.GetUploadURLExternalRequest
		wantForm url.Values
	}{
		{
			name:     "all fields",
			req:      slackapi.GetUploadURLExternalRequest{Filename: "a b.txt", Length: 11, AltText: "alt", SnippetType: "text"},
			wantForm: url.Values{"filename": {"a b.txt"}, "length": {"11"}, "alt_txt": {"alt"}, "snippet_type": {"text"}},
		},
		{
			name:     "required fields",
			req:      slackapi.GetUploadURLExternalRequest{Filename: "a.txt", Length: 3},
			wantForm: url.Values{"filename": {"a.txt"}, "length": {"3"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("files.getUploadURLExternal", map[string]any{"ok": true, "upload_url": "https://files.example/u", "file_id": "F1"})
			client := filesNewClient(srv, slackapi.Options{})

			got, err := client.GetUploadURLExternal(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("GetUploadURLExternal: %v", err)
			}
			if want := (slackapi.UploadURL{UploadURL: "https://files.example/u", FileID: "F1"}); *got != want {
				t.Fatalf("response = %+v, want %+v", *got, want)
			}
			calls := srv.Calls("files.getUploadURLExternal")
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			call := calls[0]
			if call.Form.Encode() != tt.wantForm.Encode() {
				t.Fatalf("Form = %v, want %v", call.Form, tt.wantForm)
			}
			if got := call.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := call.Header.Get("Authorization"); got != "Bearer xoxb-files" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}

	t.Run("api error", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("files.getUploadURLExternal", map[string]any{"ok": false, "error": "invalid_auth"})
		client := filesNewClient(srv, slackapi.Options{})

		got, err := client.GetUploadURLExternal(t.Context(), slackapi.GetUploadURLExternalRequest{Filename: "a.txt", Length: 1})
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok || got != nil {
			t.Fatalf("response = %v, err = %v, want nil and *slackapi.APIError", got, err)
		}
		if apiErr.Method != "files.getUploadURLExternal" || apiErr.Code != "invalid_auth" {
			t.Fatalf("APIError = %+v", apiErr)
		}
	})
}

func TestUploadToURL(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.UploadURL("F1")

		if err := client.UploadToURL(t.Context(), target, []byte("content")); err != nil {
			t.Fatalf("UploadToURL: %v", err)
		}
		reqs := srv.FileRequests(target)
		if len(reqs) != 1 {
			t.Fatalf("file requests = %d, want 1", len(reqs))
		}
		req := reqs[0]
		if req.Method != http.MethodPost || string(req.Body) != "content" {
			t.Fatalf("request = %s %q, want POST %q", req.Method, req.Body, "content")
		}
		if got, ok := req.Header["Authorization"]; ok {
			t.Fatalf("Authorization = %q, want none", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("Content-Type = %q", got)
		}
	})

	t.Run("origin not allowed", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{FileOrigins: []string{srv.URL()}})
		target := srv.UploadURL("F1")

		for _, uploadURL := range []string{target, "/upload/F1"} {
			err := client.UploadToURL(t.Context(), uploadURL, []byte("content"))
			if err == nil || !strings.Contains(err.Error(), "not in FileOrigins") {
				t.Fatalf("UploadToURL(%q) err = %v, want an origin error", uploadURL, err)
			}
		}
		if got := len(srv.FileRequests(target)); got != 0 {
			t.Fatalf("file requests = %d, want 0", got)
		}
	})

	t.Run("status error", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.HandleFile("/upload/F1", func(slackapitest.FileRequest) any {
			return slackapitest.Response{StatusCode: http.StatusInternalServerError, Body: []byte("boom")}
		})

		err := client.UploadToURL(t.Context(), target, []byte("content"))
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok {
			t.Fatalf("err = %v, want *slackapi.APIError", err)
		}
		if apiErr.Method != "upload_url" || apiErr.StatusCode != http.StatusInternalServerError || string(apiErr.Raw) != "boom" {
			t.Fatalf("APIError = %+v", apiErr)
		}
	})
}

func TestCompleteUploadExternal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      slackapi.CompleteUploadExternalRequest
		wantBody string
	}{
		{
			name: "all fields",
			req: slackapi.CompleteUploadExternalRequest{
				Files:          []slackapi.FileSummary{{ID: "F1", Title: "Report"}, {ID: "F2"}},
				ChannelID:      "C1",
				ThreadTS:       "111.222",
				InitialComment: "here",
			},
			wantBody: `{"files":[{"id":"F1","title":"Report"},{"id":"F2"}],"channel_id":"C1","thread_ts":"111.222","initial_comment":"here"}`,
		},
		{
			name:     "files only",
			req:      slackapi.CompleteUploadExternalRequest{Files: []slackapi.FileSummary{{ID: "F1"}}},
			wantBody: `{"files":[{"id":"F1"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("files.completeUploadExternal", json.RawMessage(`{"ok":true,"files":[{"id":"F1","name":"a.txt","title":"Report","size":3}]}`))
			client := filesNewClient(srv, slackapi.Options{})

			got, err := client.CompleteUploadExternal(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("CompleteUploadExternal: %v", err)
			}
			want := []slackapi.File{{ID: "F1", Name: "a.txt", Title: "Report", Size: 3}}
			if !slices.Equal(got.Files, want) {
				t.Fatalf("files = %+v, want %+v", got.Files, want)
			}
			calls := srv.Calls("files.completeUploadExternal")
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			if got := string(calls[0].Body); got != tt.wantBody {
				t.Fatalf("body = %s, want %s", got, tt.wantBody)
			}
			if got := calls[0].Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := calls[0].Header.Get("Authorization"); got != "Bearer xoxb-files" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}

	t.Run("api error", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("files.completeUploadExternal", map[string]any{"ok": false, "error": "file_not_found"})
		client := filesNewClient(srv, slackapi.Options{})

		got, err := client.CompleteUploadExternal(t.Context(), slackapi.CompleteUploadExternalRequest{Files: []slackapi.FileSummary{{ID: "F1"}}})
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok || got != nil {
			t.Fatalf("response = %v, err = %v, want nil and *slackapi.APIError", got, err)
		}
		if apiErr.Method != "files.completeUploadExternal" || apiErr.Code != "file_not_found" {
			t.Fatalf("APIError = %+v", apiErr)
		}
	})
}

func TestUploadFile(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		uploadURL := srv.UploadURL("F1")
		srv.Respond("files.getUploadURLExternal", map[string]any{"ok": true, "upload_url": uploadURL, "file_id": "F1"})
		srv.Respond("files.completeUploadExternal", json.RawMessage(`{"ok":true,"files":[{"id":"F1","name":"a.txt","title":"Report"},{"id":"F9"}]}`))
		client := filesNewClient(srv, slackapi.Options{})

		file, err := client.UploadFile(t.Context(), slackapi.UploadFileRequest{
			Filename:       "a.txt",
			Title:          "Report",
			Content:        []byte("hello world"),
			AltText:        "alt",
			SnippetType:    "text",
			ChannelID:      "C1",
			ThreadTS:       "111.222",
			InitialComment: "here",
		})
		if err != nil {
			t.Fatalf("UploadFile: %v", err)
		}
		if want := (slackapi.File{ID: "F1", Name: "a.txt", Title: "Report"}); *file != want {
			t.Fatalf("file = %+v, want %+v", *file, want)
		}

		gets := srv.Calls("files.getUploadURLExternal")
		if len(gets) != 1 {
			t.Fatalf("files.getUploadURLExternal calls = %d, want 1", len(gets))
		}
		wantForm := url.Values{"filename": {"a.txt"}, "length": {"11"}, "alt_txt": {"alt"}, "snippet_type": {"text"}}
		if gets[0].Form.Encode() != wantForm.Encode() {
			t.Fatalf("Form = %v, want %v", gets[0].Form, wantForm)
		}
		uploads := srv.FileRequests(uploadURL)
		if len(uploads) != 1 || string(uploads[0].Body) != "hello world" {
			t.Fatalf("uploads = %+v, want one with the content", uploads)
		}
		completes := srv.Calls("files.completeUploadExternal")
		if len(completes) != 1 {
			t.Fatalf("files.completeUploadExternal calls = %d, want 1", len(completes))
		}
		wantBody := `{"files":[{"id":"F1","title":"Report"}],"channel_id":"C1","thread_ts":"111.222","initial_comment":"here"}`
		if got := string(completes[0].Body); got != wantBody {
			t.Fatalf("complete body = %s, want %s", got, wantBody)
		}
	})

	tests := []struct {
		name          string
		getResp       any
		uploadResp    any
		completeResp  any
		wantPrefix    string
		wantAPIMethod string
		wantRateLimit bool
		wantUploads   int
		wantCompletes int
	}{
		{
			name:          "get upload URL",
			getResp:       map[string]any{"ok": false, "error": "invalid_auth"},
			wantPrefix:    "slack: upload file: get upload URL: ",
			wantAPIMethod: "files.getUploadURLExternal",
		},
		{
			name:          "upload content",
			uploadResp:    slackapitest.Response{StatusCode: http.StatusInternalServerError},
			wantPrefix:    "slack: upload file: upload content: ",
			wantAPIMethod: "upload_url",
			wantUploads:   1,
		},
		{
			name:          "upload content rate limited",
			uploadResp:    slackapitest.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}}},
			wantPrefix:    "slack: upload file: upload content: ",
			wantRateLimit: true,
			wantUploads:   1,
		},
		{
			name:          "complete upload",
			completeResp:  map[string]any{"ok": false, "error": "file_not_found"},
			wantPrefix:    "slack: upload file: complete upload: ",
			wantAPIMethod: "files.completeUploadExternal",
			wantUploads:   1,
			wantCompletes: 1,
		},
		{
			name:          "no files",
			completeResp:  map[string]any{"ok": true, "files": []any{}},
			wantPrefix:    "slack: upload file: complete upload: response has no files",
			wantUploads:   1,
			wantCompletes: 1,
		},
	}
	for _, tt := range tests {
		t.Run("fails at "+tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			uploadURL := srv.UploadURL("F1")
			if tt.uploadResp != nil {
				uploadURL = srv.HandleFile("/upload/F1", func(slackapitest.FileRequest) any { return tt.uploadResp })
			}
			getResp := tt.getResp
			if getResp == nil {
				getResp = map[string]any{"ok": true, "upload_url": uploadURL, "file_id": "F1"}
			}
			completeResp := tt.completeResp
			if completeResp == nil {
				completeResp = map[string]any{"ok": true, "files": []any{map[string]any{"id": "F1"}}}
			}
			srv.Respond("files.getUploadURLExternal", getResp)
			srv.Respond("files.completeUploadExternal", completeResp)
			client := filesNewClient(srv, slackapi.Options{RetryPolicy: slackapi.RetryPolicy{MaxAttempts: 1}})

			file, err := client.UploadFile(t.Context(), slackapi.UploadFileRequest{Filename: "a.txt", Content: []byte("abc")})
			if err == nil || file != nil {
				t.Fatalf("file = %v, err = %v, want nil and an error", file, err)
			}
			if !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Fatalf("err = %q, want prefix %q", err.Error(), tt.wantPrefix)
			}
			apiErr, isAPIErr := errors.AsType[*slackapi.APIError](err)
			switch {
			case tt.wantAPIMethod != "" && (!isAPIErr || apiErr.Method != tt.wantAPIMethod):
				t.Fatalf("err = %v, want *slackapi.APIError for %s", err, tt.wantAPIMethod)
			case tt.wantAPIMethod == "" && isAPIErr:
				t.Fatalf("err = %v, want no *slackapi.APIError", err)
			}
			if _, ok := errors.AsType[*slackapi.RateLimited](err); ok != tt.wantRateLimit {
				t.Fatalf("err = %v, *slackapi.RateLimited = %t, want %t", err, ok, tt.wantRateLimit)
			}
			if got := len(srv.FileRequests(uploadURL)); got != tt.wantUploads {
				t.Fatalf("uploads = %d, want %d", got, tt.wantUploads)
			}
			if got := len(srv.Calls("files.completeUploadExternal")); got != tt.wantCompletes {
				t.Fatalf("files.completeUploadExternal calls = %d, want %d", got, tt.wantCompletes)
			}
		})
	}
}

func TestDownloadFile(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		obs := &coreObserver{}
		client := filesNewClient(srv, slackapi.Options{Observer: obs})
		target := srv.ServeFile("/files-pri/T1-F1/download/a.txt", "text/plain", []byte("hello"))

		var buf bytes.Buffer
		n, err := client.DownloadFile(t.Context(), target, &buf, 1024)
		if err != nil {
			t.Fatalf("DownloadFile: %v", err)
		}
		if n != 5 || buf.String() != "hello" {
			t.Fatalf("n = %d, body = %q, want 5 %q", n, buf.String(), "hello")
		}
		reqs := srv.FileRequests(target)
		if len(reqs) != 1 || reqs[0].Method != http.MethodGet {
			t.Fatalf("file requests = %+v, want one GET", reqs)
		}
		if got := reqs[0].Header.Get("Authorization"); got != "Bearer xoxb-files" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := obs.count(chat.ObsAdapterCall); got != 1 {
			t.Fatalf("ObsAdapterCall = %d, want 1", got)
		}
	})

	t.Run("no token", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{}).WithToken("")
		target := srv.ServeFile("/a.txt", "text/plain", []byte("hello"))

		if _, err := client.DownloadFile(t.Context(), target, &bytes.Buffer{}, 1024); err != nil {
			t.Fatalf("DownloadFile: %v", err)
		}
		reqs := srv.FileRequests(target)
		if len(reqs) != 1 {
			t.Fatalf("file requests = %d, want 1", len(reqs))
		}
		if got, ok := reqs[0].Header["Authorization"]; ok {
			t.Fatalf("Authorization = %q, want none", got)
		}
	})

	t.Run("size limit", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			body     string
			maxBytes int64
			wantErr  bool
		}{
			{name: "under", body: "1234", maxBytes: 5},
			{name: "exact", body: "12345", maxBytes: 5},
			{name: "over", body: "123456", maxBytes: 5, wantErr: true},
			{name: "over by a lot", body: strings.Repeat("x", 64<<10), maxBytes: 5, wantErr: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				srv := slackapitest.NewServer(t)
				client := filesNewClient(srv, slackapi.Options{})
				target := srv.ServeFile("/a.bin", "", []byte(tt.body))

				var buf bytes.Buffer
				n, err := client.DownloadFile(t.Context(), target, &buf, tt.maxBytes)
				if tt.wantErr {
					if !errors.Is(err, slackapi.ErrFileTooLarge) {
						t.Fatalf("err = %v, want ErrFileTooLarge", err)
					}
					if n != tt.maxBytes || int64(buf.Len()) != tt.maxBytes {
						t.Fatalf("n = %d, written = %d, want %d", n, buf.Len(), tt.maxBytes)
					}
					return
				}
				if err != nil {
					t.Fatalf("DownloadFile: %v", err)
				}
				if n != int64(len(tt.body)) || buf.String() != tt.body {
					t.Fatalf("n = %d, body = %q, want %q", n, buf.String(), tt.body)
				}
			})
		}
	})

	t.Run("invalid maxBytes", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.ServeFile("/a.txt", "text/plain", []byte("hello"))

		for _, maxBytes := range []int64{0, -1} {
			if _, err := client.DownloadFile(t.Context(), target, &bytes.Buffer{}, maxBytes); err == nil || !strings.Contains(err.Error(), "maxBytes must be positive") {
				t.Fatalf("DownloadFile(maxBytes %d) err = %v, want an argument error", maxBytes, err)
			}
		}
		if got := len(srv.FileRequests(target)); got != 0 {
			t.Fatalf("file requests = %d, want 0", got)
		}
	})

	t.Run("origin not allowed", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{FileOrigins: []string{srv.URL()}})
		target := srv.ServeFile("/a.txt", "text/plain", []byte("hello"))

		for _, fileURL := range []string{target, "/a.txt", "http://[::1"} {
			n, err := client.DownloadFile(t.Context(), fileURL, &bytes.Buffer{}, 1024)
			if err == nil || n != 0 {
				t.Fatalf("DownloadFile(%q) = %d, %v, want an error", fileURL, n, err)
			}
		}
		_, err := client.DownloadFile(t.Context(), target, &bytes.Buffer{}, 1024)
		if !strings.Contains(err.Error(), "not in FileOrigins") {
			t.Fatalf("err = %v, want an origin error", err)
		}
		if got := len(srv.FileRequests(target)); got != 0 {
			t.Fatalf("file requests = %d, want 0", got)
		}
	})

	t.Run("redirect to allowed origin", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("files/F1", slackapitest.Response{Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("moved")})
		client := filesNewClient(srv, slackapi.Options{FileOrigins: []string{srv.FileOrigin(), srv.URL()}})
		target := srv.HandleFile("/redirect", func(slackapitest.FileRequest) any { return filesRedirect(srv.URL() + "/files/F1") })

		var buf bytes.Buffer
		n, err := client.DownloadFile(t.Context(), target, &buf, 1024)
		if err != nil {
			t.Fatalf("DownloadFile: %v", err)
		}
		if n != 5 || buf.String() != "moved" {
			t.Fatalf("n = %d, body = %q, want 5 %q", n, buf.String(), "moved")
		}
		if got := len(srv.Calls("files/F1")); got != 1 {
			t.Fatalf("redirect target calls = %d, want 1", got)
		}
	})

	t.Run("redirect to other origin", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		srv.Respond("files/F1", slackapitest.Response{Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("moved")})
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.HandleFile("/redirect", func(slackapitest.FileRequest) any { return filesRedirect(srv.URL() + "/files/F1") })

		var buf bytes.Buffer
		n, err := client.DownloadFile(t.Context(), target, &buf, 1024)
		if err == nil || !strings.Contains(err.Error(), "redirect origin") || n != 0 || buf.Len() != 0 {
			t.Fatalf("n = %d, written = %d, err = %v, want a redirect origin error", n, buf.Len(), err)
		}
		if got := len(srv.Calls("files/F1")); got != 0 {
			t.Fatalf("redirect target calls = %d, want 0", got)
		}
		if srv.Client().CheckRedirect != nil {
			t.Fatal("DownloadFile set CheckRedirect on the shared HTTP client")
		}
	})

	t.Run("redirect limit", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.HandleFile("/loop", func(slackapitest.FileRequest) any { return filesRedirect(srv.FileOrigin() + "/loop") })

		_, err := client.DownloadFile(t.Context(), target, &bytes.Buffer{}, 1024)
		if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
			t.Fatalf("err = %v, want the redirect limit error", err)
		}
		if got := len(srv.FileRequests(target)); got != 10 {
			t.Fatalf("file requests = %d, want 10", got)
		}
	})

	t.Run("redirect policy of the HTTP client", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		httpClient := *srv.Client()
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client := slackapi.New(slackapi.Options{Token: "xoxb-files", HTTPClient: &httpClient, FileOrigins: []string{srv.FileOrigin()}})
		target := srv.HandleFile("/redirect", func(slackapitest.FileRequest) any { return filesRedirect(srv.FileOrigin() + "/a.txt") })

		_, err := client.DownloadFile(t.Context(), target, &bytes.Buffer{}, 1024)
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok || apiErr.StatusCode != http.StatusFound {
			t.Fatalf("err = %v, want *slackapi.APIError with status 302", err)
		}
	})

	t.Run("html page", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.ServeFile("/login", "text/html; charset=utf-8", []byte("<html>sign in</html>"))

		var buf bytes.Buffer
		n, err := client.DownloadFile(t.Context(), target, &buf, 1024)
		if err == nil || !strings.Contains(err.Error(), "HTML page") || n != 0 || buf.Len() != 0 {
			t.Fatalf("n = %d, written = %d, err = %v, want an HTML error", n, buf.Len(), err)
		}
	})

	t.Run("status error", func(t *testing.T) {
		t.Parallel()

		srv := slackapitest.NewServer(t)
		client := filesNewClient(srv, slackapi.Options{})
		target := srv.HandleFile("/gone", func(slackapitest.FileRequest) any {
			return slackapitest.Response{StatusCode: http.StatusNotFound, Body: []byte("not found")}
		})

		var buf bytes.Buffer
		n, err := client.DownloadFile(t.Context(), target, &buf, 1024)
		apiErr, ok := errors.AsType[*slackapi.APIError](err)
		if !ok || n != 0 || buf.Len() != 0 {
			t.Fatalf("n = %d, written = %d, err = %v, want *slackapi.APIError", n, buf.Len(), err)
		}
		if apiErr.Method != "files.download" || apiErr.StatusCode != http.StatusNotFound || string(apiErr.Raw) != "not found" {
			t.Fatalf("APIError = %+v", apiErr)
		}
		if got, want := err.Error(), "slack: files.download status 404"; got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
		if got := len(srv.FileRequests(target)); got != 1 {
			t.Fatalf("file requests = %d, want 1 (downloads are not retried)", got)
		}
	})
}
