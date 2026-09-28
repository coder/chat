package slackapitest

import (
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

// Server is a fake Slack Web API on an httptest.Server. It is safe for
// concurrent use.
type Server struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]func(Call) any
	calls    map[string][]Call
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
		t:        t,
		handlers: map[string]func(Call) any{},
		calls:    map[string][]Call{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.srv.Close)
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

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
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

	if fn == nil {
		s.t.Errorf("slackapitest: no response for method %q", method)
		s.write(w, method, Response{Body: json.RawMessage(unknownMethodBody)})
		return
	}
	switch resp := fn(call).(type) {
	case Response:
		s.write(w, method, resp)
	case *Response:
		s.write(w, method, *resp)
	default:
		s.write(w, method, Response{Body: resp})
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
