# Tutorial: Your First Slack Bot

In this tutorial you take Chat SDK Go from zero to a running Slack bot that
replies when you mention it. You will create a Slack app, run the bundled
hello-world example against your workspace, and then make your first change to
the bot's behavior.

Expect the whole tutorial to take under 30 minutes.

## What You Need

- Go 1.26.3 or newer (`go version`).
- A Slack workspace where you are allowed to create and install apps.
- A way to expose local port 8080 to the public internet over HTTPS, such as
  [Tailscale Funnel](https://tailscale.com/kb/1223/funnel), `ngrok`, or
  `cloudflared`. Slack delivers events by calling your bot over HTTPS.

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

1. Build a Slack adapter from a signing secret and a bot token.
2. Build a `chat.Chat` runtime with in-memory state and that adapter.
3. Register one handler: when the bot is mentioned, reply with
   `**hello** _world_` in the same thread.
4. Serve the Slack webhook on `http://localhost:8080/webhooks/slack`.

## Step 2: Create A Slack App

Open the Slack app dashboard and create a new app (from scratch) in your
workspace:

<https://api.slack.com/apps>

Then configure it:

1. In **OAuth & Permissions**, under **Bot Token Scopes**, add:

   | Scope | Why the bot needs it |
   | --- | --- |
   | `chat:write` | Post the reply with `chat.postMessage`. |
   | `app_mentions:read` | Receive `app_mention` events when the bot is mentioned. |
   | `im:history` | Only needed if you also want direct messages to reach the bot. |

2. In **App Home**, under **Show Tabs**, enable the **Messages Tab** and allow
   users to send messages from it (Slack labels this "Allow users to send
   Slash commands and messages from the messages tab"). This matters only for
   direct messages; mentions in channels work without it.

3. In **OAuth & Permissions**, click **Install to Workspace** and approve the
   app.

## Step 3: Collect Credentials

You need two secrets. Treat both like passwords.

1. In **OAuth & Permissions**, copy the **Bot User OAuth Token**. It starts
   with `xoxb-`. This becomes `SLACK_BOT_TOKEN`.
2. In **Basic Information**, under **App Credentials**, copy the
   **Signing Secret**. This becomes `SLACK_SIGNING_SECRET`.

## Step 4: Run The Bot

From the repository root:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export CHAT_DEMO_IN_MEMORY_STATE=1
export PORT=8080

go run ./examples/slack-hello-world
```

`CHAT_DEMO_IN_MEMORY_STATE=1` is a deliberate speed bump: it acknowledges that
in-memory state is lost on restart, which is fine for this tutorial and wrong
for production. The [state backend guide](../how-to/choose-a-state-backend.md)
covers the durable options.

On startup the adapter calls Slack's `auth.test` with your bot token to
discover the bot's own identity. If the token is wrong you find out now, not
on the first message. When the bot is up you should see a log line like:

```text
level=INFO msg=listening addr=:8080
```

## Step 5: Expose The Bot To Slack

Slack must be able to reach your machine over public HTTPS. In a second
terminal, expose port 8080 with your tunnel of choice. For example, with
Tailscale Funnel:

```sh
tailscale funnel --bg --https=443 localhost:8080
tailscale funnel status
```

or with ngrok:

```sh
ngrok http 8080
```

Either way you end up with a public HTTPS URL such as
`https://your-host.example.com`. Keep the tunnel running.

## Step 6: Subscribe To Events

Back in the Slack app dashboard:

1. In **Event Subscriptions**, enable events.
2. Set the **Request URL** to:

   ```text
   https://YOUR_PUBLIC_HOST/webhooks/slack
   ```

   Slack immediately sends a `url_verification` challenge. The Slack adapter
   answers it automatically; the dashboard should show **Verified** within a
   few seconds. If it does not, check that the bot from Step 4 and the tunnel
   from Step 5 are both still running.

3. Under **Subscribe to bot events**, add `app_mention` (and `message.im` if
   you added `im:history` in Step 2).
4. Save changes. If Slack prompts you to reinstall the app, do it from
   **OAuth & Permissions**.

## Step 7: Talk To Your Bot

In Slack, invite the bot to a channel and mention it:

```text
/invite @your-bot
@your-bot hello
```

The bot replies in a thread on your message with `**hello** _world_`,
rendered with bold and italics. You have a running Slack bot.

## Step 8: Make It Yours

The bot currently answers every mention but forgets the conversation
immediately. Make it stay in the conversation.

First, let Slack deliver unmentioned channel messages to your bot — without
this, only mentions ever reach it:

1. In **OAuth & Permissions**, add the `channels:history` bot scope.
2. In **Event Subscriptions**, add the `message.channels` bot event.
3. Reinstall the app from **OAuth & Permissions**.

(If you set up `message.im` in Step 2, you can skip this and test the
follow-up flow in a direct message instead.)

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
echoed back — no mention required. (If you type several messages faster than
the bot replies, some may be skipped: the default concurrency strategy drops
events that arrive while the thread's previous event is still being handled.)
Two things to notice:

- Replying never subscribes a thread. `Thread.Subscribe` is always an explicit
  decision, and it lasts until you call `Thread.Unsubscribe`.
- Subscriptions live in runtime state. Because this example uses in-memory
  state, restarting the bot forgets them.

## Where To Go Next

- [Choose a state backend](../how-to/choose-a-state-backend.md) to keep
  subscriptions, dedupe marks, and locks across restarts.
- [Defer long-running work](../how-to/deferred-dispatch.md) before your
  handlers start doing anything slower than a quick reply.
- [Handle slash commands](../how-to/slash-commands.md) and
  [interactive components](../how-to/interactive-components.md).
- Read the [architecture explanation](../explanation.md) to understand the
  model behind what you just built.

The example's own [README](../../examples/slack-hello-world/README.md) repeats
the Slack app setup with more detail (including Tailscale Funnel specifics)
if you need to revisit it later.
