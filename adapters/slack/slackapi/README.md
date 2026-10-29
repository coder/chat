# slackapi

## Overview

Package `slackapi` (`github.com/coder/chat/adapters/slack/slackapi`) is a low-level Slack package for applications that own their runtime: their own state, dispatch, and routing. It works on raw Slack IDs (team, channel, and user IDs and message timestamps). It does not use the portable `chat.Adapter` surface. It holds Slack protocol and data formats only, with no runtime, state, dispatch, or policy. [doc.go:slackapi]

The package has four parts:

1. A Slack Web API client (`Client`) with typed methods, a generic `Call` escape hatch, and bounded rate-limit retry.
2. Two inbound transports that share one `Handler`: `Webhook` (signed HTTP requests) and `Client.Listen` (Socket Mode websocket).
3. Payload decoding for the Events API (`ParseEnvelope` and event types).
4. Formats: Block Kit blocks, a Markdown splitter, mention helpers, and app manifest types.

The Slack adapter (package `slack`) uses this package for its Web API calls, rate-limit retry, history reads, and both inbound transports. The package doc marks the API as experimental. `RetryPolicy` and `RateLimited` keep the supported tier, because the Slack adapter exports them as type aliases. [doc.go:slackapi]

## Interface

### Client

- `New` returns a `Client` for `Options`, with defaults for every empty field. [client.go:New]
- `Options` holds `Token`, `BaseURL`, `HTTPClient`, `RetryPolicy`, `FileOrigins`, `Observer`, and `Logger`. [client.go:Options]
- `DefaultBaseURL` is `https://slack.com/api`. [client.go:DefaultBaseURL]
- `Client` is the Web API client. [client.go:Client]
- `Client.WithToken` returns a copy of the client that sends another token. [client.go:Client.WithToken]
- `Client.Call` is the generic Web API escape hatch. [client.go:Client.Call]
- `Client.PostResponseURL` POSTs JSON to a `response_url` with no token. [client.go:Client.PostResponseURL]
- `APIError` is a Slack rejection (`Method`, `StatusCode`, `Code`, `Detail`, `Raw`). [errors.go:APIError]
- `RetryPolicy` sets `MaxAttempts`, `MaxElapsed`, `BaseDelay`, and `MaxDelay`. [retry.go:RetryPolicy]
- `RateLimited` is the error returned when throttling retry stops. [retry.go:RateLimited]

### Typed Web API methods

| Method | Slack API method | Source |
| --- | --- | --- |
| `Client.AuthTest` | `auth.test` | [auth.go:Client.AuthTest] |
| `Client.CreateApp`, `Client.DeleteApp` | `apps.manifest.create`, `apps.manifest.delete` | [apps.go:Client.CreateApp,Client.DeleteApp] |
| `Client.PostMessage` | `chat.postMessage` | [chat.go:Client.PostMessage] |
| `Client.PostEphemeral` | `chat.postEphemeral` | [chat.go:Client.PostEphemeral] |
| `Client.UpdateMessage` | `chat.update` | [chat.go:Client.UpdateMessage] |
| `Client.DeleteMessage` | `chat.delete` | [chat.go:Client.DeleteMessage] |
| `Client.ConversationInfo` | `conversations.info` | [conversations.go:Client.ConversationInfo] |
| `Client.ConversationHistory` | `conversations.history` | [conversations.go:Client.ConversationHistory] |
| `Client.ConversationReplies` | `conversations.replies` | [conversations.go:Client.ConversationReplies] |
| `Client.FileInfo` | `files.info` | [files.go:Client.FileInfo] |
| `Client.GetUploadURLExternal` | `files.getUploadURLExternal` | [files.go:Client.GetUploadURLExternal] |
| `Client.CompleteUploadExternal` | `files.completeUploadExternal` | [files.go:Client.CompleteUploadExternal] |
| `Client.OAuthV2Access` | `oauth.v2.access` | [oauth.go:Client.OAuthV2Access] |
| `Client.AddReaction`, `Client.RemoveReaction` | `reactions.add`, `reactions.remove` | [reactions.go:Client.AddReaction,Client.RemoveReaction] |
| `Client.SetAssistantThreadStatus` | `assistant.threads.setStatus` | [status.go:Client.SetAssistantThreadStatus] |
| `Client.SetAgentSessionStatus` | `agents.sessions.setStatus` | [status.go:Client.SetAgentSessionStatus] |
| `Client.UserInfo` | `users.info` | [users.go:Client.UserInfo] |
| `Client.OpenConnection` | `apps.connections.open` | [socketmode.go:Client.OpenConnection] |

Most methods have request or response types in the same file, for example `PostMessageRequest` and `PostMessageResponse` [chat.go:PostMessageRequest,PostMessageResponse].

Some of those types hold nested exported types:

