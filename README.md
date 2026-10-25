# simple-go-logging

A small file logger for Go services: records go to a configured directory, one
file per day, stale files are removed, and `log.Printf` from the standard
library is redirected through the same sink.

Nothing to configure beyond a directory, no timers, no background goroutines,
no hook tree.

```sh
go get github.com/lyr2000/simple-go-logging
```

## Getting started

```go
package main

import (
	"context"
	"log"

	"github.com/lyr2000/simple-go-logging/pkg/logging"
)

func main() {
	if _, err := logging.Init("/var/log/myapp",
		logging.WithName("myapp"),
		logging.WithLevel(logging.LevelInfo),
		logging.WithMaxAge(7)); err != nil {
		panic(err)
	}
	defer logging.L().Close()

	logging.Infof("listening on port %d", 8080)

	ctx := logging.WithFields(context.Background(), "trace_id", "abc123")
	logging.InfoContextf(ctx, "order created: %s", orderID)

	log.Printf("this line comes from the standard log package")
}
```

```go
func setupLog(cfg *config.Config) {
	if cfg.App.LogDir == "" {
		cfg.App.LogDir = "_logs/"
	}
	var opt []logging.Option
	if runtime.GOOS == "windows" {
		opt = append(opt, logging.WithMirror(os.Stdout))
	}
	if _, err := logging.InitDefault(cfg.App.LogDir,
		logging.WithName("PROJECT_DEMO"),
		logging.WithLevel(logging.LevelInfo),
		logging.WithMaxAge(7)); err != nil {
		logging.Errorf("init error, err:%+v", err)
		return
	}
}


```

`Init` is `New` followed by `SetDefault`, so all three end up writing to the
same rotating file:

- `logging.Infof` — package-level API, writes to the default logger;
- `l.Infof` — instance API, writes to that logger only;
- `log.Printf` — anything already written with the standard package.

## Files

```
<dir>/myapp-2026-10-25.log     everything written today
<dir>/myapp-2026-10-26.log     the next day's file
```

The directory is created if it does not exist. Files are opened with
`O_APPEND`, so several processes may share one directory.

Rotation needs no timer: every write checks whether the day is still the same
and switches files when it is not. Because the name carries its own date, there
is nothing to rename and restarts cannot interfere. The cost is one
`time.Now` plus one `Format` per line, which buys freedom from timers that could
miss writes around midnight and from coordination between processes.

## Cleanup

Files that have not been written to for more than `WithMaxAge` days are
removed:

```go
logging.WithMaxAge(7)     // remove anything idle for longer than a week
logging.WithMaxAge(0)     // never remove anything
```

Two things worth knowing:

- Age comes from the **modification time**, not from the date in the name. The
  name may have been produced in another time zone or edited by hand; mtime
  records how long the file has actually been idle.
- Only files matching `<name>-YYYY-MM-DD.log` exactly are candidates.
  `notes.txt` or `other-2026-01-01.log` in the same directory are left alone.

The directory is scanned once per day, on the first write after the change, and
once at start-up — otherwise short-lived processes would never reach it.

## Levels

```go
logging.Debugf("cache lookup took %dms", ms)
logging.Infof("worker started")
logging.Warnf("retrying in %s", backoff)
logging.Errorf("upload failed: %v", err)
logging.Fatalf("cannot reach the database: %v", err)   // exits 1
logging.Panicf("invariant violated: %d", n)            // panics
```

`Debug` < `Info` < `Warn` < `Error` < `Fatal` < `Panic`. Anything below
`WithLevel` is dropped silently, including `log.Printf` lines. `Fatal` and
`Panic` write the record **before** exiting: nothing can be flushed afterwards,
so the most important line would otherwise be the one most likely to be lost.

`ParseLevel` turns configuration into a level and accepts `warning` as an alias
for `warn`. Unknown input is an error, never a silent fallback:

```go
lvl, err := logging.ParseLevel(os.Getenv("LOG_LEVEL"))
if err != nil {
	panic(err)
}
l, err := logging.New(os.Getenv("LOG_DIR"), logging.WithLevel(lvl))
if err != nil {
	panic(err)
}
logging.SetDefault(l)
```

## Context fields

```go
ctx = logging.WithFields(ctx, "trace_id", tid, "user_id", uid)
logging.InfoContextf(ctx, "checkout finished in %s", d)   // trace_id=… user_id=… checkout finished in …
```

Fields accumulate — middleware adds `trace_id`, the handler adds `user_id` —
and keep the order in which they were added. Duplicates are left as they are,
and a key without a value is recorded as `nil` rather than panicking the caller.

`logging.FieldsFrom(ctx)` returns a copy of the fields on a context, which is
also what you want if a component other than this package renders them.

