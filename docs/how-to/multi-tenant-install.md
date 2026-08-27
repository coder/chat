# How To Install Into Multiple Workspaces (Multi-Tenant)

By default an adapter is single-install: one Slack workspace or one Linear
organization, with credentials passed directly in the adapter options.
Multi-tenant mode is opt-in and lets one deployment serve many installs by
resolving per-tenant credentials at webhook time (see
[ADR 0006](../adr/0006-multi-tenant-install.md)).

The boundary is deliberate:

- **The runtime resolves credentials.** You provide a
  `chat.InstallStore` and the adapter calls it with the platform tenant
  (Slack team ID, Linear organization ID) extracted from each webhook.
  Verification timing differs per adapter — see the caution below.
- **You own the OAuth web flow.** The authorize redirect, callback route,
  token exchange, and install database are ordinary application HTTP routes
  and storage — the runtime does not mount them. App-user account linking and
  login flows stay app-owned too.

## Implement An InstallStore

```go
type InstallStore interface {
	Lookup(ctx context.Context, adapter, tenant string) (Install, error)
}
```

Return `chat.ErrInstallNotFound` for tenants you do not know: the adapter
acknowledges the event and ignores it (an uninstalled workspace is not an
error). Any other error is treated as a transport failure and surfaces as a
5xx so the platform retries.

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

In multi-tenant mode the adapter does not discover the app's identity per
install, so treat the per-install bot identity as **required** on both
adapters:

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

For Slack, the signing secret is app-level and shared; signature verification
happens before any store lookup, so the tenant your store sees came from a
verified request. For Linear, the webhook secret is itself per-install, so
the adapter must parse the organization ID from the **unverified** body and
call `Lookup` first to fetch the secret it verifies with. Treat the Linear
tenant argument as untrusted routing input: keep `Lookup` a cheap indexed
read, do not let unknown tenants trigger expensive work, and rely on
`ErrInstallNotFound` (not errors) for tenants you do not know.

## Wire Up Your OAuth Flow

Sketch of the app-owned part for Slack:

1. Mount `/slack/install` — redirect to Slack's OAuth authorize URL with your
   client ID and scopes.
2. Mount `/slack/oauth/callback` — exchange the code via `oauth.v2.access`,
   then store the returned team ID, bot token, and bot user ID in your
   install database.
3. Your `InstallStore.Lookup` reads that row.

Uninstalls: delete the row; subsequent events from that tenant resolve to
`ErrInstallNotFound` and are acknowledged and ignored.

## What Stays Tenant-Correct Automatically

Thread IDs, actors, and dedupe keys all carry the platform tenant, so two
workspaces never collide in runtime state. Thread handle reconstruction
(`bot.Thread(ctx, threadID)`) decodes and validates the stored ID without
touching the install store; the credential lookup for the stored tenant
happens when the reconstructed handle actually posts. Proactive posts work
across installs without extra plumbing — but a successful `bot.Thread` call
is not proof the tenant is still installed; an uninstalled tenant surfaces as
an error from the post.