- `AppCredentials`: the app credentials in `CreateAppResponse`. [apps.go:AppCredentials]
- `FileSummary`: one uploaded file in `CompleteUploadExternalRequest`. [files.go:FileSummary]
- `FileShares` and `FileShare`: the shares of a file by channel ID, in `FileInfoResponse`. [files.go:FileShares,FileShare]
- `OAuthTeam`: the workspace or Enterprise Grid organization in `OAuthV2AccessResponse`. [oauth.go:OAuthTeam]

### Files

- `Client.UploadFile` runs the three-step external upload and returns the completed `File`. [files.go:Client.UploadFile]
- `Client.UploadToURL` POSTs raw bytes to a pre-authorized upload URL. [files.go:Client.UploadToURL]
- `Client.DownloadFile` GETs a file URL with the bearer token into a writer, with a size limit. [files.go:Client.DownloadFile]
- `ErrFileTooLarge` is the sentinel for a file over the size limit. [files.go:ErrFileTooLarge]

### Inbound payloads and transports

- `Delivery` is one inbound payload (`Type`, `Payload`, `RetryAttempt`, `RetryReason`). [delivery.go:Delivery]
- `DeliveryType` has the values `DeliveryEventsAPI`, `DeliverySlashCommands`, and `DeliveryInteractive`. [delivery.go:DeliveryType]
- `Handler` handles a `Delivery` and returns the ack response. [delivery.go:Handler]
- `ErrBusy` and `ErrInvalidPayload` are sentinel handler errors. [delivery.go:ErrBusy,ErrInvalidPayload]
- `Webhook` is the HTTP transport (`SigningSecret`, `Handler`, `Now`, `SignatureTolerance`). `Webhook.ServeHTTP` handles one request. [webhook.go:Webhook,Webhook.ServeHTTP]
- `VerifyRequest` checks the Slack v0 HMAC-SHA256 request signature. `DefaultSignatureTolerance` is 5 minutes. [signature.go:VerifyRequest,DefaultSignatureTolerance]
- `Client.Listen` is the Socket Mode transport. [socketmode.go:Client.Listen]
- `ListenOptions` holds `OnConnect`, `OnError`, `PingTimeout`, `MinBackoff`, and `MaxBackoff`. `OnConnect` and `OnError` run on the goroutine that called `Listen`, so they must return quickly. [socketmode.go:ListenOptions]
- `Hello` is the Socket Mode hello message. `DisconnectError` is a Slack disconnect message as an error. [socketmode.go:Hello,DisconnectError]
- `ErrPingTimeout` and `ErrConnectTimeout` are Socket Mode sentinel errors. [socketmode.go:ErrPingTimeout,ErrConnectTimeout]

### Events API decoding

- `ParseEnvelope` decodes an Events API body. It does not check the signature. [envelope.go:ParseEnvelope]
- `Envelope` is the outer Events API body. [envelope.go:Envelope]
- `Envelope.MessageEvent` decodes `app_mention` and `message` events. `Envelope.AgentSessionStoppedEvent` and `Envelope.ChannelSharedEvent` decode one event type each. [envelope.go:Envelope.MessageEvent,Envelope.AgentSessionStoppedEvent,Envelope.ChannelSharedEvent]
- `EnvelopeURLVerification`, `EnvelopeEventCallback`, `EventType*`, and `Subtype*` are envelope, event, and message subtype constants. [envelope.go:EnvelopeURLVerification,EventTypeAppMention,SubtypeMessageChanged]

### Data types and formats

- `Message`, `BotProfile`, `Edited`, and `File` are Slack message types. [message.go:Message,BotProfile,Edited,File]
- `User` and `UserProfile` are users.info types. `User.DisplayName` returns the first non-empty name. [users.go:User,UserProfile,User.DisplayName]
- `Conversation` and `MessagePage` are conversations types. [conversations.go:Conversation,MessagePage]
- `AgentSessionActive` and `AgentSessionProcessing` are agent session status values. [status.go:AgentSessionActive,AgentSessionProcessing]
- `Block` is the Block Kit block interface. `MarkdownBlock`, `ActionsBlock`, `ButtonElement`, and `ImageBlock` implement Block Kit objects. [blocks.go:Block,MarkdownBlock,ActionsBlock,ButtonElement,ImageBlock]
- `SplitMarkdown` splits text into fence-aware chunks. `MarkdownBlockLimit` is 12000. [markdown.go:SplitMarkdown,MarkdownBlockLimit]
- `UserMentions` and `ReplaceUserMentions` read and replace `<@U...>` mentions. [mentions.go:UserMentions,ReplaceUserMentions]
- `Manifest` and its `Manifest*` section types encode a Slack app manifest. [manifest.go:Manifest]

### Subdirectories

