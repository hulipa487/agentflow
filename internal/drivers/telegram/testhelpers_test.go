package telegram

import (
	"io"
	"log/slog"
)

// testLogger is a discard logger shared by the telegram tests; tests assert
// on observable effects (setWebhook/deleteWebhook calls) rather than logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil)).With("driver", "telegram", "channel", "telegram", "mode", "auto")
}