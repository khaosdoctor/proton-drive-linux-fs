package logx

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// chanCap bounds how many records can queue for the writer goroutine before Handle starts
// dropping them. Generous enough to absorb a burst (a bulk copy logging one line per file)
// without blocking the FUSE handler goroutine that's doing the logging.
const chanCap = 4096

// dropReportInterval is how often the writer goroutine checks for a nonzero drop count and, if
// it finds one, emits a single Warn record and resets it.
const dropReportInterval = 10 * time.Second

// sink is where an async handler's writer goroutine finally puts a record. journaldSink and the
// stderr text fallback both implement it. Unexported so tests can inject a fake sink without a
// real journald socket.
type sink interface {
	writeRecord(r slog.Record) error
}

// asyncCore is the writer goroutine and its channel, shared by every handler cloned off the same
// root (WithAttrs/WithGroup produce a new *asyncHandler but keep the same core).
type asyncCore struct {
	sink     sink
	ch       chan slog.Record
	dropped  atomic.Int64
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newAsyncCore(s sink) *asyncCore {
	c := &asyncCore{
		sink: s,
		ch:   make(chan slog.Record, chanCap),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go c.run()
	return c
}

// run is the single goroutine that ever calls sink.writeRecord, so a sink (in particular the
// journald unix socket) never needs to be safe for concurrent use.
func (c *asyncCore) run() {
	defer close(c.done)

	ticker := time.NewTicker(dropReportInterval)
	defer ticker.Stop()

	for {
		select {
		case rec := <-c.ch:
			_ = c.sink.writeRecord(rec)
		case <-ticker.C:
			c.reportDropped()
		case <-c.quit:
			c.drain()
			return
		}
	}
}

// drain flushes whatever is still queued, without waiting for more; called once on shutdown.
func (c *asyncCore) drain() {
	for {
		select {
		case rec := <-c.ch:
			_ = c.sink.writeRecord(rec)
		default:
			return
		}
	}
}

func (c *asyncCore) reportDropped() {
	n := c.dropped.Swap(0)
	if n == 0 {
		return
	}

	rec := slog.NewRecord(time.Now(), slog.LevelWarn, "log records dropped", 0)
	rec.AddAttrs(slog.Int64("count", n))
	_ = c.sink.writeRecord(rec)
}

// stop asks the writer goroutine to flush and exit, and waits for it to do so. Safe to call more
// than once (and concurrently): the returned func from Setup/newAsyncHandler is a shutdown hook a
// caller may defer *and* call explicitly, so a second call must not double-close c.quit.
func (c *asyncCore) stop() {
	c.stopOnce.Do(func() {
		close(c.quit)
		<-c.done
	})
}

// asyncHandler is a slog.Handler that never blocks the caller: Handle copies the record onto a
// buffered channel for asyncCore's goroutine to write. A full channel means the sink can't keep
// up (e.g. journald itself is stalled); the record is dropped and counted rather than stalling
// whatever FUSE handler is logging, since a filesystem operation must never wait on logging.
type asyncHandler struct {
	core  *asyncCore
	level slog.Level
	attrs []slog.Attr
	group string // "" or the dot-joined prefix opened by WithGroup
}

func newAsyncHandler(s sink, level slog.Level) (*asyncHandler, func()) {
	core := newAsyncCore(s)
	h := &asyncHandler{core: core, level: level}
	return h, core.stop
}

func (h *asyncHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *asyncHandler) Handle(_ context.Context, r slog.Record) error {
	rec := r.Clone()
	if h.group != "" {
		rec = regroup(rec, h.group)
	}
	if len(h.attrs) > 0 {
		rec.AddAttrs(h.attrs...)
	}

	select {
	case h.core.ch <- rec:
	default:
		h.core.dropped.Add(1)
	}
	return nil
}

func (h *asyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	if h.group != "" {
		attrs = prefixAttrs(h.group, attrs)
	}

	next := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	next = append(next, h.attrs...)
	next = append(next, attrs...)

	return &asyncHandler{core: h.core, level: h.level, attrs: next, group: h.group}
}

// WithGroup is part of the slog.Handler interface; this codebase never opens a group, so it only
// needs to keep attrs logged afterward correctly namespaced.
// ponytail: single flat prefix, not a real nested group tree — good enough since nothing here calls it.
func (h *asyncHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	group := name
	if h.group != "" {
		group = h.group + "." + name
	}
	return &asyncHandler{core: h.core, level: h.level, attrs: h.attrs, group: group}
}

// regroup returns a copy of r with every attr's key prefixed by "<group>.".
func regroup(r slog.Record, group string) slog.Record {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(slog.Attr{Key: group + "." + a.Key, Value: a.Value})
		return true
	})
	return nr
}

func prefixAttrs(group string, attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Attr{Key: group + "." + a.Key, Value: a.Value}
	}
	return out
}