- [slackapitest](./slackapitest/README.md): fake Slack Web API, file server, and Socket Mode, plus a request signer, for tests.

## Invariants

### Client and retry

- `Client` is safe for concurrent use. [client.go:Client]
- `New` trims trailing slashes from `BaseURL`. It drops each `FileOrigins` entry that is not an absolute URL with a host, and logs a warning for it. [client.go:New]
- The client sends the `Authorization` header only when the token is not empty. `UploadToURL`, `PostResponseURL`, and `OAuthV2Access` never send the client token. [client.go:send,Options], [oauth.go:Client.OAuthV2Access]
- `Call` encodes a nil payload as `{}`, a `url.Values` payload as a form body, and any other payload as JSON. [client.go:Client.Call,encodeRequest]
- An `ok:false` response or a non-2xx status returns `*APIError`. [client.go:Client.Call,send]
- `WithToken` shares the base URL, file origins, HTTP client, retry policy, observer, and logger with the original client. [client.go:Client.WithToken]
- `RetryPolicy` defaults for zero fields: `MaxAttempts` 3, `MaxElapsed` 2s, `BaseDelay` 200ms, `MaxDelay` 1s. `MaxAttempts` 1 turns retry off. [retry.go:RetryPolicy.withDefaults]
- Retry runs only on HTTP 429 or an `ok:false` envelope with error `ratelimited` or `rate_limited`. Transport errors return directly and are never retried. [retry.go:slackThrottle,Client.doWithRetry]
- Retry returns `*RateLimited` when `MaxAttempts` is reached, when total backoff would pass `MaxElapsed`, or when the next sleep would pass the context deadline. [retry.go:Client.doWithRetry]
- Each retry attempt sends a fresh copy of the buffered request body. [retry.go:Client.doWithRetry]
- `DownloadFile` does not retry. [doc.go:slackapi]

### Messages

- `PostMessageRequest.MarkdownText` cannot combine with `Text` or `Blocks`, and holds at most 12,000 characters. [chat.go:PostMessageRequest]
- `PostMessageRequest.ThreadTS` must be the parent message ts, not a reply ts. [chat.go:PostMessageRequest]
- `PostEphemeralResponse.MessageTS` cannot update or delete the ephemeral message. [chat.go:PostEphemeralResponse]
- `UpdateMessage` with `Text` and no `Blocks` removes the existing blocks. A bot token can update and delete only its own messages. [chat.go:Client.UpdateMessage,Client.DeleteMessage]
- `ConversationHistory` returns messages newest first. `ConversationReplies` returns them oldest first. [conversations.go:Client.ConversationHistory,Client.ConversationReplies]
- `Message.Raw` holds the raw JSON of each message from history reads and is never JSON-encoded. [conversations.go:messagePage], [message.go:Message]
- `SetAssistantThreadStatusRequest.LoadingMessages` holds at most 10 entries. An empty `Status` clears the status. [status.go:SetAssistantThreadStatusRequest]

### Files

- `UploadToURL` and `DownloadFile` accept only URLs whose origin is in `FileOrigins`. They check every redirect against `FileOrigins` and reject a redirect that changes the HTTP method. [files.go:Client.checkFileURL,Client.fileRedirectPolicy]
- Origin matching lowercases scheme and host and strips default ports. [client.go:originOf]
- `DownloadFile` requires a positive `maxBytes`. It rejects a `text/html` response. A larger body returns an error that wraps `ErrFileTooLarge`. [files.go:Client.DownloadFile]
- `UploadFile` wraps each step error with the step name, so `errors.As` still finds `*APIError` and `*RateLimited`. [files.go:Client.UploadFile]

### Inbound transports

