// Package logging is a small file logger: records go to a configured
// directory, one file per day, stale files are removed, and the standard
// library "log" package is redirected through the same sink.
//
// # Usage
//
//	if _, err := logging.Init("/var/log/myapp",
//	    logging.WithName("myapp"),
//	    logging.WithLevel(logging.LevelInfo),
//	    logging.WithMaxAge(7)); err != nil {
//	    panic(err)
//	}
//	defer logging.L().Close()
//
//	logging.Infof("listening on port %d", 8080)
//
//	ctx = logging.WithFields(ctx, "trace_id", tid)
//	logging.InfoContextf(ctx, "order created: %s", orderID)
//
//	log.Printf("written with the standard log package")
//
// Records can go to more than one place at once. Mirrors receive a copy of
// every line; the file stays the copy this package rotates and cleans up:
//
//	l, _ := logging.New("/var/log/myapp", logging.WithMirror(os.Stdout))
//
// # Files
//
//	<dir>/myapp-2026-10-25.log     everything written today
//	<dir>/myapp-2026-10-26.log     the next day's file
//
// Rotation needs no timer: every write checks whether the day is still the
// same and switches files when it is not. File names carry their own date, so
// nothing has to be renamed, and restarts do not interfere.
//
// # Cleanup
//
// Files whose last modification is older than the WithMaxAge number of days
// are removed. The directory is scanned once per day, on the first write after
// the change, and only <name>-YYYY-MM-DD.log files are candidates.
//
// # Limits
//
//   - Files are opened with O_APPEND, so several processes may share a
//     directory. Cleanup is not coordinated between them: two processes may
//     attempt the same deletion, and failures are reported on stderr without
//     interrupting logging.
//   - Lines arriving through log.Printf are recorded at INFO, because the
//     standard package has no notion of level. Their caller is recovered by
//     walking past the frames of this package and of "log"; when it cannot be
//     determined the caller field is left empty rather than wrong.
//   - Level methods return nothing. A failed write is reported on stderr and
//     does not surface in the caller's error handling.
//   - There is no asynchronous buffering, sampling or hook tree.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// DefaultName is the prefix of log file names.
	DefaultName = "app"
	// DefaultMaxAgeDays is how long an idle log file survives by default: seven
	// days cover a full operational week (weekend plus a week of incidents)
	// without letting the directory grow without bound.
	DefaultMaxAgeDays = 7
)

// options holds what New collected.
type options struct {
	name       string
	level      Level
	maxAgeDays int
	formatter  Formatter
	mirrors    []io.Writer
}

// Option configures New and Init.
type Option func(*options)

// WithName sets the prefix of log file names; DefaultName by default. Files
// are named <name>-YYYY-MM-DD.log.
//
// Names containing a path separator are rejected: the log directory is decided
// in one place only, and a name that tries to escape it is a bug, not a
// configuration.
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// WithLevel sets the minimum level recorded; LevelInfo by default, meaning
// LevelDebug output is dropped.
func WithLevel(l Level) Option {
	return func(o *options) { o.level = l }
}

// WithMaxAge removes files that have been idle for more than the given number
// of days. Zero or negative disables cleanup entirely.
func WithMaxAge(days int) Option {
	return func(o *options) { o.maxAgeDays = days }
}

// WithFormatter selects the output format.
func WithFormatter(f Formatter) Option {
	return func(o *options) { o.formatter = f }
}

// WithText selects the built-in text format.
func WithText() Option {
	return func(o *options) { o.formatter = TextFormatter{} }
}

// WithJSON selects the built-in JSON format.
func WithJSON() Option {
	return func(o *options) { o.formatter = JSONFormatter{} }
}

