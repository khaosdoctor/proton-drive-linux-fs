package logx

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

// fakeSink records every message (and its attrs, as a plain string map) handed to it, guarded by
// a mutex since the writer goroutine and the test goroutine can both touch it (the test reads
// only after stop() has returned, but the race detector doesn't know that without the lock).
type fakeSink struct {
	mu    sync.Mutex
	msgs  []string
	attrs []map[string]string
}

func (f *fakeSink) writeRecord(r slog.Record) error {
	m := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, r.Message)
	f.attrs = append(f.attrs, m)
	return nil
}

func (f *fakeSink) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.msgs))
	copy(out, f.msgs)
	return out
}

func (f *fakeSink) attrSnapshot() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]string, len(f.attrs))
	copy(out, f.attrs)
	return out
}

func TestAsyncHandlerDeliversInOrder(t *testing.T) {
	fs := &fakeSink{}
	h, stop := newAsyncHandler(fs, slog.LevelInfo)
	logger := slog.New(h)

	const n = 50
	for i := range n {
		logger.Info(fmt.Sprintf("msg-%d", i))
	}
	stop()

	got := fs.snapshot()
	if len(got) != n {
		t.Fatalf("got %d records, want %d", len(got), n)
	}
	for i, msg := range got {
		if want := fmt.Sprintf("msg-%d", i); msg != want {
			t.Fatalf("record %d = %q, want %q", i, msg, want)
		}
	}
}

func TestAsyncHandlerRespectsLevel(t *testing.T) {
	fs := &fakeSink{}
	h, stop := newAsyncHandler(fs, slog.LevelWarn)
	logger := slog.New(h)

	logger.Debug("dropped by level")
	logger.Info("also dropped by level")
	logger.Warn("kept")
	stop()

	got := fs.snapshot()
	if len(got) != 1 || got[0] != "kept" {
		t.Fatalf("got %v, want [\"kept\"]", got)
	}
}

// blockingSink lets a test hold the writer goroutine stuck inside writeRecord, so records queued
// behind it pile up and overflow the channel deterministically.
type blockingSink struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu    sync.Mutex
	count int
}

func (b *blockingSink) writeRecord(_ slog.Record) error {
	b.once.Do(func() { close(b.started) })
	<-b.release

	b.mu.Lock()
	b.count++
	b.mu.Unlock()
	return nil
}

func TestAsyncHandlerDropsWhenFull(t *testing.T) {
	sink := &blockingSink{started: make(chan struct{}), release: make(chan struct{})}
	h, stop := newAsyncHandler(sink, slog.LevelInfo)
	logger := slog.New(h)

	// The writer goroutine picks this one up and blocks in writeRecord, so nothing drains the
	// channel until release is closed.
	logger.Info("first")
	<-sink.started

	const extra = 100
	for range chanCap + extra {
		logger.Info("filler")
	}

	if got := h.core.dropped.Load(); got != extra {
		t.Fatalf("dropped = %d, want %d", got, extra)
	}

	close(sink.release)
	stop()

	sink.mu.Lock()
	total := sink.count
	sink.mu.Unlock()
	if want := 1 + chanCap; total != want {
		t.Fatalf("sink wrote %d records, want %d", total, want)
	}
}

func TestReportDroppedResetsAndLogsOnce(t *testing.T) {
	fs := &fakeSink{}
	core := newAsyncCore(fs)
	core.dropped.Store(7)

	core.reportDropped()
	core.stop()

	got := fs.snapshot()
	if len(got) != 1 || got[0] != "log records dropped" {
		t.Fatalf("got %v, want one \"log records dropped\" record", got)
	}
	if core.dropped.Load() != 0 {
		t.Fatalf("dropped count not reset, got %d", core.dropped.Load())
	}
}

func TestReportDroppedNoopWhenZero(t *testing.T) {
	fs := &fakeSink{}
	core := newAsyncCore(fs)

	core.reportDropped()
	core.stop()

	if got := fs.snapshot(); len(got) != 0 {
		t.Fatalf("got %v, want no records", got)
	}
}

func TestSanitizeFieldName(t *testing.T) {
	cases := map[string]string{
		"op":         "OP",
		"cache.hit":  "CACHE_HIT",
		"path":       "PATH",
		"already_OK": "ALREADY_OK",
		"a-b c":      "A_B_C",
		"_id":        "F__ID",
		"9lives":     "F_9LIVES",
	}
	for in, want := range cases {
		if got := sanitizeFieldName(in); got != want {
			t.Errorf("sanitizeFieldName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStopIsSafeToCallTwice(t *testing.T) {
	fs := &fakeSink{}
	_, stop := newAsyncHandler(fs, slog.LevelInfo)

	stop()
	stop() // must not panic (close of a closed channel) or hang
}

func TestWithAttrsAndWithGroupPrefixing(t *testing.T) {
	fs := &fakeSink{}
	h, stop := newAsyncHandler(fs, slog.LevelInfo)

	// A plain With(...): its attrs land on the record unprefixed, same as inline attrs.
	slog.New(h).With("op", "upload").Info("plain", "path", "/a")

	// WithGroup nests a prefix onto every attr logged afterward, from With(...) and inline alike;
	// an attr bound via With() before a deeper WithGroup keeps its own (shallower) prefix.
	slog.New(h).WithGroup("upload").With("op", "x").WithGroup("retry").Info("nested", "n", 2)

	stop()

	attrs := fs.attrSnapshot()
	if len(attrs) != 2 {
		t.Fatalf("got %d records, want 2", len(attrs))
	}

	if got := attrs[0]["op"]; got != "upload" {
		t.Errorf("plain record[op] = %q, want %q (record: %v)", got, "upload", attrs[0])
	}
	if got := attrs[0]["path"]; got != "/a" {
		t.Errorf("plain record[path] = %q, want %q (record: %v)", got, "/a", attrs[0])
	}

	if got := attrs[1]["upload.op"]; got != "x" {
		t.Errorf("nested record[upload.op] = %q, want %q (record: %v)", got, "x", attrs[1])
	}
	if got := attrs[1]["upload.retry.n"]; got != "2" {
		t.Errorf("nested record[upload.retry.n] = %q, want %q (record: %v)", got, "2", attrs[1])
	}
}

func TestJournaldFieldNameReservedNames(t *testing.T) {
	cases := map[string]string{
		"message":           "ATTR_MESSAGE",
		"priority":          "ATTR_PRIORITY",
		"syslog_identifier": "ATTR_SYSLOG_IDENTIFIER",
		"op":                "OP",
	}
	for in, want := range cases {
		if got := journaldFieldName(in); got != want {
			t.Errorf("journaldFieldName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevelToPriority(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "debug"},
		{slog.LevelInfo, "info"},
		{slog.LevelWarn, "warning"},
		{slog.LevelError, "error"},
	}

	names := map[int]string{0: "emerg", 1: "alert", 2: "crit", 3: "error", 4: "warning", 5: "notice", 6: "info", 7: "debug"}

	for _, c := range cases {
		got := names[int(levelToPriority(c.level))]
		if got != c.want {
			t.Errorf("levelToPriority(%v) = %v, want %v", c.level, got, c.want)
		}
	}
}
