# How To Handle Interactive Components

Button clicks and menu selections go to their own hook, `OnInteraction`.
Like messages, they are deduped and serialized per thread
([ADR 0004](../adr/0004-interactive-components.md)). This guide covers Slack
buttons and menus on messages: Block Kit content posted to a channel,
thread, or direct message.

There is no cross-platform card DSL. Portable posts stay plain text or
portable Markdown; Block Kit goes to Slack as an opaque payload through
typed adapter access.

## Configure Slack

In your Slack app dashboard, under **Interactivity & Shortcuts**, enable
interactivity and set the **Request URL** to your existing webhook:

```text
https://YOUR_PUBLIC_HOST/webhooks/slack
```

## Post Something Clickable

Block Kit content is `NativeContent`, posted through the Slack adapter's
`NativeContentPoster` capability:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if !ok {
	return errors.New("slack adapter is not registered")
}

ref, err := slackAdapter.ValidateThreadID(ev.Thread.ID())
if err != nil {
	return err
}

sent, err := slackAdapter.PostNative(ctx, ref, chat.NativeContent{
	Adapter: "slack",
	Payload: []any{
		map[string]any{
			"type": "actions",
			"elements": []any{
				map[string]any{
					"type":      "button",
					"action_id": "approve",
					"text":      map[string]any{"type": "plain_text", "text": "Approve"},
				},
			},
		},
	},
})
if err != nil {
	return err
}
_ = sent // sent.ID identifies the posted message, like portable posting
```

The runtime treats the payload as opaque: the adapter neither validates nor
translates it. A `NativeContent` whose `Adapter` does not match the target
adapter is an error, never a silent downgrade to a portable post.

## Handle The Click

```go
bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
	switch ev.Interaction.ActionID {
	case "approve":
		_, err := ev.Thread.Post(ctx, chat.Text(
			"Approved (by user " + ev.Interaction.Actor.ID + ")",
		))
		return err
	default:
		return nil
	}
})
```

`ev.Interaction.Kind` is `chat.InteractionBlockAction`. The component's value
is on the event:

- `ev.Interaction.Value` holds a button's `value`, or the selected option of
  a single-select component (`static_select`, `external_select`, `overflow`,
  `radio_buttons`).
- `ev.Interaction.Values` holds the selected options of a multi-select
  component (`multi_static_select`, `checkboxes`).

A menu whose options share one `action_id` routes on `ActionID` and switches
on `Value`:

```go
case "pick-env":
	_, err := ev.Thread.Post(ctx, chat.Text("Deploying to "+ev.Interaction.Value))
	return err
```

`ev.Interaction.Raw` keeps the full Slack payload, including `response_url`,
`trigger_id`, and view state. Its concrete type is unexported; pass it to the
adapter methods that accept it (`RespondURL`, `OpenModalFromRaw`).

Every click is a separate event. Deduplication keys on Slack's per-click
`action_ts`, so a user who clicks the same button twice reaches
`OnInteraction` twice, while a redelivery of one click is still deduped
within `DedupeTTL`.

### Mentioning The User

`ev.Interaction.Actor` carries the Slack user ID, not a display name; the
payload does not include one. `chat.Text` posts with Slack formatting turned
off, so `<@USERID>` shows up literally. To credit the user in plain text,
look up their display name through the Slack API. For a real, clickable
mention, post Block Kit content with an `mrkdwn` text element containing
`<@USERID>`.

### Acknowledging In Time

Under the default `DispatchSync`, the adapter sends Slack its empty 2xx only
after your handler returns, so a slow handler can miss Slack's 3-second
budget. Enable [deferred dispatch](deferred-dispatch.md), usually with
`chat.ConcurrencyQueue`, so the handler runs after the acknowledgement.

## Open A Modal

Pass the event's `Raw` value; the adapter finds the `trigger_id` and calls
`views.open`:

```go
err := slackAdapter.OpenModalFromRaw(ctx, ev.Interaction.Raw, modalView)
```

In multi-tenant mode (`InstallStore` configured), use the tenant-aware
variant so the right workspace token is used:

```go
err := slackAdapter.OpenModalForTenantFromRaw(ctx, ev.Event.Tenant, ev.Interaction.Raw, modalView)
```

`ev.Command.Raw` from a slash-command handler works the same way. If you
already hold a `trigger_id` string, use `OpenModal` or `OpenModalForTenant`.

Slack invalidates a `trigger_id` after 3 seconds, so open modals right away.
Under `chat.ConcurrencyQueue`, an interaction that queues behind a
long-running handler waits for the thread lock *before* your handler runs;
by then its `trigger_id` may have expired, and the modal fails to open no
matter how fast your handler is. Keep handlers on modal-bearing threads fast,
or accept that modals from contended threads may fail.

## Respond Via response_url

To reply only to the person who clicked:

```go
err := slackAdapter.RespondURL(ctx, ev.Interaction.Raw, chat.Text("Working on it."))
```

## Limits

- **Clicks inside modals are not handled.** `block_actions` raised inside a
  modal view have no channel or message to anchor a thread, so the adapter
  rejects them before routing.
- **Modal submissions are not delivered.** Slack expects the answer to a
  `view_submission` in the webhook's HTTP response body, which does not fit
  ack-then-work. The adapter acknowledges and drops `view_submission`
  payloads, so your code cannot read submitted values today.
