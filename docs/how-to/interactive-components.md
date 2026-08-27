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
if err != nil {
	return err
}
_ = sent // sent.ID identifies the posted message, like portable posting
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
			"Approved (by user " + ev.Interaction.Actor.ID + ")",
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
escape hatch. Be aware that the concrete payload type behind `Raw` is
currently unexported with no public accessor (tracked in
[#12](https://github.com/coder/chat/issues/12)): routing on `ActionID` works
for buttons and menus alike, but reading a menu's *selected option value* is
not yet possible without re-parsing the webhook JSON yourself. Design around
distinct `action_id`s where you can until #12 lands.

The normalized `Actor` carries the Slack user ID, not a display name (the
interactivity payload does not include one). Note that plain `chat.Text` is
posted with Slack formatting disabled, so `<@USERID>` mention syntax renders
literally; to render a real mention, resolve the display name via the Slack
API or post native Block Kit content instead.

**Known limitation:** the interaction event identity is currently anchored on
the message timestamp, not the individual activation — so when the same user
activates the same `action_id` on the same message more than once within
`DedupeTTL` (default 24 hours), only the first activation reaches
`OnInteraction`; the rest are dropped as duplicates. This also affects a menu
whose options share one action ID. Tracked in
[#43](https://github.com/coder/chat/issues/43). Until it lands, give
repeat-activatable controls distinct `action_id`s (or replace the message's
blocks after each click).

Mind the acknowledgement timing: under the default `DispatchSync` mode the
adapter writes the empty 2xx only *after* your handler returns, so a slow
handler can miss Slack's 3-second acknowledgement budget. Enable
[deferred dispatch](deferred-dispatch.md) (with `chat.ConcurrencyQueue`) so
the acknowledgement is not blocked on your handler — the handler moves to a
detached tail launched at ack time.

## Open A Modal

The Slack adapter exposes modal opening via `views.open`:

```go
err := slackAdapter.OpenModal(ctx, triggerID, modalView)
```

In multi-tenant mode (`InstallStore` configured), `OpenModal` returns an
error because there is no single workspace token; use the tenant-aware
variant with the event's tenant instead:

```go
err := slackAdapter.OpenModalForTenant(ctx, ev.Event.Tenant, triggerID, modalView)
```

**Known gap:** Slack's `trigger_id` is preserved on the `Raw` escape hatch,
but there is currently no public accessor to extract it — the preserved
payload types are unexported — so this flow is not yet reachable without
re-parsing the original webhook JSON yourself. Tracked in
[#12](https://github.com/coder/chat/issues/12). (`RespondURL` is unaffected:
it accepts the `Raw` value directly.)

Slack invalidates `trigger_id` after 3 seconds, so open modals promptly.
Modal *submissions* are not part of this slice: the synchronous
`view_submission` response (`response_action`) requires responding in the
webhook's HTTP response body, which is incompatible with ack-then-work, so
the adapter acknowledges `view_submission` payloads and drops them —
application code cannot observe submitted view values today.

## Respond Via response_url

For ephemeral-style responses to the person who clicked:

```go
err := slackAdapter.RespondURL(ctx, ev.Interaction.Raw, chat.Text("Working on it."))
```
