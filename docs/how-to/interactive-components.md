# How To Handle Interactive Components

Buttons and menus are Interaction Events: normalized events with their own
single-slot hook, `OnInteraction`, riding the same dispatch spine as messages
(see [ADR 0004](../adr/0004-interactive-components.md)). This slice covers
Slack `block_actions` — button clicks and menu selections.

There is deliberately no cross-platform card DSL. Portable posting stays plain
text and portable Markdown; platform-native rich content (Block Kit) is posted
as an opaque payload through typed adapter access.

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
```

The payload is opaque to the runtime: the adapter neither validates nor
translates it. A `NativeContent` whose `Adapter` does not match the target
adapter is an error, never a silent portable downgrade.

## Handle The Click

```go
bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
	switch ev.Interaction.ActionID {
	case "approve":
		_, err := ev.Thread.Post(ctx, chat.Text(
			"Approved by " + ev.Interaction.Actor.Name,
		))
		return err
	default:
		return nil
	}
})
```

`ev.Interaction.Kind` is `chat.InteractionBlockAction` for this slice, and
`ev.Interaction.Raw` preserves the full Slack payload — including
`response_url`, `trigger_id`, action values, and view state — as the platform
escape hatch.

The Slack adapter acknowledges the interaction with an empty 2xx inside
Slack's 3-second budget before your handler posts anything. For slow work,
combine this with [deferred dispatch](deferred-dispatch.md) and
`chat.ConcurrencyQueue`.

## Open A Modal

The Slack adapter preserves `trigger_id` on the raw payload and exposes modal
opening via `views.open`:

```go
err := slackAdapter.OpenModal(ctx, triggerID, modalView)
```

Slack invalidates `trigger_id` after 3 seconds, so open modals promptly. The
synchronous modal `view_submission` response (`response_action`) is not
supported: it requires responding in the webhook's HTTP response body, which
is incompatible with ack-then-work. Submitted view payloads still arrive as
events you can observe through the escape hatch.

## Respond Via response_url

For ephemeral-style responses to the person who clicked:

```go
err := slackAdapter.RespondURL(ctx, ev.Interaction.Raw, chat.Text("Working on it."))
```
