package slackapitest_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// filesDo sends a request to target with srv.Client and returns the response
// and its body.
func filesDo(t *testing.T, srv *slackapitest.Server, method, target string, header http.Header, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for key, values := range header {
		req.Header[key] = values
	}
	client := *srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, string(payload)
}

func TestFileOrigin(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	origin, err := url.Parse(srv.FileOrigin())
	if err != nil {
		t.Fatalf("parse FileOrigin: %v", err)
	}
	api, err := url.Parse(srv.URL())
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if origin.Scheme == "" || origin.Host == "" || origin.Path != "" {
		t.Fatalf("FileOrigin = %q, want scheme://host", srv.FileOrigin())
	}
	if origin.Host == api.Host {
		t.Fatalf("FileOrigin host = API host %q, want a separate origin", origin.Host)
	}
}

func TestUploadURL(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	target := srv.UploadURL("F1")
	if !strings.HasPrefix(target, srv.FileOrigin()+"/") {
		t.Fatalf("UploadURL = %q, want it under %q", target, srv.FileOrigin())
	}

	resp, body := filesDo(t, srv, http.MethodPost, target, http.Header{"Content-Type": {"application/octet-stream"}}, "hello")
	if resp.StatusCode != http.StatusOK || body != "OK - 5" {
		t.Fatalf("response = %d %q, want 200 %q", resp.StatusCode, body, "OK - 5")
	}

	reqs := srv.FileRequests(target)
	if len(reqs) != 1 {
		t.Fatalf("file requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost || req.Path != "/upload/F1" || string(req.Body) != "hello" {
		t.Fatalf("request = %+v", req)
	}
	if got := req.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := srv.FileRequests("/upload/F1"); len(got) != 1 {
		t.Fatalf("file requests by path = %d, want 1", len(got))
	}
	if got := srv.FileRequests(srv.UploadURL("F2")); len(got) != 0 {
		t.Fatalf("F2 requests = %d, want 0", len(got))
	}
}

func TestServeFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		want        string
	}{
		{name: "content type", contentType: "text/plain", want: "text/plain"},
		{name: "default", want: "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			target := srv.ServeFile("files-pri/T1-F1/report.txt", tt.contentType, []byte("data"))
			if want := srv.FileOrigin() + "/files-pri/T1-F1/report.txt"; target != want {
				t.Fatalf("ServeFile = %q, want %q", target, want)
			}

			resp, body := filesDo(t, srv, http.MethodGet, target, http.Header{"Authorization": {"Bearer xoxb-files"}}, "")
			if resp.StatusCode != http.StatusOK || body != "data" {
				t.Fatalf("response = %d %q", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.want {
				t.Fatalf("Content-Type = %q, want %q", got, tt.want)
			}
			reqs := srv.FileRequests(target)
			if len(reqs) != 1 || reqs[0].Method != http.MethodGet {
				t.Fatalf("file requests = %+v, want one GET", reqs)
			}
			if got := reqs[0].Header.Get("Authorization"); got != "Bearer xoxb-files" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}
}

func TestHandleFile(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	target := srv.HandleFile("/redirect", func(slackapitest.FileRequest) any {
		return slackapitest.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {srv.URL() + "/elsewhere"}},
		}
	})

	resp, _ := filesDo(t, srv, http.MethodGet, target, nil, "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != srv.URL()+"/elsewhere" {
		t.Fatalf("Location = %q", got)
	}
	if got := len(srv.FileRequests(target)); got != 1 {
		t.Fatalf("file requests = %d, want 1", got)
	}
}

func TestFileRequestsConcurrent(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	target := srv.UploadURL("F1")

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			filesDo(t, srv, http.MethodPost, target, nil, "x")
			_ = srv.FileRequests(target)
		})
	}
	wg.Wait()
	if got := len(srv.FileRequests(target)); got != n {
		t.Fatalf("file requests = %d, want %d", got, n)
	}
}

func TestUnknownFile(t *testing.T) {
	t.Parallel()

	tb := &coreRecordingTB{TB: t}
	srv := slackapitest.NewServer(tb)

	resp, _ := filesDo(t, srv, http.MethodGet, srv.FileOrigin()+"/missing", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := tb.errorCount(); got != 1 {
		t.Fatalf("Errorf calls = %d, want 1", got)
	}
	if got := len(srv.FileRequests("/missing")); got != 1 {
		t.Fatalf("file requests = %d, want 1 (unknown requests are recorded)", got)
	}
}
