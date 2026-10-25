package logging_test

import (
	"fmt"
	"os"

	"github.com/lyr2000/simple-go-logging/pkg/logging"
)

// levelOnly is used by the examples so that what they print stays reproducible.
//
// Examples carrying an "// Output:" comment are executed by "go test", so a
// real timestamp would differ on every run. Any Formatter can do this; whether
// the time matters is the caller's call.
type levelOnly struct{}

func (levelOnly) Format(r logging.Record) []byte {
	return []byte(r.Level.String() + " " + r.Msg + "\n")
}

// Console output, with nothing written to disk.
func ExampleNewStdout() {
	l := logging.NewStdout(logging.WithFormatter(levelOnly{}))
	l.Infof("listening on port %d", 8080)
	// Output:
	// INFO listening on port 8080
}

// The usual combination: console output now, rotating files for later.
//
// The mirror receives the same line the file receives, so there is no second
// formatter to configure and nothing that can drift out of sync.
func Example_mirrorToStdout() {
	dir, err := os.MkdirTemp("", "logging-example-*")
	if err != nil {
		panic(err)
	}
	// Deferred calls run last-in first-out, so these are registered in
	// reverse: drop the logger as default first (releasing the file handle,
	// without which Windows cannot delete the directory), then close it, then
	// remove the directory.
	defer os.RemoveAll(dir)

	l, err := logging.New(dir,
		logging.WithName("myapp"),
		logging.WithMirror(os.Stdout),
		logging.WithFormatter(levelOnly{}))
	if err != nil {
		panic(err)
	}
	defer l.Close()

	logging.SetDefault(l)
	defer logging.SetDefault(logging.NewStderr())

	logging.Infof("listening on port %d", 8080)
	logging.Errorf("cache warm-up failed: %v", fmt.Errorf("connection refused"))

	// Both lines went to stdout and to the file. The count is printed instead
	// of the name because the name carries today's date and would change daily.
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	fmt.Println("files written:", len(entries))

	// Output:
	// INFO listening on port 8080
	// ERROR cache warm-up failed: connection refused
	// files written: 1
}
