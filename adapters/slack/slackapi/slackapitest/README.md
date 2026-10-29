# slackapitest

## Overview

Package `slackapitest` (`github.com/coder/chat/adapters/slack/slackapi/slackapitest`) holds test fakes for code that uses the parent `slackapi` package. It has three parts:

1. A fake Slack Web API server and a fake Slack file server (`Server`).
2. A fake Socket Mode service on that server (`SocketMode`). It sends hello, pings, and envelopes over websockets, and it resends envelopes until a client acks them.
3. A request signer (`SignRequest`) that makes `slackapi.VerifyRequest` accept a request.

A test points `slackapi.Options.BaseURL` at `Server.URL` and `slackapi.Options.HTTPClient` at `Server.Client`. The package doc marks the API as experimental. Its exported API can change before promotion. [doc.go:slackapitest]

## Interface

### Web API fake

- `NewServer` starts a `Server` and closes it at test cleanup. [server.go:NewServer]
- `Server` is the fake Web API and file server. [server.go:Server]
- `Call` is one recorded Web API request (`Method`, `Header`, `Body`, `Form`). [server.go:Call]
- `Response` is a full HTTP response (`StatusCode`, `Header`, `Body`), for example a 429 with `Retry-After`. [server.go:Response]
- `Server.URL` returns the base URL for `slackapi.Options.BaseURL`. [server.go:Server.URL]
- `Server.Client` returns the HTTP client for `slackapi.Options.HTTPClient`. [server.go:Server.Client]
- `Server.Respond` sets a fixed response for a Web API method. [server.go:Server.Respond]
- `Server.Handle` sets a function that computes the response for each call to a method. [server.go:Server.Handle]
- `Server.Calls` returns the recorded calls to a method, in arrival order. [server.go:Server.Calls]
- `Server.Wait` waits until a condition is true or the context ends. [server.go:Server.Wait]

### File server fake

- `FileRequest` is one recorded file-server request (`Method`, `Path`, `Header`, `Body`). [files.go:FileRequest]
- `Server.FileOrigin` returns the origin to add to `slackapi.Options.FileOrigins`. [files.go:Server.FileOrigin]
- `Server.UploadURL` returns an upload URL that answers `OK - <length>`. [files.go:Server.UploadURL]
- `Server.ServeFile` serves a fixed body at a path and returns its URL. [files.go:Server.ServeFile]
- `Server.HandleFile` serves a computed response at a path and returns its URL. [files.go:Server.HandleFile]
- `Server.FileRequests` returns the recorded requests to a file target, in order. [files.go:Server.FileRequests]

### Socket Mode fake

- `Server.SocketMode` installs the fake Socket Mode on the server. [socketmode.go:Server.SocketMode]
- `SocketModeOptions` configures `AppToken`, `AppID`, `PingInterval`, `ResendAfter`, and `MaxConnections`. [socketmode.go:SocketModeOptions]
- `SocketMode` is the fake Socket Mode service. [socketmode.go:SocketMode]
- `SocketMode.Open` answers `apps.connections.open`. [socketmode.go:SocketMode.Open]
- `SocketMode.SetAppToken` changes the accepted app-level token. [socketmode.go:SocketMode.SetAppToken]
- `SocketMode.Send` sends an envelope to a random live connection and returns its ID. [socketmode.go:SocketMode.Send]
- `SocketMode.Disconnect` and `SocketMode.DisconnectConn` send a disconnect message to all live connections or to one connection. [socketmode.go:SocketMode.Disconnect,SocketMode.DisconnectConn]
- `SocketMode.StopPings` stops pings on one connection. [socketmode.go:SocketMode.StopPings]
- `SocketMode.Drop` closes one connection without a close frame. [socketmode.go:SocketMode.Drop]
- `SocketMode.Live`, `SocketMode.Conns`, `SocketMode.Closes`, `SocketMode.Envelope`, and `SocketMode.Envelopes` return counts and snapshots. [socketmode.go:SocketMode.Live,SocketMode.Conns,SocketMode.Closes,SocketMode.Envelope,SocketMode.Envelopes]
- `SocketMode.WaitLive`, `SocketMode.WaitClosed`, and `SocketMode.WaitAcked` wait for a condition and fail the test on timeout. [socketmode.go:SocketMode.WaitLive,SocketMode.WaitClosed,SocketMode.WaitAcked]
- `SocketConn` is a snapshot of one connection. `SocketConn.Live` reports hello received, not closed, and not dropped. [socketmode.go:SocketConn,SocketConn.Live]
- `SocketEnvelope`, `SocketDelivery`, and `SocketAck` are snapshots of one envelope, one send of it, and one ack. [socketmode.go:SocketEnvelope,SocketDelivery,SocketAck]