## Formats

Text, for people:

```
2026-10-25 15:04:05.000 INFO main.go:42 trace_id=abc listening on 8080
```

JSON, for collectors:

```json
{"time":"2026-10-25T15:04:05Z","level":"info","caller":"main.go:42","trace_id":"abc","msg":"listening"}
```

```go
logging.WithText()   // default
logging.WithJSON()
```

Text quotes a value only when it contains whitespace, a quote or `=`, so there
is no ambiguity between `who=a b` and `who=a` followed by the key `b`. In JSON,
a field named like a reserved key (`time`, `level`, `msg`, `caller`) is renamed
to `field.msg` rather than overwriting it, and a value that cannot be marshalled
(a channel, say) degrades to its string form instead of taking the whole record
down.

Both are plain implementations of one interface, so anything else is a few
lines:

```go
type logfmt struct{}

func (logfmt) Format(r logging.Record) []byte {
	return []byte(r.Level.String() + " msg=" + strconv.Quote(r.Msg) + "\n")
}

logging.New(dir, logging.WithFormatter(logfmt{}))
```

## Console output, mirrors and multiple destinations

`NewStdout` and `NewStderr` write to the console only — no files, no rotation:

```go
logging.NewStdout(logging.WithJSON())
logging.NewStderr(logging.WithLevel(logging.LevelDebug))
```

The default logger starts out writing to stderr, so nothing is lost before
`Init` runs. Restoring that is exactly what `SetDefault(logging.NewStderr())`
does, which is how tests put the global state back.

To keep the rotating file **and** see the output in the terminal, add a mirror.
Mirrors receive the same formatted bytes, so there is no second formatter to
configure and nothing that can drift out of sync:

```go
l, err := logging.New("/var/log/myapp", logging.WithMirror(os.Stdout))
```

Several mirrors may be added at once, `nil` ones are dropped, and a mirror that
fails is reported on its own without holding up the others.

## The standard library bridge

`Init` and `SetDefault` call `log.SetOutput` and clear the standard flags, so
`log.Printf` output lands in the same file as everything else.

- Bridged lines are recorded at **INFO**: `log` has no notion of level, so
  anything smarter would be a guess. That also means they obey `WithLevel` —
  raising it above `LevelInfo` silences third-party output written this way.
- The caller is recovered by walking past the frames of this package and of
  `log`. When it cannot be determined, the caller field is left empty: no caller
  beats a wrong caller.
- `log.Fatalf` is still the standard library's own: it writes, then calls
  `os.Exit(1)` itself, without going through this package's fatal semantics.

## Options

| Option | Default | Meaning |
| --- | --- | --- |
| `WithName(name)` | `"app"` | Prefix of `<name>-YYYY-MM-DD.log`. Path separators, `.` and `..` are rejected. |
| `WithLevel(l)` | `LevelInfo` | Minimum level; everything below it is dropped. |
| `WithMaxAge(days)` | `7` | Remove files idle for longer, or never remove anything when `<= 0`. |
| `WithFormatter(f)` | `TextFormatter{}` | Any `Formatter`. |
| `WithText()` / `WithJSON()` | text | The two built-ins. |
| `WithMirror(w ...io.Writer)` | none | Extra copies of every line. |

`WithName` and `WithMaxAge` have nothing to act on for `NewStdout` and
`NewStderr`; `WithLevel`, `WithFormatter` and `WithMirror` apply everywhere.

## Instance or package-level

Prefer an instance when the logger is created and owned somewhere explicit, and
the package-level functions when logging has to be possible from anywhere.
Both resolve the caller correctly; `logging.L()` returns whichever logger is
current.

```go
l, err := logging.New(dir, opts...)     // you own it, you Close it
logging.SetDefault(l)                   // optional: adopt it globally
```

`Close` releases the file handle. It is idempotent, and not terminal: writing
afterwards opens the day's file again.

## Development

```sh
go test -race ./...      # also runs the examples below
go vet ./...
```

`example_test.go` holds runnable examples — `ExampleNewStdout`,
`Example_mirrorToStdout` — each with expected output, so they are executed by
`go test` and stay accurate.

## Not included

- No asynchronous buffering or background flush: one `write(2)` per line. A
  slow sink slows the caller, and a crash keeps everything already written.
- No size-based rotation, no compression, no upload of rotated files.
- No coordination of cleanup between processes sharing a directory. Two
  processes may attempt the same deletion; the loser is reported on stderr and
  keeps logging.
- No sampling, hooks or per-call sites configuration.
- `Context` is used only to carry fields; it does not cancel anything.

## License

MIT. See [LICENSE](LICENSE).
