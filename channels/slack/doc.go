// Package slack adapts Slack Events API webhooks to lebro channels.
//
// The adapter validates Slack's signed request protocol, maps each message
// event to a stable workspace/channel/thread conversation, and posts the final
// agent reply with chat.postMessage. It intentionally uses net/http instead of
// a Slack SDK, so it remains an optional leaf package with no provider runtime
// dependency.
//
// Slack expects an acknowledgement before long-running business work. Use the
// adapter with channels.Config.Dispatch backed by a durable queue or worker.
package slack