### Signing

- `SignRequest` sets the Slack v0 signature headers on a request. [sign.go:SignRequest]

## Invariants

- A `Server` is safe for concurrent use. [doc.go:slackapitest]
- `Handle` and `HandleFile` functions run on the server goroutine. They must not call `t.Fatal`. [server.go:Server.Handle], [files.go:Server.HandleFile]
- The `Wait` condition must not block. `Wait` does not see a change that a `Handle` or `HandleFile` function makes until that function returns. [server.go:Server.Wait]
- A request to a method with no handler fails the test (`t.Errorf`) and gets `{"ok":false,"error":"unknown_method"}`. [server.go:Server.serveHTTP]
- A request to a file path with no handler fails the test and gets status 404. [files.go:Server.serveFile]
- A Web API request whose path does not start with `/api/` followed by a method name fails the test (`t.Errorf`) and gets status 404. [server.go:Server.serveHTTP,apiPrefix]
- `SocketMode.Send` with a payload that is not valid JSON fails the test (`t.Errorf`), sends nothing, and returns an empty ID. [socketmode.go:SocketMode.Send,encodeJSON]
- A websocket upgrade with an unknown ticket, or after the fake has stopped, gets status 404. [socketmode.go:SocketMode.serve]
- The fake reads at most `socketReadLimit` (1 MiB) per websocket message from a client. [socketmode.go:socketReadLimit,SocketMode.serve]
- The server parses `Call.Form` only when the `Content-Type` is `application/x-www-form-urlencoded`. [server.go:isForm]
- A zero `StatusCode` sends 200. A `json.RawMessage` or `[]byte` body is sent as is. Other bodies are sent as JSON with a default `application/json` content type. [server.go:statusOrOK,encodeBody,Server.write]
- `Server.SocketMode` can run once per `Server`. A second call calls `t.Fatalf`. [socketmode.go:Server.SocketMode]
- `SocketModeOptions` defaults: `AppID` `A0123456789`, `PingInterval` 10s, `ResendAfter` 3s, `MaxConnections` 10. A negative `PingInterval` sends no pings. A negative `ResendAfter` turns resends off. [socketmode.go:Server.SocketMode,SocketMode.ping,SocketMode.deliverLocked]
- `SocketMode.Open` returns `not_authed` with no bearer token, and `invalid_auth` when the token differs from a non-empty `AppToken`. [socketmode.go:SocketMode.Open]
- `SetAppToken` does not change connections that are already open. [socketmode.go:SocketMode.SetAppToken]
- An unacked envelope is resent at most `maxResends` (3) times. An ack stops its resend timer. [socketmode.go:SocketMode.deliverLocked,SocketMode.ack]
- A connection at or over `MaxConnections` gets disconnect reason `too_many_websockets` and no hello. [socketmode.go:SocketMode.serve]
- The fake sets `accepts_response_payload` to true only for `slash_commands` envelopes. [socketmode.go:SocketMode.Send]
- An ack without `envelope_id`, an ack for an unknown envelope, and a binary websocket message fail the test (`t.Errorf`). [socketmode.go:SocketMode.ack,SocketMode.read]
- Hellos, deliveries, acks, and closes take sequence numbers from one counter, so a test can compare their order. [socketmode.go:SocketMode.nextSeqLocked]

## Side Effects

- `NewServer` starts two `httptest` servers (Web API and files) and registers `t.Cleanup` to close them. [server.go:NewServer]
- The server records calls and file requests in maps under `Server.mu`, and wakes `Wait` callers on each change. [server.go:Server.serveHTTP,notifier.notify], [files.go:Server.serveFile]
- `Server.SocketMode` registers an `apps.connections.open` handler and serves websocket connections at `/link/`. [socketmode.go:Server.SocketMode,SocketMode.Open]
- Each websocket connection starts write and ping goroutines. Pings use `time.NewTicker`. Resends use `time.AfterFunc`. [socketmode.go:SocketMode.serve,SocketMode.ping,SocketMode.deliverLocked]
- `SocketMode.Send` picks a live connection with `math/rand/v2`. [socketmode.go:SocketMode.deliverLocked]
- At test cleanup, the fake stops resend timers, closes open connections without a close frame, and waits for its goroutines. [socketmode.go:SocketMode.stop]
- The package does no disk access, logging, or metrics. [socketmode.go:SocketMode]

## Verification

- Run: `go test ./adapters/slack/slackapi/slackapitest/...`
