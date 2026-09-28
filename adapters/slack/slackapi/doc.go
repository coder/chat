// Package slackapi is a low-level client for the Slack Web API. It works on raw
// Slack IDs (team, channel, user, and message timestamps) and does not use the
// portable chat.Adapter surface.
//
// The package holds the Slack-specific mechanics that the Slack adapter and other
// Slack integrations share: authenticated Web API calls with bounded rate-limit
// retry (ADR 0005), a typed APIError for ok:false responses, and request signature
// verification for inbound webhooks.
package slackapi
