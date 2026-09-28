// Package slackapitest provides a fake Slack Web API server and a request
// signer for tests of code that uses package slackapi.
//
// A test creates a Server, points slackapi.Options at Server.URL and
// Server.Client, sets responses with Respond or Handle, and inspects the
// requests with Calls.
//
// The Server also serves Slack files on a second origin. A test adds
// Server.FileOrigin to slackapi.Options.FileOrigins, returns Server.UploadURL
// from files.getUploadURLExternal, serves downloads with ServeFile or
// HandleFile, and inspects the requests with FileRequests.
package slackapitest
