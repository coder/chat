# ADR 0017: Slack Socket Mode

## Status

Accepted

## Context

Slack delivers events to an app in two ways. Over HTTP, Slack posts to the Events API request URL, the slash command URLs, and the interactivity URL. Over Socket Mode, the app opens a websocket to Slack, Slack sends each payload on it as an envelope, and the app acknowledges the envelope on the same connection.

Before this ADR, the library supported only HTTP. HTTP needs a public URL, so an app behind a firewall, a VPN, or Tailscale needs a tunnel, and so does a developer on a laptop.

coderd, the Coder server, uses the **Slack API Package** directly and runs its own runtime (ADR 0016). It will use Socket Mode only. Its spec lists outcomes that the Socket Mode client must give: no gap on planned changes, a shutdown that stops acknowledging before it closes, replacement of silent connections, fatal errors that stop retries, a capped backoff with jitter that honours `Retry-After`, a close frame on every close, and proxy support.

The prior art is bolt-js (`SocketModeReceiver` and `HTTPReceiver`), node-slack-sdk (`SocketModeClient`), slack-go (`socketmode`), and python-slack-sdk (`socket_mode`). No SDK sends a close frame every time: slack-go and python close the raw socket. A connection that ends without a close frame can stay registered with Slack for hours, count towards Slack's limit of 10 connections for each app, and still get envelopes.

Related decisions, not redefined here: deferred dispatch is ADR 0002; rate-limit retry is ADR 0005; the **Admission Bound** is ADR 0015; the **Slack API Package** is ADR 0016.

## Decision

1. **One transport-neutral delivery and handler in `slackapi`.** A `Delivery` is one inbound payload from Slack. Its type is `events_api`, `slash_commands`, or `interactive`, as in Socket Mode. Its payload has the Socket Mode shape, and it carries the retry attempt and reason. A `Handler` is `func(ctx context.Context, d *Delivery) (response any, err error)`. One outcome table serves both transports:

   | Handler result | HTTP | Socket Mode |
   |---|---|---|
   | nil error | 200, with the response as a JSON body if it is not nil | ack, with the response as `payload` if it is not nil |
   | `ErrBusy` | 503 | no ack |
   | `ErrInvalidPayload` | 400 | no ack |
   | other error | 500 | no ack |

2. **Two thin transports in `slackapi`.** Each transport adds only its own first step (receive and authenticate) and its own last step (answer Slack).
   - `Webhook` is the HTTP transport, an `http.Handler`. It accepts only POST, limits the body to 1 MiB, and calls `VerifyRequest` before any parse. It answers `url_verification` itself (the **Platform Handshake**). It re-encodes a slash command form as a JSON object of strings, the shape that Socket Mode sends. It reads the retry headers into the delivery.
   - `(*Client).Listen(ctx, handler, opts)` is the Socket Mode transport. The caller passes a Client with the app-level token, for example `api.WithToken(appToken).Listen(...)`. Listen uses the Client's HTTP client, base URL, and logger. `(*Client).OpenConnection` calls `apps.connections.open`. Listen does not use the Client's retry policy for this call: the policy caps a wait at `MaxDelay`, so it can retry before `Retry-After`. The backoff of Listen retries the call instead.

3. **One adapter pipeline.** The Slack adapter has one `Handler` for both transports. It parses the payload by delivery type, resolves the install by team, normalizes the payload, and dispatches it. Parse and validation failures wrap `ErrInvalidPayload`. A workspace with no install gives an ack and an **Ignored Event**. When the runtime rejects a delivery at admission, a slash command acks with the busy reply as JSON, and events and interactions return `ErrBusy`. `Options.SigningSecret` enables `Webhook`, and `Options.AppToken` enables `Listen`. The adapter needs at least one of them, and accepts both. Multi-tenant installs work the same over both transports.

