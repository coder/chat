# slack

## Overview

Package `slack` (`github.com/coder/chat/adapters/slack`) is the Slack adapter for the `chat` runtime (root package `github.com/coder/chat`). [doc.go:slack]

1. It receives Slack deliveries over HTTP (`Adapter.Webhook`), over Socket Mode (`Adapter.Listen`), or over both at the same time. One delivery handler serves both transports, so a payload becomes the same `chat.Event` on either transport. [doc.go:slack], [slack.go:deliveryHandler]
2. It posts messages, ephemeral messages, Block Kit content, modals, and `response_url` replies. [slack.go:PostMessage,PostEphemeralMessage], [interactive.go:PostNative,OpenModal,RespondURL]
3. It reads live history through `Adapter.ReadHistory`. [history.go:ReadHistory]

The adapter runs in one of two modes. Single-install mode uses a static `Options.BotToken` for one workspace. Multi-tenant mode uses `Options.InstallStore` (`chat.InstallStore`) and looks up a per-workspace bot token by `team_id`. [doc.go:slack]

The adapter sends every Slack Web API call and every `response_url` post through the child package `slackapi`, which owns rate-limit retry. [doc.go:slack]

## Interface

### Construction and types

- `New` builds an `Adapter` from `Options` after validation. [slack.go:New]
- `Options` holds tokens (`SigningSecret`, `AppToken`, `BotToken`), team and bot IDs, `InstallStore`, `APIBaseURL`, `Client`, `Now`, `SignatureTolerance`, `DisableNativeEphemeral`, `Logger`, `Observer`, and `RetryPolicy`. [slack.go:Options]
- `SlackInstall` is the per-workspace credential (`BotToken`, `BotUserID`) carried on `chat.Install.Credential`. [slack.go:SlackInstall]
- `Adapter` is the Slack adapter. [slack.go:Adapter]
- `RetryPolicy` is a type alias of `slackapi.RetryPolicy`. [retry.go:RetryPolicy]
- `RateLimited` is a type alias of `slackapi.RateLimited`. [retry.go:RateLimited]

### Adapter methods

- `Adapter.Name` returns `"slack"`. [slack.go:Name]
- `Adapter.Init` discovers the bot identity with `auth.test` in single-install mode, unless `TeamID`, `BotUserID`, and `BotID` are all configured. It does nothing in multi-tenant mode. [slack.go:Init]
- `Adapter.Shutdown` returns nil. [slack.go:Shutdown]
- `Adapter.Webhook` returns the HTTP transport handler for a `chat.DispatchFunc`. [slack.go:Webhook]
- `Adapter.Listen` runs the Socket Mode transport. It implements `chat.Listener`. [slack.go:Listen]
- `Adapter.ValidateThreadID` decodes a Thread ID into a `chat.ThreadRef`. [slack.go:ValidateThreadID]
- `Adapter.PostMessage` posts a message to a thread. [slack.go:PostMessage]
- `Adapter.PostEphemeralMessage` posts an ephemeral message, with an optional DM fallback. It implements `chat.EphemeralPoster`. [slack.go:PostEphemeralMessage], [interactive.go:EphemeralPoster assertion]
- `Adapter.BotActor` returns the bot as a `chat.Actor`. [slack.go:BotActor]
- `Adapter.ReadHistory` reads thread replies or DM history, newest first. It implements `chat.HistoryReader`. [history.go:ReadHistory]
- `Adapter.PostNative` posts Block Kit blocks to a thread. It implements `chat.NativeContentPoster`. [interactive.go:PostNative]
- `Adapter.OpenModal` and `Adapter.OpenModalForTenant` open a modal with a `trigger_id`. [interactive.go:OpenModal,OpenModalForTenant]
- `Adapter.OpenModalFromRaw` and `Adapter.OpenModalForTenantFromRaw` open a modal with the `trigger_id` from a raw inbound payload. [interactive.go:OpenModalFromRaw,OpenModalForTenantFromRaw]
- `Adapter.RespondURL` posts an ephemeral reply to the `response_url` from a raw inbound payload. [interactive.go:RespondURL]

