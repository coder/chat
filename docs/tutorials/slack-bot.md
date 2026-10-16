# Tutorial: Your First Slack Bot

In this tutorial you take Chat SDK Go from zero to a running Slack bot that
replies when you mention it. You will create a Slack app, run the bundled
hello-world example against your workspace, and then make your first change to
the bot's behavior.

The bot connects to Slack over Socket Mode: it opens a websocket to Slack, and
Slack sends events over it. So you do not need a public URL or a tunnel.
Slack's own Bolt guides also recommend Socket Mode to start with. The
section [Use HTTP Instead Of Socket Mode](#use-http-instead-of-socket-mode)
shows the other transport, the Events API over HTTP.

Expect the whole tutorial to take under 30 minutes.

## What You Need

- Go 1.26.3 or newer (`go version`).
- A Slack workspace where you are allowed to create and install apps.

You do not need Docker, Redis, Postgres, or any other service: this tutorial
uses the in-memory state backend.

## Step 1: Clone And Build

Clone the repository and make sure the example compiles:

```sh
git clone https://github.com/coder/chat.git
cd chat
go build ./examples/slack-hello-world
```

If `go build` succeeds, your toolchain is ready.

The example you are about to run lives in
[`examples/slack-hello-world/main.go`](../../examples/slack-hello-world/main.go).
Its whole job is:

1. Build a Slack adapter from an app-level token and a bot token.
2. Build a `chat.Chat` runtime with in-memory state and that adapter.
3. Register one handler: when the bot is mentioned, reply with
   `**hello** _world_` in the same thread.
4. Call `Chat.Listen`, which connects to Slack over Socket Mode and receives
   events.
5. When you stop the bot, call `Chat.Shutdown`. It stops acking new events,
   and then closes the connection.

## Step 2: Create A Slack App

Open the Slack app dashboard:

<https://api.slack.com/apps>

Click **Create New App**, choose **From a manifest**, and pick your workspace.
In the **JSON** tab, replace the example with this manifest:

```json
{
  "display_information": {
    "name": "Hello Bot"
  },
  "features": {
    "app_home": {
      "home_tab_enabled": false,
      "messages_tab_enabled": true,
      "messages_tab_read_only_enabled": false
    },
    "bot_user": {
      "display_name": "Hello Bot",
      "always_online": false
    }
  },
  "oauth_config": {
    "scopes": {
      "bot": ["chat:write", "app_mentions:read", "im:history"]
    }
  },
  "settings": {
    "event_subscriptions": {
      "bot_events": ["app_mention", "message.im"]
    },
    "org_deploy_enabled": false,
    "socket_mode_enabled": true,
    "token_rotation_enabled": false
  }
}
```

Click **Next**, check the summary, and click **Create**. The manifest sets up
the app like this:

| Setting | Why the bot needs it |
| --- | --- |
| `chat:write` scope | Post the reply with `chat.postMessage`. |
| `app_mentions:read` scope, `app_mention` event | Receive an event when the bot is mentioned. |
| `im:history` scope, `message.im` event | Receive direct messages. Mentions in channels work without them. |
| `socket_mode_enabled` | Turn on Socket Mode, so Slack sends events over the bot's websocket and not to a request URL. |
| `messages_tab_enabled`, `messages_tab_read_only_enabled` | Let users send direct messages to the bot from its **Messages** tab. |

The package `slackapi` has a `Manifest` type that encodes this format, if you
want to make manifests in Go.

Then, in **OAuth & Permissions**, click **Install to Workspace** and approve
the app.

## Step 3: Collect Credentials

You need two tokens. Treat both like passwords.

1. In **OAuth & Permissions**, copy the **Bot User OAuth Token**. It starts
   with `xoxb-`. This becomes `SLACK_BOT_TOKEN`.
2. In **Basic Information**, under **App-Level Tokens**, click **Generate Token
   and Scopes**. Give the token a name, such as `socket-mode`, add the
   `connections:write` scope, and click **Generate**. Copy the token. It
   starts with `xapp-`. This becomes `SLACK_APP_TOKEN`.

The app-level token lets the bot open Socket Mode connections. Check that
**Enable Socket Mode** is on in **Socket Mode**. The manifest turned it on.

## Step 4: Run The Bot

From the repository root:

```sh
export SLACK_APP_TOKEN="xapp-..."
export SLACK_BOT_TOKEN="xoxb-..."
export CHAT_DEMO_IN_MEMORY_STATE=1

go run ./examples/slack-hello-world
```

`CHAT_DEMO_IN_MEMORY_STATE=1` is a deliberate speed bump: it acknowledges that
in-memory state is lost on restart, which is fine for this tutorial and wrong
for production. The [state backend guide](../how-to/choose-a-state-backend.md)
covers the durable options.

On startup the adapter calls Slack's `auth.test` with your bot token to
discover the bot's own identity. If the token is wrong you find out now, not
on the first message. Then the bot opens a Socket Mode connection with your
app-level token. When the bot is connected you should see a log line like:

```text
2026/09/23 14:00:00 INFO slack socket mode connected adapter=slack host=applink-1 num_connections=1 app_id=A0123456789
```

If Slack rejects the app-level token, the bot stops with an error such as
`invalid_auth`. Check Step 3. If you turn Socket Mode off while the bot runs,
Slack disconnects it with the reason `link_disabled`, and the bot stops.

## Step 5: Talk To Your Bot

In Slack, invite the bot to a channel and mention it:

```text
/invite @your-bot
@your-bot hello
```

The bot replies in a thread on your message with `**hello** _world_`,
rendered with bold and italics. You have a running Slack bot. You can also
send a direct message to the bot.

If Slack shows **"Sending messages to this app has been turned off"** in the
app's DM, the **Messages** tab is still read-only. In **App Home**, under
**Show Tabs**, allow users to send messages from the **Messages** tab.

## Step 6: Make It Yours

The bot currently answers every mention but forgets the conversation
immediately. Make it stay in the conversation.

First, let Slack deliver unmentioned channel messages to your bot. Without
this, only mentions reach it:

1. In **OAuth & Permissions**, add the `channels:history` bot scope.
2. In **Event Subscriptions**, under **Subscribe to bot events**, add the
   `message.channels` bot event, and save the changes.
3. Reinstall the app from **OAuth & Permissions**.

(You can skip this and test the follow-up flow in a direct message instead.)

Then open `examples/slack-hello-world/main.go` and replace the
`OnNewMention` handler registration with:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	if err := ev.Thread.Subscribe(ctx); err != nil {
		return err
	}
	_, err := ev.Thread.Post(ctx, chat.Markdown("**hello** _world_"))
	return err
})

bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
	_, err := ev.Thread.Post(ctx, chat.Text("You said: "+ev.Message.Text))
	return err
})
```

Restart the bot (`Ctrl-C`, then `go run ./examples/slack-hello-world` again)
and mention it once more. From then on, follow-up messages in that thread get
echoed back, with no mention required. (If you type several messages faster
than the bot replies, some may be skipped: the default concurrency strategy
drops events that arrive while the thread's previous event is still being
handled.) Two things to notice:

- Replying never subscribes a thread. `Thread.Subscribe` is always an explicit
  decision, and it lasts until you call `Thread.Unsubscribe`.
- Subscriptions live in runtime state. Because this example uses in-memory
  state, restarting the bot forgets them.

## Use HTTP Instead Of Socket Mode

Over HTTP, Slack sends each event to a public request URL of your bot, through
the Events API. Slack's Bolt guides recommend HTTP for apps that run on a
hosting service, apps in large Slack organizations, and apps for the Slack
Marketplace. Both transports use the same adapter pipeline, so your handlers do
not change.

The example serves HTTP when `SLACK_SIGNING_SECRET` is set. It can run both
transports at the same time. To use HTTP:

1. In **Basic Information**, under **App Credentials**, copy the **Signing
   Secret**. This becomes `SLACK_SIGNING_SECRET`.
2. Run the bot with the signing secret. Leave out `SLACK_APP_TOKEN` to use only
   HTTP:

   ```sh
   export SLACK_SIGNING_SECRET="..."
   export SLACK_BOT_TOKEN="xoxb-..."
   export CHAT_DEMO_IN_MEMORY_STATE=1
   export PORT=8080

   go run ./examples/slack-hello-world
   ```

   The bot serves the Slack webhook on
   `http://localhost:8080/webhooks/slack`, and logs
   `INFO serving Slack webhooks addr=:8080`.
3. Expose port 8080 over public HTTPS, so that Slack can reach your machine.
   In a second terminal, use a tunnel such as
   [Tailscale Funnel](https://tailscale.com/kb/1223/funnel), `ngrok`, or
   `cloudflared`. With ngrok:

   ```sh
   ngrok http 8080
   ```

   With Tailscale Funnel (Funnel must be enabled for your tailnet; `tailscale
   serve` does not work, because it is visible only inside your tailnet):

   ```sh
   tailscale funnel --bg --https=443 localhost:8080
   tailscale funnel status
   ```

   When you finish, run `tailscale funnel reset` to turn the public endpoint
   off. Either way you get a public HTTPS URL such as
   `https://your-host.example.com`. Keep the tunnel running.
4. In **Socket Mode**, turn **Enable Socket Mode** off. While it is on, Slack
   sends events over Socket Mode and not to the request URL.
5. In **Event Subscriptions**, set the **Request URL** to:

   ```text
   https://YOUR_PUBLIC_HOST/webhooks/slack
   ```

   Slack immediately sends a `url_verification` challenge. The Slack adapter
   answers it automatically; the dashboard should show **Verified** within a
   few seconds. If it does not, check that the bot and the tunnel are both
   still running. Save the changes. If Slack prompts you to reinstall the app,
   do it from **OAuth & Permissions**.

## Where To Go Next

- [Choose a state backend](../how-to/choose-a-state-backend.md) to keep
  subscriptions, dedupe marks, and locks across restarts.
- [Defer long-running work](../how-to/deferred-dispatch.md) before your
  handlers start doing anything slower than a quick reply.
- [Handle slash commands](../how-to/slash-commands.md) and
  [interactive components](../how-to/interactive-components.md).
- Read the [architecture explanation](../explanation.md) to understand the
  model behind what you just built.

Slack's own documentation for the settings used here:

- App dashboard: <https://api.slack.com/apps>
- App manifests: <https://docs.slack.dev/app-manifests>
- Socket Mode: <https://docs.slack.dev/apis/events-api/using-socket-mode>
- HTTP and Socket Mode compared:
  <https://docs.slack.dev/apis/events-api/comparing-http-socket-mode>
- Events API request URLs:
  <https://docs.slack.dev/apis/events-api/using-http-request-urls>
- Events: [`app_mention`](https://docs.slack.dev/reference/events/app_mention/),
  [`message.channels`](https://docs.slack.dev/reference/events/message.channels),
  [`message.im`](https://docs.slack.dev/reference/events/message.im)
- App Home Messages tab: <https://docs.slack.dev/surfaces/app-home>
- Bot tokens (`xoxb-`) and app-level tokens (`xapp-`):
  <https://docs.slack.dev/authentication/tokens>
- Scopes: [`chat:write`](https://docs.slack.dev/reference/scopes/chat.write);
  [`conversations.open`](https://docs.slack.dev/reference/methods/conversations.open/)
  lists the scopes DM fallback needs
