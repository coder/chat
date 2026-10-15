package slackapitest

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

const apiPrefix = "/api/"

// unknownMethodBody is the response for a method with no Respond or Handle.
const unknownMethodBody = `{"ok":false,"error":"unknown_method"}`

// Server is a fake Slack Web API on an httptest.Server, with a second
// httptest.Server that serves file uploads and downloads on its own origin. It
// is safe for concurrent use.
type Server struct {
	t     testing.TB
	srv   *httptest.Server
	files *httptest.Server

	changes notifier

	mu           sync.Mutex
	handlers     map[string]func(Call) any
	calls        map[string][]Call
	fileHandlers map[string]func(FileRequest) any
	fileRequests map[string][]FileRequest
	socketMode   *SocketMode
}

// Call is one request that the Server received.
type Call struct {
	// Method is the Web API method, for example "chat.postMessage".
	Method string
	// Header is a copy of the request header.
	Header http.Header
	// Body is the raw request body.
	Body json.RawMessage
	// Form is the parsed body when the request is form encoded, else nil.
	Form url.Values
}

// Response is a full HTTP response. A Handle function returns it to control the
// status code and headers, for example a 429 with Retry-After.
type Response struct {
	// StatusCode is the HTTP status. Zero sends 200.
	StatusCode int
	// Header holds extra response headers.
	Header http.Header
	// Body is the response body. A json.RawMessage or []byte is sent as is, nil
	// sends no body, and any other value is sent as JSON.
	Body any
}

// NewServer starts a Server and closes it with t.Cleanup.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		t:            t,
		handlers:     map[string]func(Call) any{},
		calls:        map[string][]Call{},
		fileHandlers: map[string]func(FileRequest) any{},
		fileRequests: map[string][]FileRequest{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.srv.Close)
	s.files = httptest.NewServer(http.HandlerFunc(s.serveFile))
	t.Cleanup(s.files.Close)
	return s
}

// URL returns the Web API base URL for slackapi.Options.BaseURL.
func (s *Server) URL() string {
	return s.srv.URL + strings.TrimSuffix(apiPrefix, "/")
}

// Client returns an HTTP client for slackapi.Options.HTTPClient.
func (s *Server) Client() *http.Client {
	return s.srv.Client()
}

// Respond makes every later call to method return resp. A json.RawMessage or
// []byte is sent as is, a Response sets the full HTTP response, and any other
// value is sent as JSON.
func (s *Server) Respond(method string, resp any) {
	s.Handle(method, func(Call) any { return resp })
}

// Handle makes every later call to method return the result of fn, with the same
// rules as Respond. fn runs on the server goroutine, so it must not call
// t.Fatal.
func (s *Server) Handle(method string, fn func(Call) any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

// Calls returns the requests to method in the order the Server received them.
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls[method])
}

// Wait waits until cond returns true. It calls cond at the start and again
// after each change in the Server: a Web API call, a file request, or a Socket
// Mode change. It returns nil when cond returns true, and the error of ctx when
// ctx is done first. cond must not block.
func (s *Server) Wait(ctx context.Context, cond func() bool) error {
	for {
		changed := s.changes.next()
		if cond() {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == socketModePath {
		s.mu.Lock()
		m := s.socketMode
		s.mu.Unlock()
		if m != nil {
			m.serve(w, r)
			return
		}
	}
	method, ok := strings.CutPrefix(r.URL.Path, apiPrefix)
	if !ok || method == "" {
		s.t.Errorf("slackapitest: unexpected request path %q", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("slackapitest: read %s request body: %v", method, err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	call := Call{Method: method, Header: r.Header.Clone(), Body: json.RawMessage(body)}
	if isForm(r.Header.Get("Content-Type")) {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			s.t.Errorf("slackapitest: parse %s form body: %v", method, err)
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		call.Form = form
	}

	s.mu.Lock()
	s.calls[method] = append(s.calls[method], call)
	fn := s.handlers[method]
	s.mu.Unlock()
	s.changes.notify()

	if fn == nil {
		s.t.Errorf("slackapitest: no response for method %q", method)
		s.write(w, method, Response{Body: json.RawMessage(unknownMethodBody)})
		return
	}
	s.writeAny(w, method, fn(call))
}

// writeAny writes resp with the rules of Respond.
func (s *Server) writeAny(w http.ResponseWriter, name string, resp any) {
	switch resp := resp.(type) {
	case Response:
		s.write(w, name, resp)
	case *Response:
		s.write(w, name, *resp)
	default:
		s.write(w, name, Response{Body: resp})
	}
}

func (s *Server) write(w http.ResponseWriter, method string, resp Response) {
	var body []byte
	switch b := resp.Body.(type) {
	case nil:
	case json.RawMessage:
		body = b
	case []byte:
		body = b
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			s.t.Errorf("slackapitest: encode %s response: %v", method, err)
			http.Error(w, "encode response", http.StatusInternalServerError)
			return
		}
		body = encoded
	}
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if len(body) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(statusOrOK(resp.StatusCode))
	_, _ = w.Write(body)
}

func statusOrOK(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

func isForm(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}

// notifier wakes the goroutines in Server.Wait when the Server changes.
type notifier struct {
	mu      sync.Mutex
	changed chan struct{}
}

// next returns a channel that is closed at the next change.
func (n *notifier) next() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.changed == nil {
		n.changed = make(chan struct{})
	}
	return n.changed
}

func (n *notifier) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.changed != nil {
		close(n.changed)
		n.changed = nil
	}
}
