package logging

import (
	"fmt"
	"strings"
)

// Level is a log severity. Lower values are more verbose: LevelDebug is the
// most verbose, LevelPanic the most severe.
type Level int

const (
	// LevelDebug is diagnostic output intended for development.
	LevelDebug Level = iota
	// LevelInfo describes normal operation and is the default minimum level.
	LevelInfo
	// LevelWarn marks an unusual condition that the process can continue past.
	LevelWarn
	// LevelError marks an operation that failed while the process survived.
	LevelError
	// LevelFatal terminates the process with status 1 after writing the record.
	LevelFatal
	// LevelPanic panics after writing the record.
	LevelPanic
)

// String returns the label used in text records. Labels are five characters
// wide so the fields that follow stay aligned column-wise.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	case LevelPanic:
		return "PANIC"
	default:
		return fmt.Sprintf("LEVEL(%d)", int(l))
	}
}

// shortString returns the label used in JSON records.
func (l Level) shortString() string {
	return strings.ToLower(l.String())
}

// ParseLevel converts a level name read from configuration into a Level.
//
// "warning" is accepted as an alias for "warn". Unknown input is an error
// rather than a silent fallback to some arbitrary level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	case "fatal":
		return LevelFatal, nil
	case "panic":
		return LevelPanic, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q (want debug, info, warn, error, fatal or panic)", s)
	}
}
