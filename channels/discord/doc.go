// Package discord adapts Discord application-command interactions to lebro
// channels. It validates Discord's Ed25519 webhook signatures, acknowledges
// interactions promptly, and edits the deferred original response with the
// completed agent reply.
//
// It is an optional leaf package built only on the Go standard library. Use it
// with channels.Config.Dispatch: Discord interaction tokens expire after 15
// minutes and must receive their initial deferred acknowledgement within three
// seconds.
package discord
