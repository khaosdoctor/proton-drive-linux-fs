package logx

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Op returns a logger scoped to one filesystem operation, carrying "op" and "path" as attrs on
// every line it logs.
func Op(ctx context.Context, op, path string) *slog.Logger {
	return slog.Default().With("op", op, "path", path)
}

// Elapsed returns a "elapsed" duration attr measured from start, for a log line at the end of an
// operation.
func Elapsed(start time.Time) slog.Attr {
	return slog.Duration("elapsed", time.Since(start))
}

// DebugEnabled reports whether the default logger would actually emit a debug record. Guard a
// slog.Debug call with it on a per-request hot path (e.g. once per block read) so building the
// attrs (which allocates when it boxes non-string values into `any`) is skipped entirely at the
// default info level.
func DebugEnabled(ctx context.Context) bool {
	return slog.Default().Enabled(ctx, slog.LevelDebug)
}

// FormatSize renders a byte count as a human-readable size with one decimal place: "2.3 MiB",
// "512.0 KiB", "128 B" below 1 KiB.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
