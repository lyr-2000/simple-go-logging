package logging_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lyr2000/simple-go-logging/pkg/logging"
)

// This file deliberately lives in the external test package: asserting that
// the caller of log.Printf resolves correctly would be meaningless from
// inside this package, whose frames callerFromStdLog skips.

// newDirLogger installs a Logger writing into a temporary directory as the
// default and returns the path of today's file.
func newDirLogger(t *testing.T, opts ...logging.Option) (*logging.Logger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := logging.New(dir, append([]logging.Option{logging.WithName("app")}, opts...)...)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	logging.SetDefault(l)

	// The cleanup step has to be registered after t.TempDir(), since Cleanup
	// runs last-in first-out: restoring the default logger closes the file
	// handle first, and on Windows an open file cannot be deleted with the
	// directory.
	t.Cleanup(func() {
		logging.SetDefault(logging.NewStderr())
		log.SetOutput(os.Stderr)
	})
	return l, filepath.Join(dir, "app-"+time.Now().Format(logging.DateLayout)+".log")
}

// A line written with log.Printf lands in the same file, is recorded at INFO,
// and its caller points at whoever called Printf.
func TestStdLogLineKeepsRealCaller(t *testing.T) {
	_, path := newDirLogger(t)

	log.Printf("written with log.Printf")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(b)
	for _, want := range []string{"written with log.Printf", "INFO", "exported_test.go:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "logging.go:") {
		t.Errorf("caller should not point inside this package: %s", got)
	}
}

// Package-level functions - not methods - must also resolve the caller: one
// extra forwarding layer would point it at a line inside this package.
func TestPackageLevelAPIKeepsCallerAndFields(t *testing.T) {
	_, path := newDirLogger(t)

	ctx := logging.WithFields(context.Background(), "trace_id", "abc-123")
	logging.InfoContextf(ctx, "through the package-level API")
	logging.Infof("and one more")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "trace_id=abc-123") {
		t.Errorf("context field missing: %s", got)
	}
	if strings.Count(got, "exported_test.go:") != 2 {
		t.Errorf("both records should report the real caller: %s", got)
	}
}

// Every level reaches the file, one line each.
func TestPackageLevelsWriteInOrder(t *testing.T) {
	_, path := newDirLogger(t, logging.WithLevel(logging.LevelDebug))

	logging.Debugf("d")
	logging.Infof("i")
	logging.Warnf("w")
	logging.Errorf("e")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(b)
	for _, want := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 4 {
		t.Errorf("four lines expected, got %d: %s", lines, got)
	}
}

// ---------- Stdout sink and mirrors ----------

// NewStdout writes to os.Stdout, captured at construction time.
func TestNewStdoutWritesToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = orig
		// Nothing may stay pointed at the closed pipe.
		logging.SetDefault(logging.NewStderr())
	}()

	l := logging.NewStdout()
	logging.SetDefault(l)
	logging.Infof("to stdout")

	_ = w.Close()
	os.Stdout = orig
	logging.SetDefault(logging.NewStderr())

	out := make([]byte, 512)
	n, _ := r.Read(out)
	got := string(out[:n])
	if !strings.Contains(got, "to stdout") {
		t.Fatalf("record should reach stdout, got %q", strings.TrimSpace(got))
	}
	if !strings.Contains(got, "INFO") {
		t.Fatalf("level should be rendered, got %q", strings.TrimSpace(got))
	}
}

// A mirror receives exactly what the primary sink receives: one line, both
// places.
func TestMirrorReceivesTheSameLine(t *testing.T) {
	var buf bytes.Buffer
	_, path := newDirLogger(t, logging.WithMirror(&buf))

	logging.Infof("through the file logger")

	written := buf.String()
	if !strings.Contains(written, "through the file logger") {
		t.Fatalf("mirror did not receive the record: %q", written)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if got, mirror := string(b), buf.String(); got != mirror {
		t.Fatalf("mirror and file diverged:\nfile   %q\nmirror %q", got, mirror)
	}
}

// Mirrors follow the level filter: what is dropped is dropped everywhere, so
// the mirror cannot become a back door past WithLevel.
func TestMirrorHonoursTheMinimumLevel(t *testing.T) {
	var buf bytes.Buffer
	_, _ = newDirLogger(t, logging.WithLevel(logging.LevelWarn), logging.WithMirror(&buf))

	logging.Infof("dropped")
	logging.Warnf("kept")

	got := buf.String()
	if strings.Contains(got, "dropped") {
		t.Errorf("a filtered record reached the mirror: %q", got)
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("the record above the threshold is missing: %q", got)
	}
}

// Before Init the default logger writes to stderr, so early start-up output is
// not lost.
func TestDefaultLoggerWritesToStderrBeforeInit(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	// The logger has to be constructed after redirecting os.Stderr: it captures
	// the handle at construction time.
	logging.SetDefault(logging.NewStderr())

	logging.Infof("written before Init")

	_ = w.Close()
	os.Stderr = orig
	// Restore with a logger pointing at the real stderr, leaving later tests
	// with a usable default instead of a closed pipe.
	logging.SetDefault(logging.NewStderr())

	out := make([]byte, 512)
	n, _ := r.Read(out)
	if got := string(out[:n]); !strings.Contains(got, "written before Init") {
		t.Fatalf("the default logger should write to stderr, got %q", strings.TrimSpace(got))
	}
}
