// Package slackapitest provides a fake Slack Web API server and a request
// signer for tests of code that uses package slackapi.
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
// SignRequest sets the Slack signature headers of a request so that
// slackapi.VerifyRequest accepts it.
//
// A Server is safe for concurrent use. Handle and HandleFile functions run on
// the server goroutine, so they must not call t.Fatal.
package slackapitest
