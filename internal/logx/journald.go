package logx

import (
	"log/slog"
	"runtime"
	"strconv"
	"strings"

	"github.com/coreos/go-systemd/v22/journal"
)

// journaldSocket is where systemd exposes the journal's structured logging socket; its presence
// is how Setup decides between journald and the stderr fallback.
const journaldSocket = "/run/systemd/journal/socket"

// JournaldAvailable reports whether the systemd journal's logging socket exists, i.e. whether
// Setup would pick it as the sink. Exported so main can decide whether the detached mount needs
// a log-file fallback before it even starts the daemon.
func JournaldAvailable() bool {
	return statExists(journaldSocket)
}

// journaldSink writes records to the local systemd journal over its unix socket
// (github.com/coreos/go-systemd/v22/journal, pure Go, no cgo).
type journaldSink struct {
	tag string
}

func (s journaldSink) writeRecord(r slog.Record) error {
	vars := map[string]string{"SYSLOG_IDENTIFIER": s.tag}

	r.Attrs(func(a slog.Attr) bool {
		vars[journaldFieldName(a.Key)] = a.Value.String()
		return true
	})

	// Resolving the caller's file/func/line costs a runtime.CallersFrames call; only pay for it
	// on debug records, which are already the expensive, high-volume ones.
	if r.Level <= slog.LevelDebug && r.PC != 0 {
		addSourceFields(vars, r.PC)
	}

	return journal.Send(r.Message, levelToPriority(r.Level), vars)
}

func addSourceFields(vars map[string]string, pc uintptr) {
	frames := runtime.CallersFrames([]uintptr{pc})
	frame, _ := frames.Next()
	if frame.Function == "" {
		return
	}
	vars["CODE_FUNC"] = frame.Function
	vars["CODE_FILE"] = frame.File
	vars["CODE_LINE"] = strconv.Itoa(frame.Line)
}

func levelToPriority(level slog.Level) journal.Priority {
	switch {
	case level >= slog.LevelError:
		return journal.PriErr
	case level >= slog.LevelWarn:
		return journal.PriWarning
	case level >= slog.LevelInfo:
		return journal.PriInfo
	default:
		return journal.PriDebug
	}
}

// journaldFieldName turns a slog attr key into a valid journald field name: sanitized, with a
// reserved name journal.Send sets itself renamed so it can't collide with (and silently multi-
// value) that field.
func journaldFieldName(key string) string {
	return reservedFieldName(sanitizeFieldName(key))
}

// reservedFields are the journal fields journal.Send populates itself (MESSAGE and PRIORITY
// always, SYSLOG_IDENTIFIER via journaldSink); an attr sanitizing to one of these is renamed
// ATTR_<name> so it can't overwrite or duplicate them.
var reservedFields = map[string]bool{
	"MESSAGE":           true,
	"PRIORITY":          true,
	"SYSLOG_IDENTIFIER": true,
}

func reservedFieldName(name string) string {
	if reservedFields[name] {
		return "ATTR_" + name
	}
	return name
}

// sanitizeFieldName turns a slog attr key into a valid journald field name: uppercase, with
// every character outside [A-Z0-9_] replaced by "_", and, since journald also rejects a field
// name starting with "_" or a digit, prefixed with "F_" when it does (journald's field-name
// rules).
func sanitizeFieldName(key string) string {
	upper := strings.ToUpper(key)
	b := make([]byte, len(upper))
	for i := 0; i < len(upper); i++ {
		c := upper[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b[i] = c
		default:
			b[i] = '_'
		}
	}

	name := string(b)
	if name == "" || name[0] == '_' || (name[0] >= '0' && name[0] <= '9') {
		return "F_" + name
	}
	return name
}