- A `Handler` must not do slow work, because Slack expects an ack within 3 seconds. [delivery.go:Handler]
- Inbound payloads are capped at `maxPayloadBytes` (1 MiB) in `Webhook` and `Listen`. [delivery.go:maxPayloadBytes], [webhook.go:Webhook.ServeHTTP], [socketmode.go:socketConn.dial]
- `Webhook.ServeHTTP` accepts only POST (else 405). It verifies the signature before it parses the body (else 401). [webhook.go:Webhook.ServeHTTP]
- A body over 1 MiB returns 413, and an unreadable body returns 400. [webhook.go:Webhook.ServeHTTP]
- After the signature check, a form body with `payload` becomes a `DeliveryInteractive`, and a form body with `command` becomes a `DeliverySlashCommands` whose payload is the form fields as a JSON object. Any other body becomes a `DeliveryEventsAPI`. Another form body, a `payload` that is not valid JSON, or a JSON body that does not parse returns 400. `X-Slack-Retry-Num` and `X-Slack-Retry-Reason` fill `RetryAttempt` and `RetryReason`. [webhook.go:parseRequest]
- `Webhook.ServeHTTP` answers `url_verification` with the challenge and does not call the `Handler`. [webhook.go:parseRequest,Webhook.ServeHTTP]
- `Webhook` maps handler errors: `ErrBusy` to 503, `ErrInvalidPayload` to 400, and any other error to 500. [webhook.go:Webhook.ServeHTTP]
- `Webhook` requires `SigningSecret` and `Handler`. `Webhook` is safe for concurrent use. [webhook.go:Webhook]
- `VerifyRequest` rejects an empty secret, missing headers, a bad timestamp, and clock skew beyond the tolerance in either direction. It compares signatures with `hmac.Equal`. [signature.go:VerifyRequest]
- `Listen` acks an envelope only when the handler returns no error. It acks on the connection the envelope came on. [socketmode.go:socketConn.handle]
- `Listen` returns at once on fatal auth and config codes such as `invalid_auth`, `token_revoked`, `missing_scope`, and `link_disabled`. It retries other errors with backoff. [socketmode.go:fatal,Client.Listen]
- `Listen` returns nil when `ctx` is done, after it closes all connections and waits for handlers. Handler contexts are not cancelled when `ctx` is done. [socketmode.go:Client.Listen,listener.run]
- `Listen` promotes a replacement connection only after its hello arrives. The old connection stays up until then. [socketmode.go:listener.promote,listener.replace]
- A closing connection waits at most 3 seconds for running handlers, then sends a close frame. [socketmode.go:socketConn.drain,socketConn.close]
- `Listen` removes the query (the connection ticket) from websocket dial errors. [socketmode.go:withoutTicket]
- `ListenOptions` defaults: `PingTimeout` 30s, `MinBackoff` 1s, `MaxBackoff` 1m. `MinBackoff` is capped at `MaxBackoff`. [socketmode.go:ListenOptions.withDefaults]

### Formats

- `SplitMarkdown` uses `MarkdownBlockLimit` for a limit of zero or less, and returns nil for empty text. When the limit is at least twice the length of the longest fence line plus six runes, it closes and reopens a fence that a chunk ends inside, and does not end a chunk inside or right after a code-fence opening line. Below that limit it splits without this fence repair. [markdown.go:SplitMarkdown,splitFenced,longestFenceLine]
- `UserMentions` returns IDs in first-appearance order with no duplicates. [mentions.go:UserMentions]
- `ParseEnvelope` returns an error for invalid JSON or an empty envelope type. [envelope.go:ParseEnvelope]

## Side Effects

- `Call` and each typed Web API method in the table above send an HTTP POST to `BaseURL` + `/<method>` through the configured `http.Client`. [client.go:Client.Call,send] The file methods below use other URLs or verbs: `UploadFile` calls two Web API methods and also POSTs to the upload URL.
- `PostResponseURL` sends an HTTP POST to a caller-supplied `response_url`. [client.go:Client.PostResponseURL]
- `UploadToURL` POSTs file content to the upload URL. `DownloadFile` GETs a file and writes it to the caller's writer. Both use a copied `http.Client` with a custom redirect policy. [files.go:Client.UploadToURL,Client.DownloadFile,Client.fileHTTPClient]
- Retry sleeps between attempts with `ratelimit.SleepCtx` from `internal/ratelimit`. [retry.go:Client.doWithRetry]
- The client emits `chat.ObsAdapterCall` per attempt and `chat.ObsRateLimit` per throttle to `Options.Observer`. `DownloadFile` also emits `chat.ObsAdapterCall`. [retry.go:Client.doWithRetry], [files.go:Client.DownloadFile]
- The client logs a `slack rate limited` warning per throttle. `New` logs a warning per dropped file origin. [retry.go:Client.logRetry], [client.go:New]
- `Listen` dials a websocket with the client's HTTP client, under a 30s connect deadline. [socketmode.go:socketConn.dial,newSocketConn]
- `Listen` starts a read goroutine per connection and a goroutine per envelope handler. It uses timers for pings and retry backoff, and `math/rand/v2` for jitter. [socketmode.go:socketConn.read,socketConn.handle,socketConn.onPing,listener.failed,ListenOptions.backoff]
- `Listen` logs connects, refreshes, reconnects, ping timeouts, and retries through the client logger. [socketmode.go:listener.promote,listener.disconnected,listener.failed]
- `Webhook.ServeHTTP` reads the request body into memory, calls the `Handler` with the request context, and writes the HTTP response. [webhook.go:Webhook.ServeHTTP,writeResponse]
- `SplitMarkdown`, `UserMentions`, `ReplaceUserMentions`, and `VerifyRequest` are pure functions with no I/O. [markdown.go:SplitMarkdown], [mentions.go:UserMentions,ReplaceUserMentions], [signature.go:VerifyRequest]

## Verification

- Run: `go test ./adapters/slack/slackapi/...`
