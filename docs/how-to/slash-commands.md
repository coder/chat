# How To Handle Slash Commands

Slash commands such as `/deploy` go to their own hook, `OnCommand`, not to
the message hooks. They still get the same dedupe and thread locking as
messages ([ADR 0003](../adr/0003-slash-commands.md)). Slack is the only
adapter with slash commands.

## Configure Slack

In your Slack app dashboard, under **Slash Commands**, create the command
(for example `/deploy`) and set its **Request URL** to the same webhook you
already mounted:

```text
https://YOUR_PUBLIC_HOST/webhooks/slack
```

The Slack adapter acknowledges each command with an empty 2xx. Under the
default synchronous dispatch, that happens after your handler returns; see
[Long-running commands](#long-running-commands) to stay inside Slack's
3-second budget.

## Register The Handler

Respond through the `response_url` Slack includes with every slash command,
reached via the Slack adapter's `RespondURL`:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if !ok {
	return errors.New("slack adapter is not registered")
}

bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
	switch ev.Command.Name {
	case "/deploy":
		return slackAdapter.RespondURL(ctx, ev.Command.Raw, chat.Text(
			"Deploying "+strings.Join(ev.Command.Args, " "),
		))
	default:
		return nil
	}
})
```

Why not `ev.Thread.Post`? A command typed in a channel has no parent message,
so its thread is rooted at the channel itself, and a threaded post to that
root fails. `RespondURL` answers the command in place instead. The adapter
sends that response as ephemeral, so only the person who ran the command
sees it. In a direct message with the bot, `ev.Thread.Post` works normally.

Fields on `ev.Command`:

- `Name` — the command, including the slash (`/deploy`).
- `Text` — the raw argument text after the command name.
- `Args` — an advisory whitespace split of `Text`.
- `Actor` — the human who invoked the command.
- `Raw` — the platform escape hatch, preserving Slack's `response_url` and
  `trigger_id` for native responses (see the
  [interactive components guide](interactive-components.md)).

## Routing Rules

- A command always goes to `OnCommand`, even in a subscribed thread. It
  never reaches `OnSubscribedMessage`.
- A command does not subscribe its thread.
- `OnCommand` holds one handler. Registering again replaces it. With no
  handler set, commands are acknowledged and ignored.
- A channel command locks the channel-rooted thread, which is separate from
  every message thread in that channel. Do not expect a channel command to
  wait for message handlers, or the reverse. In a direct message, commands
  and messages share one thread and one lock.

## Long-Running Commands

Under the default `DispatchSync`, your handler runs before Slack gets its
acknowledgement, so slow work risks Slack's 3-second timeout. Enable
[deferred dispatch](deferred-dispatch.md) so the acknowledgement no longer
waits on your handler. Slack keeps a command's `response_url` valid for 30
minutes, so a deferred handler can finish its work and then call
`RespondURL`.

Consider `chat.ConcurrencyQueue` too, so commands and clicks that arrive
mid-work wait instead of being dropped. One caveat: every command in a
channel shares the channel-rooted thread, and the queue keeps only the newest
waiting event per thread. While one command runs, later commands in that
channel — from other users, or for other command names — replace each other,
and all but the newest are acknowledged without reaching `OnCommand`.

If your bot must handle several channel commands at once, keep the handler
fast: acknowledge the command, hand the real work to your own queue keyed by
`response_url`, and return.
