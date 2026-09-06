// Package logx sets up the process-wide structured logger: an asynchronous slog handler that
// writes to the systemd journal when it's available, falling back to a text handler on stderr.
// Every package logs through the standard slog.Info/Debug/Warn/Error calls against the default
// logger Setup installs; nothing here needs a logger threaded through it.
package logx

import (
	"context"
	"log/slog"
	"os"
)

// defaultTag is the syslog identifier logged entries carry and the journalctl -t filter value.
const defaultTag = "proton-drive-fs"

// Options configures Setup.
type Options struct {
	// Level is the minimum level that gets logged; slog.LevelInfo by default (the zero value).
	Level slog.Level

	// Tag is the syslog identifier logged entries carry (SYSLOG_IDENTIFIER) and the journalctl -t
	// filter value. Defaults to "proton-drive-fs".
	Tag string

	// ForceStderr skips journald even when it's available, e.g. for a -foreground run where the
	// user wants to watch logs in the terminal instead of following the journal.
	ForceStderr bool
}

// Setup builds the process-wide logger, installs it as slog's default, and returns it alongside
// a func that flushes pending records and stops the writer goroutine. Call the func on shutdown
// (or defer it right after Setup).
func Setup(opts Options) (*slog.Logger, func()) {
	tag := opts.Tag
	if tag == "" {
		tag = defaultTag
	}

	var s sink
	if !opts.ForceStderr && JournaldAvailable() {
		s = journaldSink{tag: tag}
	} else {
		s = textSink{h: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     opts.Level,
			AddSource: opts.Level <= slog.LevelDebug,
		})}
	}

	h, stop := newAsyncHandler(s, opts.Level)
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger, stop
}

// textSink adapts a plain slog.Handler (the stderr fallback) to the sink interface, so it runs
// through the same async channel and drop-counting as journaldSink.
type textSink struct {
	h slog.Handler
}

func (s textSink) writeRecord(r slog.Record) error {
	return s.h.Handle(context.Background(), r)
}

func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