// WithMirror adds destinations that receive a copy of every record besides the
// primary sink: the rotating file for New, stdout or stderr for NewStdout and
// NewStderr.
//
// A mirror gets the line already formatted: rotation, cleanup and formatting
// are decided once, in one place, and the primary sink remains the copy this
// package owns. Nils are dropped rather than stored. A failing mirror is
// reported for that mirror alone and does not hold up the others.
func WithMirror(writers ...io.Writer) Option {
	return func(o *options) {
		for _, w := range writers {
			if w != nil {
				o.mirrors = append(o.mirrors, w)
			}
		}
	}
}

// Logger writes records to one sink. It is safe for concurrent use and
// implements io.Writer (see Write).
type Logger struct {
	out       io.Writer
	closer    func() error
	formatter Formatter
	level     Level
	// mirrors receive every formatted record in addition to out (see WithMirror).
	mirrors []io.Writer
	// toStderr records that the sink is stderr itself. Write failures must then
	// not be reported on stderr, or one failure would be printed twice.
	toStderr bool
}

// New returns a Logger writing into dir, creating the directory if needed, and
// reports an error for unusable arguments.
//
// Each Logger owns its own file handle and should be closed when it is no
// longer used. Closing late does not lose data - writes are not buffered - but
// leaves the handle open.
func New(dir string, opts ...Option) (*Logger, error) {
	o := options{
		name:       DefaultName,
		level:      LevelInfo,
		maxAgeDays: DefaultMaxAgeDays,
		formatter:  TextFormatter{},
	}
	for _, opt := range opts {
		opt(&o)
	}

	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("log directory is empty")
	}
	name := strings.TrimSpace(o.name)
	if name == "" {
		name = DefaultName
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid log file name %q: must not contain a path separator, nor be . or ..", name)
	}

	// The directory is created rather than required to exist: failing to write
	// logs is usually worse than creating one directory, and this lets a
	// service start on a fresh machine without preparation.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("log directory %q is unusable: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("log path %q is not a directory", dir)
	}

	fw := newFileWriter(dir, name, o.maxAgeDays)
	// Clean once at start-up: short-lived processes (cron jobs, one-off tools)
	// would otherwise never reach the daily cleanup.
	fw.cleanupLocked()

	return &Logger{
		out:       fw,
		closer:    fw.Close,
		formatter: o.formatter,
		level:     o.level,
		mirrors:   o.mirrors,
	}, nil
}

// Close releases the underlying file. It is idempotent.
func (l *Logger) Close() error {
	if l.closer == nil {
		return nil
	}
	return l.closer()
}

// Write implements io.Writer, allowing log.SetOutput to redirect the standard
// log package here.
//
// Each incoming line is recorded at INFO: "log" has no levels, so anything
// smarter would be a guess. It is therefore subject to the minimum level, so
// raising that level above LevelInfo also silences third-party output written
// through log.Printf.
//
// The returned count is always len(p): reporting a short write would only make
// the standard package turn a logging failure into io.ErrShortWrite.
func (l *Logger) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg == "" {
		return len(p), nil
	}
	l.record(LevelInfo, nil, callerFromStdLog(), msg)
	return len(p), nil
}

// ---------- Level methods ----------
//
// Each entry point reaches logf directly. Adding another layer in between
// would change the stack depth used to resolve the caller.

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, nil, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, nil, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, nil, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, nil, format, args...) }
func (l *Logger) Fatalf(format string, args ...any) { l.logf(LevelFatal, nil, format, args...) }
func (l *Logger) Panicf(format string, args ...any) { l.logf(LevelPanic, nil, format, args...) }
func (l *Logger) Printf(format string, args ...any) { l.logf(LevelInfo, nil, format, args...) }

func (l *Logger) DebugContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelDebug, FieldsFrom(ctx), format, args...)
}
func (l *Logger) InfoContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelInfo, FieldsFrom(ctx), format, args...)
}
func (l *Logger) WarnContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelWarn, FieldsFrom(ctx), format, args...)
}
func (l *Logger) ErrorContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelError, FieldsFrom(ctx), format, args...)
}
func (l *Logger) FatalContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelFatal, FieldsFrom(ctx), format, args...)
}
func (l *Logger) PanicContextf(ctx context.Context, format string, args ...any) {
	l.logf(LevelPanic, FieldsFrom(ctx), format, args...)
}

