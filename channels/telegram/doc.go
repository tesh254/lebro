// Package telegram adapts Telegram Bot API webhook updates to lebro channels.
// It verifies Telegram's webhook secret header, maps chat and forum-topic
// conversations to durable threads, and sends final agent replies with the Bot
// API. The package is optional and uses only net/http.
package telegram
