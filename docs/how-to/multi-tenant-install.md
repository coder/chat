# How To Install Into Multiple Workspaces (Multi-Tenant)

By default an adapter serves one Slack workspace or one Linear organization,
with credentials passed in its options. Multi-tenant mode lets one deployment
serve many: the adapter looks up each tenant's credentials when a webhook
arrives ([ADR 0006](../adr/0006-multi-tenant-install.md)).

The split of work is deliberate:

- **The adapter looks up credentials.** You implement a `chat.InstallStore`;
  the adapter calls it with the tenant from each webhook (the Slack team ID
  or the Linear organization ID).
- **You own the OAuth flow.** The install redirect, the callback route, the
  token exchange, and the install database are ordinary routes and storage in
  your application; the runtime does not mount them. Account linking and
  login flows for your users are yours too.

## Implement An InstallStore

```go
type InstallStore interface {
	Lookup(ctx context.Context, adapter, tenant string) (Install, error)
}
```

Return `chat.ErrInstallNotFound` for a tenant you do not know. The adapter
acknowledges and ignores that tenant's events, because an uninstalled
workspace is not an error. Any other error becomes a 5xx, so the platform
retries.

```go
type installStore struct{ db *sql.DB }

func (s *installStore) Lookup(ctx context.Context, adapter, tenant string) (chat.Install, error) {
	row, err := s.queryInstall(ctx, adapter, tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return chat.Install{}, chat.ErrInstallNotFound
	}
	if err != nil {
		return chat.Install{}, err
	}
	return chat.Install{
		Tenant: tenant,
		Credential: slack.SlackInstall{
			BotToken:  row.BotToken,
			BotUserID: row.BotUserID,
		},
	}, nil
}
```

The `Credential` field is adapter-specific:

- Slack: `slack.SlackInstall{BotToken, BotUserID}`
- Linear: `linear.LinearInstall{WebhookSecret, ClientCredentials, AccessToken, BotUserID}`
  (either client credentials for token exchange or a pre-exchanged access
  token)

### Store The Bot User ID

In multi-tenant mode the adapter cannot discover the bot's own identity per
install, so store it on every install record. Without it, the bot cannot
recognize its own messages:

- Slack: without `SlackInstall.BotUserID` (or `Install.BotActorID`),
  self-message filtering has no identity to match — if you subscribe to
  `message.channels` or `message.im`, the bot's own posts re-enter routing
  and a subscribed thread can loop (reply triggers `OnSubscribedMessage`,
  which replies again). Slack's `oauth.v2.access` response includes the
  `bot_user_id`; store it on the install record.
- Linear: without `LinearInstall.BotUserID` (or `Install.BotActorID`),
  mention detection and self-comment filtering for generic comment
  participation have nothing to match — the app never sees its own
  @-mentions as mentions, and may route its own comments back to itself.
  Capture the app user ID during your OAuth flow (e.g. query `viewer { id }`
  with the freshly exchanged token) and store it.

## Construct The Adapter In Multi-Tenant Mode

`InstallStore` is mutually exclusive with the single-install credential
options:

```go
slackAdapter, err := slack.New(ctx, slack.Options{
	SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"), // shared across installs
	InstallStore:  store,
})
```

```go
linearAdapter, err := linear.New(ctx, linear.Options{
	InstallStore: store, // per-install webhook secrets and credentials
})
```

### Treat Linear Tenants As Untrusted

The two adapters verify requests at different times:

- **Slack** has one signing secret for the whole app. The adapter verifies
  the signature before it calls your store, so the tenant your store sees
  came from a verified request.
- **Linear** has a webhook secret per install. The adapter must read the
  organization ID from the **unverified** body and call `Lookup` to get the
  secret it verifies with.

So treat the Linear tenant argument as untrusted input. Keep `Lookup` a
cheap indexed read, do not let unknown tenants trigger expensive work, and
return `ErrInstallNotFound`, not an error, for tenants you do not know.

## Wire Up Your OAuth Flow

For Slack, the part you build looks like this:

1. Mount `/slack/install` — redirect to Slack's OAuth authorize URL with your
   client ID and scopes.
2. Mount `/slack/oauth/callback` — exchange the code via `oauth.v2.access`,
   then store the returned team ID, bot token, and bot user ID in your
   install database.
3. Your `InstallStore.Lookup` reads that row.

To handle an uninstall, delete the row. Later events from that tenant
resolve to `ErrInstallNotFound` and are acknowledged and ignored.

## What Stays Tenant-Correct Automatically

Thread IDs, actors, and dedupe keys all include the tenant, so two
workspaces never collide in runtime state.

Proactive posts work across installs with no extra code. `bot.Thread(ctx,
threadID)` decodes and validates a stored thread ID without calling your
install store; the credential lookup happens when the handle posts. So a
successful `bot.Thread` call does not prove the tenant is still installed.
An uninstalled tenant shows up as an error from the post.
