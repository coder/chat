// Package slackapi is a low-level Slack package for applications that own
// their runtime: their own state, dispatch, and routing. It works on raw Slack
// IDs (team, channel, and user IDs and message timestamps) and does not use the
// portable chat.Adapter surface. It holds Slack protocol and data formats only.
// It has no runtime, state, dispatch, or policy; the application brings those.
// The Slack adapter (package slack) uses it for its Web API calls, rate-limit
// retry, history reads, and both inbound transports. See ADR 0016 and ADR 0017.
//
// The package is experimental. Its exported API may change before promotion.
// RetryPolicy and RateLimited keep the Slack adapter's supported tier because
// the adapter exports them as type aliases.
//
// # Web API
//
// New returns a Client for Options; the fields of Options document the
// defaults. The typed methods cover the calls that a bot needs: AuthTest,
// PostMessage, PostEphemeral, UpdateMessage, DeleteMessage,
// SetAssistantThreadStatus, SetAgentSessionStatus, AddReaction,
// RemoveReaction, UserInfo, ConversationInfo, ConversationHistory,
// ConversationReplies, and FileInfo. Call is the escape hatch for other
// methods. PostResponseURL posts JSON without a token to the response_url of a
// slash command or an interaction. WithToken returns a copy of a Client that
// sends another token.
//
// The setup of a bot needs three more methods. OAuthV2Access exchanges the code
// of an install for a bot token, with the client ID and the client secret of
// the app and no bearer token. CreateApp and DeleteApp create and delete an
// app from a Manifest. Call them on a Client with an app configuration token:
// api.WithToken(configToken).CreateApp(ctx, req).
//
// Web API calls are POSTs with the bearer token and a JSON body. Slack
// documents users.info, conversations.info, conversations.history,
// conversations.replies, files.info, and files.getUploadURLExternal as GET or
// form-only methods, so the client sends them with a form-encoded body.
// OAuthV2Access, CreateApp, and DeleteApp send a form-encoded body too. Call
// sends a url.Values payload form encoded and any other payload as JSON, so it
// reaches form-only methods too.
//
// An ok:false response or a non-2xx status returns *APIError. Every method
// except DownloadFile retries Slack throttling (HTTP 429, or the ratelimited or
// rate_limited error code) within Options.RetryPolicy, and never sleeps past
// the context deadline (ADR 0005). When retry stops, the method returns
// *RateLimited.
//
// # Files
//
// UploadFile runs the three steps of an external upload: GetUploadURLExternal,
// UploadToURL, and CompleteUploadExternal. UploadToURL sends raw bytes and no
// token, because the upload URL is pre-authorized. DownloadFile GETs a file
// with the bearer token and a size limit, and wraps ErrFileTooLarge when the
// file is larger. UploadToURL and DownloadFile accept only URLs whose origin is
// in Options.FileOrigins, and both check every redirect too, so a URL from an
// untrusted payload cannot send the token or file content to another host.
//
// # Inbound payloads
//
// A Delivery is one inbound payload from Slack: an Events API payload, a slash
// command, or an interactivity payload. A Handler handles a Delivery and gives
// the acknowledgement. Its error tells the transport how to answer Slack:
// ErrBusy, ErrInvalidPayload, or another error.
//
// Webhook is the HTTP transport. It is an http.Handler for the request URLs of
// the app. It checks the signature with VerifyRequest, answers the
// url_verification request, and gives each other request to its Handler as a
// Delivery.
//
// ParseEnvelope decodes an Events API payload. Envelope.MessageEvent decodes
// its app_mention or message event, Envelope.AgentSessionStoppedEvent its
// agent_session_stopped event, and Envelope.ChannelSharedEvent its
// channel_shared event.
//
// # Socket Mode
//
// Listen is the Socket Mode transport. The app opens a websocket to Slack, so
// it needs no public request URL. Call Listen on a Client with the app-level
// token: api.WithToken(appToken).Listen(ctx, handler, opts). Listen gets the
// websocket URL with OpenConnection, and opens the websocket with the HTTP
// client of the Client, so its proxy settings apply. It gives each envelope to
// its Handler as a Delivery, and acks the envelope on the connection that it
// came on.
//
// Listen keeps a connection up. When Slack asks for a refresh, or stops its
// pings, Listen opens a new connection before it closes the old one. It retries
// other errors with backoff, and reports them to ListenOptions.OnError. It
// returns when ctx is done, or on an error that a retry cannot fix, for example
// invalid_auth or link_disabled. A connection waits at most 3 seconds for its
// running handlers, and then closes with a close frame.
//
// The app-level token (xapp-) needs the connections:write scope, and Socket
// Mode must be on in the app settings. Webhook and Listen take the same
// Handler, so an app can use one transport or both. The Slack adapter runs
// Listen when the app calls Chat.Listen.
//
// # Formats
//
// MarkdownBlock, ActionsBlock with ButtonElement, and ImageBlock are Block Kit
// blocks. Any type that implements Block and encodes itself as a Block Kit
// object can also go in a []Block.
// SplitMarkdown splits long Markdown into chunks that fit in a MarkdownBlock;
// it does not convert Markdown. UserMentions and ReplaceUserMentions read and
// replace user mentions such as <@U123>. Manifest and its section types encode
// a Slack app manifest.
//
// # Tests
//
// Package slackapitest provides a fake Slack Web API server with a fake Socket
// Mode, and a request signer, for tests of code that uses this package.
package slackapi