// logf is the common path of every level method.
//
// The caller is resolved here: skip 0 is logf, 1 is the level method, 2 is
// whoever called it. Entry points must therefore reach logf directly.
func (l *Logger) logf(level Level, fields []Field, format string, args ...any) {
	_, file, line, ok := runtime.Caller(2)
	l.record(level, fields, frameOf(file, line, ok), formatOf(format, args...))
}

// record writes one entry, or does nothing if the level is below the minimum.
//
// Fatal and Panic act only after the record has been written: nothing gets
// flushed once they return, so the most important line would otherwise be the
// one most likely to be lost.
func (l *Logger) record(level Level, fields []Field, caller, msg string) {
	if level < l.level {
		return
	}
	rec := Record{Time: time.Now(), Level: level, Msg: msg, Caller: caller, Fields: fields}
	line := l.formatter.Format(rec)
	if _, err := l.out.Write(line); err != nil && !l.toStderr {
		warnf("write log record: %v", err)
	}
	// Mirrors get the very same bytes: one line, decided once, copied as-is.
	for _, m := range l.mirrors {
		if _, err := m.Write(line); err != nil {
			warnf("write log mirror: %v", err)
		}
	}

	switch level {
	case LevelFatal:
		os.Exit(1)
	case LevelPanic:
		panic(msg)
	}
}

// formatOf renders the message.
//
// Sprintf is skipped when there are no arguments: it would parse the format
// again, and a stray percent sign is common in literal messages ("disk 100%
// full" would otherwise render as "disk 100%!f(MISSING) full").
func formatOf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// ---------- Caller ----------

// frameOf renders a caller as file:line. When the frame is unavailable it
// returns an empty string: no caller beats a wrong caller.
func frameOf(file string, line int, ok bool) string {
	if !ok {
		return ""
	}
	return filepath.Base(file) + ":" + fmt.Sprint(line)
}

// thisPkgPrefix is the package path of this package, used to tell whether a
// stack frame belongs to us.
var thisPkgPrefix = func() string {
	pc, _, _, _ := runtime.Caller(0)
	full := runtime.FuncForPC(pc).Name() // github.com/lyr2000/simple-go-logging/pkg/logging.init.func1
	if i := strings.LastIndex(full, "/"); i >= 0 {
		rest := full[i+1:]
		if j := strings.Index(rest, "."); j >= 0 {
			return full[:i+1+j] + "."
		}
	}
	return full + "."
}()

// callerFromStdLog resolves the caller of log.Printf as far as it can.
//
// How many frames sit between Printf and this Write is an implementation
// detail of the standard library and varies between Go versions, so the depth
// is not guessed. Frames are walked instead, skipping those belonging to this
// package and to "log"; if none is left, the caller stays empty.
func callerFromStdLog() string {
	for skip := 1; skip <= 8; skip++ {
		pc, file, line, ok := runtime.Caller(skip)
		if !ok {
			return ""
		}
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}
		name := fn.Name()
		if strings.HasPrefix(name, thisPkgPrefix) || strings.HasPrefix(name, "log.") {
			continue
		}
		return frameOf(file, line, true)
	}
	return ""
}

// ---------- Package-level entry points ----------
//
// The default logger exists so that importing the package is enough to log: it
// writes to stderr until Init (or SetDefault) replaces it, so nothing written
// during early start-up is lost.

var std atomic.Pointer[Logger]

func init() {
	std.Store(NewStdout())
}

// NewStderr returns a Logger writing to stderr at LevelInfo with the text
// format. It writes no files, so rotation and cleanup do not apply.
//
// It serves two purposes: logging while the destination directory is still
// unknown, and restoring the default logger (see SetDefault).
func NewStderr(opts ...Option) *Logger { return newSinkLogger(os.Stderr, opts...) }

