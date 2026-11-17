package workspace

import "log/slog"

func nil2() *slog.Logger { return slog.New(slog.DiscardHandler) }
