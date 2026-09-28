package slackapitest

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// FileRequest is one request that the file server received.
type FileRequest struct {
	// Method is the HTTP method, for example "POST".
	Method string
	// Path is the URL path, for example "/upload/F1".
	Path string
	// Header is a copy of the request header.
	Header http.Header
	// Body is the raw request body.
	Body []byte
}

// FileOrigin returns the scheme://host origin of the file server for
// slackapi.Options.FileOrigins. It differs from the origin of URL.
func (s *Server) FileOrigin() string {
	return s.files.URL
}

// UploadURL returns an upload URL for fileID on the file server, for the
// upload_url field of a files.getUploadURLExternal response. A request to it
// returns 200 with the body "OK - <length>", like Slack. FileRequests returns
// the requests to it.
func (s *Server) UploadURL(fileID string) string {
	return s.HandleFile("/upload/"+fileID, func(req FileRequest) any {
		return Response{
			Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			Body:   fmt.Appendf(nil, "OK - %d", len(req.Body)),
		}
	})
}

// ServeFile makes every later request to path on the file server return body
// with the Content-Type contentType, and returns the URL of path. An empty
// contentType sends application/octet-stream.
func (s *Server) ServeFile(path, contentType string, body []byte) string {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp := Response{Header: http.Header{"Content-Type": {contentType}}, Body: body}
	return s.HandleFile(path, func(FileRequest) any { return resp })
}

// HandleFile makes every later request to path on the file server return the
// result of fn, with the same rules as Respond, and returns the URL of path. A
// path without a leading slash gets one. fn runs on the server goroutine, so it
// must not call t.Fatal.
func (s *Server) HandleFile(path string, fn func(FileRequest) any) string {
	path = "/" + strings.TrimPrefix(path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileHandlers[path] = fn
	return s.files.URL + (&url.URL{Path: path}).EscapedPath()
}

// FileRequests returns the requests to target on the file server in the order
// the Server received them. target is a URL that UploadURL, ServeFile, or
// HandleFile returned, or its path.
func (s *Server) FileRequests(target string) []FileRequest {
	u, err := url.Parse(target)
	if err != nil {
		s.t.Errorf("slackapitest: parse file URL %q: %v", target, err)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.fileRequests[u.Path])
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("slackapitest: read file request body for %s: %v", r.URL.Path, err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	req := FileRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}

	s.mu.Lock()
	s.fileRequests[req.Path] = append(s.fileRequests[req.Path], req)
	fn := s.fileHandlers[req.Path]
	s.mu.Unlock()

	if fn == nil {
		s.t.Errorf("slackapitest: no file at path %q", req.Path)
		http.NotFound(w, r)
		return
	}
	s.writeAny(w, req.Path, fn(req))
}