The code has compile-time assertions for `chat.Listener`, `chat.HistoryReader`, `chat.NativeContentPoster`, and `chat.EphemeralPoster`. [slack.go:Listener assertion], [history.go:HistoryReader assertion], [interactive.go:NativeContentPoster assertion]

### Subdirectories

- [slackapi](./slackapi/README.md): low-level Slack Web API client, inbound transports (`Webhook`, Socket Mode `Listen`), payload decoding, and formats.

## Invariants

### Construction

- `New` requires `SigningSecret` or `AppToken`. [slack.go:New]
- `New` requires exactly one of `BotToken` or `InstallStore`. Both, or neither, is an error. [slack.go:New], [doc.go:slack]
- `New` rejects a negative `SignatureTolerance` and uses `slackapi.DefaultSignatureTolerance` when it is zero. [slack.go:New]
- `Init` requires non-empty `team_id`, `user_id`, and `bot_id` from `auth.test`. Each must match any configured value. [slack.go:Init,requireMatchingAuthField]
- Without a `SigningSecret`, `Webhook` answers 404 to each request. Without an `AppToken`, `Listen` returns an error. [slack.go:Webhook,Listen]

### Inbound deliveries

- When the runtime rejects a delivery with `chat.ErrAdmissionRejected`, the adapter acks a slash command with a busy reply. It returns a wrapped `slackapi.ErrBusy` for other deliveries, so Slack does not get an ack. [slack.go:deliveryHandler]
- An unknown delivery type, a non-`event_callback` envelope, and an interaction type other than `block_actions` are Ignored Events. [slack.go:normalizeDelivery,normalizeEvent], [interactive.go:normalizeInteraction]
- A malformed payload returns an error that unwraps to `slackapi.ErrInvalidPayload`. [slack.go:invalidPayload]
- The adapter accepts `app_mention` events, and `message` events with an empty or `bot_message` subtype. [slack.go:supportedMessageEvent]
- A message event needs envelope `team_id` and `event_id`, and event channel, `ts`, and an author ID. [slack.go:messageEvent]
- A slash command needs `command`, `team_id`, `channel_id`, `user_id`, and `trigger_id`. Its event ID is deterministic. [interactive.go:commandEvent,commandEventID]
- An interaction's event ID joins team, channel, user, action ID, and the first non-empty value of `action_ts`, `trigger_id`, the message ts, and the thread ts. A redelivered activation repeats its ID, and each new click gets a new one. [interactive.go:interactionEventID]
- Single-install mode rejects events, commands, and interactions from another team as invalid payloads (`slackapi.ErrInvalidPayload`). Over HTTP, `slackapi.Webhook` answers them with 400. [slack.go:messageEvent,invalidPayload], [interactive.go:commandEvent,interactionEvent]
- Multi-tenant mode drops the bot's own messages when the bot user ID matches. [slack.go:messageEvent]
- In multi-tenant mode, the bot user ID comes from `chat.Install.BotActorID` when it is set, else from `SlackInstall.BotUserID`. [slack.go:resolveInstall,SlackInstall]

### Multi-tenant installs

- An empty tenant or `chat.ErrInstallNotFound` from `InstallStore.Lookup` is an Ignored Event. Any other lookup error is a transport failure. [slack.go:resolveInstall], [doc.go:slack]
- An install credential must be a `SlackInstall` or a non-nil `*SlackInstall` with a `BotToken`. [slack.go:slackCredential,resolveInstall,postToken]
- An outbound post in multi-tenant mode requires a non-empty tenant. [slack.go:postToken]
- `OpenModal` fails in multi-tenant mode. Use `OpenModalForTenant`. [interactive.go:OpenModal]

### Thread IDs and posting

