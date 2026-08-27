# 01 - Live-tenant validation of the Teams adapter spike

Status: ready-for-human

## Summary

Validate the `spike/msteams-adapter` implementation (PR #4) against a real Azure Bot
resource and Teams tenant so ADR 0007 can move from Proposed to Accepted and the adapter
can land as experimental.

Public tracking: https://github.com/coder/chat/issues/6

## Tasks

Work through the `spike-required` markers in `adapters/msteams` (the ADR 0007 Open
Questions, now the live test plan), including:

- Inbound ack semantics and the real turn timeout for the `msteams` channel.
- Reply delivery as a separate Connector REST call (no body-reply shortcut).
- Exact channel-endorsement rule (the spike fails closed when `msteams` is absent).
- Single-tenant Azure Bot resource specifics (token URL, `aud`/`iss`).
- The stdlib JWT/JWKS validator against real Teams-issued tokens (carried over from
  Open Question 9 after `msbotbuilder-go` was rejected).
- Teams Markdown fidelity, `serviceUrl`/`conversation.id` persistence stability,
  proactive-posting prerequisites, mention behavior, canonical `Actor.ID`, and
  `Activity.id` dedupe stability.

## Acceptance

- Every `spike-required` marker is confirmed or corrected against a live tenant.
- ADR 0007 flips to Accepted and the adapter lands as experimental.

## Comments

Created while recording the ADR 0007 spike findings on main (PR #14), so the remaining
live validation is tracked in this repo's `.scratch` issue tracker per
`docs/agents/issue-tracker.md`, with GitHub issue #6 as the public mirror.
