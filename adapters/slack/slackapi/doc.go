// Package slackapi is a low-level Slack package for applications that own
// their runtime: their own state, dispatch, and routing. It works on raw Slack
// IDs (team, channel, and user IDs and message timestamps) and does not use the
// portable chat.Adapter surface. It holds Slack protocol and data formats only.
// It has no runtime, state, dispatch, or policy; the application brings those.
// The Slack adapter (package slack) uses it for its Web API calls, rate-limit
// retry, signature check, and history reads. See ADR 0016.
//
// The package is experimental. Its exported API may change before promotion.
// RetryPolicy and RateLimited keep the Slack adapter's supported tier because
// the adapter exports them as type aliases.
//
// # Web API
//
// New returns a Client for Options; the fields of Options document the
// defaults. The typed methods cover the calls that a bot needs: AuthTest,
// PostMessage, UpdateMessage, DeleteMessage, SetAssistantThreadStatus,
// AddReaction, RemoveReaction, UserInfo, ConversationInfo, ConversationHistory,
// and ConversationReplies. Call is the escape hatch for other methods.
// PostResponseURL posts JSON without a token to the response_url of a slash
// command or an interaction. WithToken returns a copy of a Client that sends
// another token.
//
// Web API calls are POSTs with the bearer token and a JSON body. Slack
// documents users.info, conversations.info, conversations.history,
// conversations.replies, and files.getUploadURLExternal as GET or form-only
// methods, so the client sends them with a form-encoded body. Call sends a
// url.Values payload form encoded and any other payload as JSON, so it reaches
// form-only methods too.
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
// # Inbound requests
//
// VerifyRequest checks the Slack v0 signature of a webhook request.
// ParseEnvelope decodes an Events API request body, and Envelope.MessageEvent
// decodes its app_mention or message event.
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
// Package slackapitest provides a fake Slack Web API server and a request
// signer for tests of code that uses this package.
package slackapi