- A Thread ID starts with `slack:v1:`. It needs a team and a channel, and a root unless the thread is a direct message. [slack.go:encodeThreadID,decodeThreadID]
- A channel message thread is rooted at `thread_ts`, else `ts`. A direct message thread has no root. [slack.go:threadIDForEvent]
- Posts accept only `chat.MessageFormatText` and `chat.MessageFormatMarkdown`. [slack.go:slackMessageFields]
- `PostEphemeralMessage` falls back to a DM only on an `ok:false` rejection, and only when `FallbackToDM` is set. With `DisableNativeEphemeral` set and no fallback, it returns `(nil, nil)`. [slack.go:PostEphemeralMessage]
- `PostNative` requires `content.Adapter == "slack"` and a non-nil payload. It does not downgrade the content. [interactive.go:PostNative]
- `OpenModal` requires a `trigger_id` and a non-nil view. `RespondURL` requires a `response_url`. [interactive.go:OpenModal,RespondURL]

### History

- `ReadHistory` uses 100 when `Limit <= 0` and caps `Limit` at 1000. [history.go:ReadHistory,slackDefaultHistoryLimit,slackMaxHistoryLimit]
- `ReadHistory` does no storage, dedupe, or caching. [doc.go:slack]
- `ReadHistory` returns an empty page with no Slack call when the `Before` cursor equals the thread root. [history.go:ReadHistory]
- Retry never sleeps past the caller's context deadline. The default `MaxElapsed` (2 s) also keeps the total backoff sleep under Slack's 3-second ack window. Neither bound covers the time of each HTTP attempt, so under `DispatchSync` a delivery stays inside the window only when the request context or the HTTP client (`Options.Client`) bounds those attempts. [doc.go:slack], [slackapi/retry.go:RetryPolicy.withDefaults,Client.doWithRetry]

## Side Effects

- In single-install mode, `Init` calls `auth.test` through `slackapi.Client.AuthTest` unless all three IDs are configured, and writes the team, bot user, and bot IDs on the `Adapter`. [slack.go:Init]
- Posts call `chat.postMessage`, `chat.postEphemeral`, and `conversations.open` (DM fallback) through the typed `slackapi.Client` methods `PostMessage`, `PostEphemeral`, and `OpenConversation`. [slack.go:PostMessage,PostEphemeralMessage,postEphemeralFallback]
- `PostNative` sends its payload to `chat.postMessage` as `blocks` without decoding it, and `OpenModal` calls `views.open`. Both go through `slackapi.Client.Call`. [interactive.go:PostNative,nativePostPayload,openModalWithToken]
- `ReadHistory` calls `conversations.replies` for threads and `conversations.history` for direct messages. [history.go:ReadHistory]
- `RespondURL` POSTs to the `response_url` through `slackapi.Client.PostResponseURL`, with no bot token. [interactive.go:RespondURL]
- `Listen` opens a Socket Mode websocket through `slackapi.Client.Listen` with the app token. [slack.go:Listen]
- `Webhook` serves inbound HTTP through a `slackapi.Webhook` that verifies signatures. [slack.go:Webhook]
- The delivery handler calls the runtime `chat.DispatchFunc` for each normalized event. [slack.go:deliveryHandler]
- Multi-tenant mode calls `InstallStore.Lookup` for each supported delivery with a team ID (an `event_callback` envelope, a slash command, or a `block_actions` interaction), and for each outbound call that needs a bot token. Ignored delivery types and `RespondURL`, which sends no token, do no lookup. The adapter does not cache install records. [slack.go:normalizeDelivery,normalizeEvent,resolveInstall,postToken], [interactive.go:normalizeCommand,normalizeInteraction,RespondURL], [doc.go:slack]
- Retry sleeps, observation events (`chat.ObsAdapterCall`, `chat.ObsRateLimit`), and throttle logs come from `slackapi`. `New` passes `Client`, `RetryPolicy`, `Observer`, and `Logger` to `slackapi.New`. [slack.go:New], [doc.go:slack]
- The package starts no goroutines and holds no mutable package-level state. [slack.go:New]

## Verification

- Run: `go test ./adapters/slack/...`
