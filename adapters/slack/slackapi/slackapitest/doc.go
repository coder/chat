// Package slackapitest provides a fake Slack Web API server and a request
// signer for tests of code that uses package slackapi.
//
// A test creates a Server, points slackapi.Options at Server.URL and
// Server.Client, sets responses with Respond or Handle, and inspects the
// requests with Calls.
package slackapitest