4. **The `Listener` Optional Capability and `Chat.Listen`.** A **Platform Adapter** that opens its own connections to the platform implements `chat.Listener`:

   ```go
   type Listener interface {
       Listen(ctx context.Context, dispatch DispatchFunc) error
   }
   ```

   Listen dispatches each inbound payload through `dispatch`, and blocks until ctx is done. Then it drains, closes its connections, and returns nil. A non-nil error means that Listen cannot continue. `(*Chat).Listen(ctx, adapterName)` runs the capability. It returns an error at once if the adapter is not registered, or if the adapter is not a `Listener` (the error wraps `ErrUnsupportedCapability`). It returns nil at once after **Runtime Shutdown** starts. Else it blocks until ctx is done or Runtime Shutdown stops the listeners. Concurrent calls are allowed.

5. **Shutdown order.** Runtime Shutdown runs these steps in order, bounded by the Shutdown context:
   1. Mark the runtime as shut down, and close admission. Every new dispatch, in both **Dispatch Modes**, returns an error that matches `ErrAdmissionRejected`. The Slack adapter then gives no ack for events and interactions, and the busy reply for slash commands, so Slack sends new envelopes to other connections.
   2. Cancel and drain deferred work.
   3. Stop the listeners: cancel every Listen context, and wait for every Listen call to return. The listeners drain and send close frames.
   4. Shut down the adapters, then the state.