// NewStdout returns a Logger writing to stdout. It differs from NewStderr only
// in the destination: console output belongs on stdout, diagnostics about this
// package itself stay on stderr (see warnf).
func NewStdout(opts ...Option) *Logger { return newSinkLogger(os.Stdout, opts...) }

// newSinkLogger builds a Logger writing to w. Defaults live here rather than
// in each constructor so that level and format cannot drift apart between
// New, NewStdout and NewStderr.
func newSinkLogger(w io.Writer, opts ...Option) *Logger {
	o := options{level: LevelInfo, formatter: TextFormatter{}}
	for _, opt := range opts {
		opt(&o)
	}
	return &Logger{
		out:       w,
		formatter: o.formatter,
		level:     o.level,
		mirrors:   o.mirrors,
		toStderr:  w == os.Stderr,
	}
}

// newStderrLogger is the initial state of the default logger.
func newStderrLogger() *Logger { return NewStderr() }

// L returns the current default logger.
func L() *Logger { return std.Load() }

// InitDefault creates a Logger writing into dir, installs it as the default and
// redirects the standard log package to it.
//
// After this call anything written with log.Printf or log.Println goes through
// the same file, rotation and cleanup.
func InitDefault(dir string, opts ...Option) (*Logger, error) {
	l, err := New(dir, opts...)
	if err != nil {
		return nil, err
	}
	SetDefault(l)
	return l, nil
}

// SetDefault installs l as the default logger and redirects the standard log
// package to it.
//
// The previous default's file handle is closed. Closing the initial stderr
// logger is a no-op. A file-based default still referenced elsewhere is not
// disabled by this: its next write opens the day's file again and appends.
func SetDefault(l *Logger) {
	if l == nil {
		return
	}
	old := std.Swap(l)

	// Standard flags are cleared because this package owns timestamps and
	// levels; leaving them set prints every timestamp twice.
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(l)

	if old != nil {
		_ = old.Close()
	}
}

// ---------- Package-level methods ----------
//
// Mirrors of the methods on Logger, applied to the default logger.
//
// Each one reaches logf directly rather than forwarding to the method of the
// same name: the caller depth is fixed, and one more frame would point the
// caller at a line inside this package.

func Debugf(format string, args ...any) { L().logf(LevelDebug, nil, format, args...) }
func Infof(format string, args ...any)  { L().logf(LevelInfo, nil, format, args...) }
func Warnf(format string, args ...any)  { L().logf(LevelWarn, nil, format, args...) }
func Errorf(format string, args ...any) { L().logf(LevelError, nil, format, args...) }
func Fatalf(format string, args ...any) { L().logf(LevelFatal, nil, format, args...) }
func Panicf(format string, args ...any) { L().logf(LevelPanic, nil, format, args...) }
func Printf(format string, args ...any) { L().logf(LevelInfo, nil, format, args...) }

func DebugContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelDebug, FieldsFrom(ctx), format, args...)
}
func InfoContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelInfo, FieldsFrom(ctx), format, args...)
}
func WarnContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelWarn, FieldsFrom(ctx), format, args...)
}
func ErrorContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelError, FieldsFrom(ctx), format, args...)
}
func FatalContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelFatal, FieldsFrom(ctx), format, args...)
}
func PanicContextf(ctx context.Context, format string, args ...any) {
	L().logf(LevelPanic, FieldsFrom(ctx), format, args...)
}

type StdLogger interface {
	Printf(fmt string, arg ...any)
}

type LevelLogger interface {
	Errorf(fmt string, arg ...any)
	Infof(fmt string, arg ...any)
	Debugf(fmt string, arg ...any)
	Warnf(fmt string, arg ...any)
	Panicf(fmt string, arg ...any)
	Fatalf(fmt string, arg ...any)
}
