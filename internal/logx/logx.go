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

// stopPrevious stops the writer of the logger the last Setup installed. The daemon calls Setup
// twice (run's plain-info logger, then mount's -log-level one), and the first writer goroutine,
// its ticker and its 4096-slot channel would otherwise live until exit with nothing logging to
// them. Setup only runs during single-goroutine startup, so this needs no lock.
var stopPrevious func()

// Setup builds the process-wide logger, installs it as slog's default, stops the writer of the
// logger a previous Setup installed (flushing what it still had queued), and returns the new
// logger alongside a func that flushes pending records and stops its writer goroutine. Call the
// func on shutdown (or defer it right after Setup).
func Setup(opts Options) (*slog.Logger, func()) {
	tag := opts.Tag
	if tag == "" {
		tag = defaultTag
	}

	// The journald sink always gets debug+: the journal does its own priority filtering
	// (journalctl -p), so gating it on -log-level would make that filtering redundant and, worse,
	// mean debug records never reach journald at all when -log-level is left at its info default.
	// The stderr/file fallback has no such filtering of its own, so it stays gated on opts.Level.
	var s sink
	var level slog.Level
	if !opts.ForceStderr && JournaldAvailable() {
		s = journaldSink{tag: tag}
		level = slog.LevelDebug
	} else {
		s = textSink{h: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     opts.Level,
			AddSource: opts.Level <= slog.LevelDebug,
		})}
		level = opts.Level
	}

	h, stop := newAsyncHandler(s, level)
	logger := slog.New(h)
	slog.SetDefault(logger)

	// Stopped only after the new default is in place, so nothing logs into the old one meanwhile.
	if stopPrevious != nil {
		stopPrevious()
	}
	stopPrevious = stop
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
