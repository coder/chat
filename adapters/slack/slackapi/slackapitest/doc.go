// Package slackapitest provides a fake Slack Web API server with a fake Socket
// Mode, and a request signer, for tests of code that uses package slackapi.
//
// NewServer starts a Server and closes it when the test ends. A test points
// slackapi.Options.BaseURL at Server.URL and slackapi.Options.HTTPClient at
// Server.Client. Respond sets a fixed response for a Web API method, and Handle
// sets a function that computes the response from the Call. A response value is
// sent as JSON, except a json.RawMessage or []byte, which is sent as is, and a
// Response, which also sets the status code and headers, for example a 429 with
// Retry-After. Calls returns the requests to a method in the order the Server
// received them; Call.Form holds the parsed body of a form-encoded request. A
// request to a method with no response fails the test and gets the error code
// "unknown_method".
//
// The Server also serves Slack files on a second origin. A test adds
// Server.FileOrigin to slackapi.Options.FileOrigins, returns Server.UploadURL
// from files.getUploadURLExternal, serves downloads with ServeFile or
// HandleFile, and inspects the requests with FileRequests. A request to a file
// path with no handler fails the test and gets status 404.
//
// Server.SocketMode adds a fake Socket Mode to the Server. It makes
// apps.connections.open return a ws:// URL on the Server, and sends hello and
// websocket pings on each connection to that URL. Send sends an envelope to a
// random live connection and resends it until a client acks it, as Slack does.
// A test can send disconnect messages, stop pings, and drop connections. It can
// also inspect each connection and envelope: which connection got each envelope
// and sent each ack, the order of hellos and closes, and the close frame of each
// close.
//
// Server.Wait waits until a condition is true. It checks the condition again
// after each Web API call, file request, and Socket Mode change, and when a
// Handle or HandleFile function returns, so a test does not need to sleep. It
// does not see a change that a Handle function makes before it returns, so a
// test that waits for a call while its Handle function blocks checks Calls.
// SocketMode.WaitLive, SocketMode.WaitClosed, and SocketMode.WaitAcked wait for
// common conditions with a timeout.
//
// SignRequest sets the Slack signature headers of a request so that
// slackapi.VerifyRequest accepts it.
//
// A Server is safe for concurrent use. Handle and HandleFile functions run on
// the server goroutine, so they must not call t.Fatal.
//
// The package is experimental: its exported API may change before it is
// promoted to supported.
package slackapitest