6. **Socket Mode connection rules.**
   - **Connect:** call `OpenConnection`, dial the websocket with the Client's HTTP client, and wait for `hello`. The whole connect has a 30 second limit. The HTTP client gives the handshake its proxy settings. The read limit is 1 MiB, the same as the `Webhook` body limit. `ListenOptions.OnConnect` gets each hello, with `num_connections`, the app ID, and the host.
   - **Envelopes:** each envelope runs the handler in its own goroutine. The ack goes on the connection that the envelope came on. The handler context ends when that connection closes, not when the Listen context ends. An envelope of an unknown type still goes to the handler, with its type.
   - **Planned replacement with no gap:** a `disconnect` message with the reason `warning`, `refresh_requested`, or an unknown reason starts a replacement connection, and so does a ping timeout. The old connection keeps reading and acknowledging until the new connection has its hello. Then the old connection closes.
   - **Close order:** a connection closes in four steps. First, it waits for its in-flight handlers, at most 3 seconds (Slack's ack window). Envelopes that arrive during this wait still get handled and acknowledged. Second, `Close(StatusNormalClosure)` sends the close frame. Third, it waits for the read loop to end. Fourth, it cancels the handler context. The read loop never uses a context that can be cancelled, because a cancelled read context closes the connection without a close frame. Only `Close` ends the read loop, so every close sends a close frame. For the same reason, a stop does not cancel a websocket handshake that runs: Slack can accept the connection before the handshake returns. The close waits for the handshake, which the 30 second connect limit bounds, and then sends the close frame.
   - **Ping timeout:** Slack sends a websocket ping about every 10 seconds. A timer restarts on each ping, and starts only after the first ping, as in node-slack-sdk. When `ListenOptions.PingTimeout` (30 seconds by default) passes with no ping, Listen replaces the connection and reports `ErrPingTimeout` to `OnError`. The timer does not depend on the read loop, so a stuck read cannot block it.
   - **Fatal errors:** Listen stops and returns the error. These errors are fatal: an `*APIError` from `apps.connections.open` with the code `not_authed`, `invalid_auth`, `account_inactive`, `token_revoked`, `token_expired`, `user_removed_from_team`, `team_disabled`, `not_allowed_token_type`, or `missing_scope`, and a `*DisconnectError` with the reason `link_disabled`.
   - **`too_many_websockets`:** a `disconnect` message or an API error code with this reason closes that connection at once. Listen reports it to `OnError`, and waits `MaxBackoff` with jitter before the next connect.
   - **Other errors:** connect failures and dropped connections go to `OnError`, and Listen retries with an exponential backoff with equal jitter, from `MinBackoff` (1 second by default) to `MaxBackoff` (1 minute by default). A `*RateLimited` error with `RetryAfter` waits at least `RetryAfter`. A dropped connection that was up for at least `MaxBackoff` reconnects at once. Else the drop counts as a failure, so a connection that drops again and again backs off. Failed connects during a replacement back off in the same way: a connection that was up for at least `MaxBackoff` resets the failure count once, when it drops, gets a `disconnect` message, or starts its replacement, and not at each failed connect.
   - **Stop:** when ctx is done, Listen closes every connection in the close order, waits for every handler goroutine, and returns nil.

7. **The first dependency: `github.com/coder/websocket`.** Socket Mode needs a websocket client, so the module now depends on `github.com/coder/websocket`. This ends the zero-dependency rule of ADR 0016. The reasons for this module:
   - Its API takes a context in `Dial`, `Read`, `Write`, and `Ping`, which fits the limits of Listen.
   - It has no dependencies, so it adds no transitive dependencies.
   - Coder maintains it, and coderd is the main user of Socket Mode in this module.
   - Its `Close` sends a close frame, waits for the close handshake, and works while another goroutine reads. The close rule needs exactly this.

8. **`slackapitest` gets a fake Socket Mode.** `(*Server).SocketMode` makes `apps.connections.open` return a `ws://` URL on the fake server. The fake sends hello and websocket pings on each connection. It sends each envelope to a random live connection, and resends an envelope that gets no ack up to 3 times, with `retry_reason` `timeout`, as Slack does. A connection over `MaxConnections` gets a `too_many_websockets` disconnect message. Tests can send disconnect messages, stop pings, and drop connections. They can check which connection got each envelope and sent each ack, the order of hellos and closes, and the close frame of each close. The Listen tests use the fake, and coderd can use it for its own Socket Mode tests.

## Consequences

- An app can receive Slack events with no public URL and no tunnel.
- coderd gets the outcomes that the library owns: Slack refresh with no gap, drain then close frames, silent connection replacement, fatal errors, backoff, close frames, and proxies. Listen gives hooks for the rest: `Hello.NumConnections` for the extra connection warning, and `OnConnect` for health. coderd keeps the choice of holders, the handover timing, and the rest of its spec: no flapping in rolling deploys, token change and delete, disabled bots, and link button clicks. Its handler returns an error to stop acks during its shutdown.
- Both transports share one pipeline, so a payload gives the same `chat.Event` over HTTP and over Socket Mode.
- The glossary term **Webhook Event** becomes **Delivery**, to match `slackapi.Delivery`. **Listener** and **Socket Mode** are new terms.
- After Runtime Shutdown starts, a dispatch in the synchronous **Dispatch Mode** returns `ErrAdmissionRejected`. Before, sync mode had no admission gate.
- The slash command busy reply is now JSON, because a Socket Mode ack must be JSON. Slack also accepts JSON over HTTP.
- The module has a dependency. Every module that requires this module gets `github.com/coder/websocket` in its module graph, but only builds that import `slackapi` or `slackapitest` compile it. The rejection of `slack-go/slack` in ADR 0016 stands.
- Cost: the Socket Mode client is concurrent code with timing rules. Its tests run against the fake with `-race`. Each connection uses a read loop, a ping timer, and one goroutine for each in-flight envelope.

## Alternatives Considered

### Keep HTTP only

Rejected. coderd needs Socket Mode for deployments behind a firewall, a VPN, or Tailscale. Every other user needs a tunnel to try the library on a laptop.

### A separate Socket Mode pipeline in the adapter

Rejected. The adapter would have two copies of the parse, install resolution, normalization, and admission mapping, and the copies would drift. Slash commands would also reach the handler in two shapes.

### Socket Mode only in `slackapi`, with no runtime capability

Rejected. Each runtime user would have to connect `Listen` to the adapter pipeline by hand. Runtime Shutdown could not close admission before the connections close, so Slack would keep sending envelopes to a runtime that stops.

### Use `slack-go/slack` `socketmode`

Rejected. It closes the raw socket, so it sends no close frame. It is also a large dependency with its own retry and error types, which ADR 0016 already rejects.

### Use `github.com/gorilla/websocket`

Rejected. Its API has no context, only deadlines. Its `Close` closes the network connection without a close frame. A close frame needs a manual handshake: write a close message, then wait for the peer's close in the reader.

### Write a websocket client in this repository

Rejected. The module would own the framing, masking, fragmentation, control frames, and close handshake of the websocket protocol, and the tests for all of them. `github.com/coder/websocket` already does this, passes the Autobahn test suite, and has no dependencies.
