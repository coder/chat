# How To Handle Slash Commands

A slash command is a Command Event, not a message: it rides the same dispatch
spine (dedupe, thread lock, tenant scoping) but routes to its own single-slot
hook, `OnCommand`, regardless of thread subscription state (see
[ADR 0003](../adr/0003-slash-commands.md)). This slice covers Slack slash
commands.

## Configure Slack

In your Slack app dashboard, under **Slash Commands**, create the command
(for example `/deploy`) and set its **Request URL** to the same webhook you
already mounted:

```text
https://YOUR_PUBLIC_HOST/webhooks/slack
```

The Slack adapter acknowledges the command with an empty 2xx; under the
default synchronous dispatch mode that happens after your handler returns
(see [Long-Running Commands](#long-running-commands) for staying inside
Slack's 3-second budget).

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

Why not `ev.Thread.Post`? A slash command in a channel carries no message
timestamp, so its thread is rooted at the channel itself — there is no thread
to post into, and a regular threaded post to that synthetic root fails.
`RespondURL` is the channel-command response path (Slack renders it in place,
ephemeral by default). In a direct-message conversation with the bot,
`ev.Thread.Post` works normally.

What you get on `ev.Command`:

- `Name` — the command, including the slash (`/deploy`).
- `Text` — the raw argument text after the command name.
- `Args` — an advisory whitespace split of `Text`.
- `Actor` — the human who invoked the command.
- `Raw` — the platform escape hatch, preserving Slack's `response_url` and
  `trigger_id` for native responses (see the
  [interactive components guide](interactive-components.md)).

## Routing Rules Worth Knowing

- Command-ness wins: a command typed in a subscribed thread routes to
  `OnCommand`, never to `OnSubscribedMessage`.
- A command does not auto-subscribe its thread.
- `OnCommand` is single-slot like the message hooks: registering again
  atomically replaces the handler, and an unset handler is a no-op that still
  acknowledges the platform.
- Commands are deduped by event identity and take a thread lock on the
  command's own thread scope. In a channel that scope is the synthetic
  channel-rooted thread, which is distinct from every message thread's scope —
  so do not rely on a channel command serializing with message handlers.
  Direct-message commands share the DM conversation's thread scope.

## Long-Running Commands

Under the default `DispatchSync` mode your handler runs before the platform
acknowledgement, so slow command work risks Slack's 3-second timeout. Enable
[deferred dispatch](deferred-dispatch.md) so the acknowledgement no longer
waits on your handler, and consider `chat.ConcurrencyQueue` so mid-work
commands and clicks queue instead of dropping. Slack keeps a command's `response_url`
valid for 30 minutes, so a deferred handler can finish its work and respond
through `RespondURL` afterwards.

One coalescing caveat: because every channel command shares the synthetic
channel-rooted scope, the queue keeps only the single most recent pending
command per channel — while one command runs, *independent* commands from
other users (or other command names) in the same channel supersede each
other, and all but the newest are acknowledged without invoking `OnCommand`.
If your bot expects concurrent channel commands, keep command handlers fast
(ack the command, hand real work to your own queue keyed by `response_url`)
rather than holding the thread lock through long work.
