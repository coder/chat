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

The Slack adapter acknowledges the command with an empty 2xx inside Slack's
3-second budget; your handler decides what to post.

## Register The Handler

```go
bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
	switch ev.Command.Name {
	case "/deploy":
		_, err := ev.Thread.Post(ctx, chat.Text(
			"Deploying " + strings.Join(ev.Command.Args, " "),
		))
		return err
	default:
		return nil
	}
})
```

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
- Commands are deduped by event identity and take the thread lock, so a
  command racing a message on the same thread serializes.

## Long-Running Commands

`ev.Thread.Post` posts a regular thread message. If the command kicks off
slow work, enable [deferred dispatch](deferred-dispatch.md) so the ack happens
before your handler runs, and consider `chat.ConcurrencyQueue` so mid-work
commands and clicks queue instead of dropping. To respond through Slack's
`response_url` instead of posting to the thread, use the Slack adapter's
`RespondURL` via typed adapter access:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if ok {
	err := slackAdapter.RespondURL(ctx, ev.Command.Raw, chat.Text("On it."))
	if err != nil {
		return err
	}
}
```
