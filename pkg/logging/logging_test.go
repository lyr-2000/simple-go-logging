package logging

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- Helpers ----------

// newTestLogger builds a Logger in a temporary directory and replaces its
// clock. Rotation and cleanup both depend on the current day, so without an
// injected clock they could only be tested by waiting for midnight.
func newTestLogger(t *testing.T, now time.Time, opts ...Option) (*Logger, *fileWriter) {
	t.Helper()
	l, err := New(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fw := l.out.(*fileWriter)
	fw.now = func() time.Time { return now }
	return l, fw
}

// fileSet lists the names in a directory.
func fileSet(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readFile reads one file from a directory.
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// touch creates an empty file with the given modification time, standing in
// for a log file nobody has written to for a while.
func touch(t *testing.T, dir, name string, mod time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	_ = f.Close()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("set mtime of %s: %v", name, err)
	}
}

// dirOf recovers the directory from a Logger.
func dirOf(t *testing.T, l *Logger) string {
	t.Helper()
	return l.out.(*fileWriter).dir
}

// ---------- 1. Output directory ----------

// Records land in the configured directory, in a file named after today.
func TestWritesIntoGivenDirectory(t *testing.T) {
	now := time.Date(2026, 10, 25, 15, 4, 5, 0, time.Local)
	l, _ := newTestLogger(t, now, WithName("myapp"))

	l.Infof("hello %s", "world")

	want := "myapp-2026-10-25.log"
	if names := fileSet(t, dirOf(t, l)); len(names) != 1 || names[0] != want {
		t.Fatalf("want exactly %s in the directory, got %v", want, names)
	}
	got := readFile(t, dirOf(t, l), want)
	if !strings.Contains(got, "hello world") {
		t.Fatalf("message missing: %q", got)
	}
	if !strings.Contains(got, "logging_test.go:") {
		t.Fatalf("caller missing: %q", got)
	}
}

// A missing directory is created rather than reported as an error.
func TestNewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")

	l, err := New(dir)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	defer l.Close()

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory should have been created: %v", err)
	}
}

// ---------- 2. Daily rotation ----------

// After midnight records go to the new day's file rather than being appended
// to yesterday's.
func TestRotatesToANewFileEachDay(t *testing.T) {
	day1 := time.Date(2026, 10, 25, 23, 59, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Minute)
	l, fw := newTestLogger(t, day1, WithName("app"))

	l.Infof("written on day one")
	fw.now = func() time.Time { return day2 }
	l.Infof("written on day two")

	dir := dirOf(t, l)
	yesterday := readFile(t, dir, "app-2026-10-25.log")
	if !strings.Contains(yesterday, "written on day one") {
		t.Fatalf("yesterday's file lost its record: %q", yesterday)
	}
	if strings.Contains(yesterday, "written on day two") {
		t.Fatal("today's record leaked into yesterday's file")
	}
	if got := readFile(t, dir, "app-2026-10-26.log"); !strings.Contains(got, "written on day two") {
		t.Fatalf("today's file is missing the record: %q", got)
	}
}

// Repeated writes on the same day append to one file.
func TestSameDayKeepsAppendingToOneFile(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, fw := newTestLogger(t, now, WithName("app"))

	l.Infof("first")
	fw.now = func() time.Time { return now.Add(6 * time.Hour) }
	l.Infof("later the same day")

	dir := dirOf(t, l)
	if names := fileSet(t, dir); len(names) != 1 {
		t.Fatalf("one file per day expected, got %v", names)
	}
	got := readFile(t, dir, "app-2026-10-25.log")
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("two lines expected, got %q", got)
	}
}

// ---------- 3. Cleanup ----------

// Files idle for more than MaxAge days are removed; recent files and anything
// that is not ours stay.
func TestCleanupRemovesStaleFilesOnly(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.Local)
	l, fw := newTestLogger(t, now, WithName("app"), WithMaxAge(7))
	dir := dirOf(t, l)

	touch(t, dir, "app-2026-10-17.log", now.AddDate(0, 0, -8))    // stale: removed
	touch(t, dir, "app-2026-10-24.log", now.AddDate(0, 0, -1))    // recent: kept
	touch(t, dir, "other-2026-10-01.log", now.AddDate(0, 0, -24)) // different name: kept
	touch(t, dir, "app-not-a-date.log", now.AddDate(0, 0, -24))   // not our naming: kept
	touch(t, dir, "notes.txt", now.AddDate(0, 0, -30))            // other suffix: kept

	fw.cleanupLocked()

	names := strings.Join(fileSet(t, dir), ",")
	for _, keep := range []string{"app-2026-10-24.log", "other-2026-10-01.log", "app-not-a-date.log", "notes.txt"} {
		if !strings.Contains(names, keep) {
			t.Errorf("%s should have been kept, directory is now: %s", keep, names)
		}
	}
	if strings.Contains(names, "app-2026-10-17.log") {
		t.Errorf("stale log should have been removed, directory is now: %s", names)
	}
}

// A non-positive MaxAge disables cleanup entirely; it does not mean "delete
// immediately".
func TestCleanupDisabledWhenMaxAgeNotPositive(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.Local)
	l, fw := newTestLogger(t, now, WithName("app"), WithMaxAge(0))
	dir := dirOf(t, l)

	touch(t, dir, "app-2026-01-01.log", now.AddDate(0, 0, -300))
	fw.cleanupLocked()

	if names := fileSet(t, dir); !strings.Contains(strings.Join(names, ","), "app-2026-01-01.log") {
		t.Fatalf("nothing may be removed while cleanup is off, got %v", names)
	}
}

// Cleanup is triggered by the next write after the day changes, with no timer
// watching over it.
func TestCleanupRunsOnDayChange(t *testing.T) {
	day1 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.Local)
	l, fw := newTestLogger(t, day1, WithName("app"), WithMaxAge(3))
	dir := dirOf(t, l)

	l.Infof("claim today's file")
	touch(t, dir, "app-2026-10-01.log", day1.AddDate(0, 0, -24))
	fw.now = func() time.Time { return day1.AddDate(0, 0, 1) }
	l.Infof("first record of the next day")

	if names := fileSet(t, dir); strings.Contains(strings.Join(names, ","), "app-2026-10-01.log") {
		t.Fatalf("stale file should have been cleaned on day change, got %v", names)
	}
}

// ---------- 4. Redirecting the standard log package ----------

// Lines handed over by the standard package land in the same file, at INFO.
func TestWriteBridgesStdLogLine(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithName("app"))

	line := "a line handed over by log.Printf\n"
	n, err := l.Write([]byte(line))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(line) {
		t.Fatalf("Write should report len(p), got %d", n)
	}

	got := readFile(t, dirOf(t, l), "app-2026-10-25.log")
	if !strings.Contains(got, "INFO") || !strings.Contains(got, strings.TrimSpace(line)) {
		t.Fatalf("expected INFO and the original text: %q", got)
	}
}

// Whether the bridged caller points at the real caller is asserted by
// TestStdLogLineKeepsRealCaller in exported_test.go. Doing it here would be
// meaningless: this file belongs to the same package, whose frames
// callerFromStdLog skips.

// ---------- 5. Levels ----------

// Nothing below the minimum level is written.
func TestLevelFiltersLowerSeverity(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithName("app"), WithLevel(LevelWarn))

	l.Debugf("must not appear")
	l.Infof("must not appear either")
	l.Warnf("must appear")
	l.Errorf("must appear")

	got := readFile(t, dirOf(t, l), "app-2026-10-25.log")
	for _, bad := range []string{"DEBUG", "must not appear", "must not appear either"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q should not be present: %q", bad, got)
		}
	}
	if !strings.Contains(got, "WARN") || !strings.Contains(got, "ERROR") {
		t.Errorf("WARN and ERROR are expected: %q", got)
	}
}

// At LevelDebug every level reaches the file: filtering must not end up
// dropping the severe ones as well.
func TestAllLevelsReachTheFileAtDebug(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithLevel(LevelDebug))

	l.Debugf("d")
	l.Infof("i")
	l.Warnf("w")
	l.Errorf("e")

	got := readFile(t, dirOf(t, l), "app-2026-10-25.log")
	for _, want := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s is expected: %q", want, got)
		}
	}
}

// Panic writes the record first: once it returns there is nothing left to
// flush.
func TestPanicWritesThenPanics(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithName("app"))
	dir := dirOf(t, l)

	recovered := func() (v any) {
		defer func() { v = recover() }()
		l.Panicf("about to panic: %d", 42)
		return nil
	}()
	if recovered == nil {
		t.Fatal("Panicf should panic")
	}
	got := readFile(t, dir, "app-2026-10-25.log")
	if !strings.Contains(got, "PANIC") || !strings.Contains(got, "about to panic: 42") {
		t.Fatalf("record should be written before panicking: %q", got)
	}
}

// ---------- 6. Context fields ----------

// Fields carried by the context show up on the record.
func TestContextFieldsAppearInLine(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithName("app"))

	ctx := WithFields(nil, "trace_id", "abc-123", "user_id", 7)
	l.InfoContextf(ctx, "order created: %s", "A001")

	got := readFile(t, dirOf(t, l), "app-2026-10-25.log")
	for _, want := range []string{"trace_id=abc-123", "user_id=7", "order created: A001"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}

// Fields accumulate layer by layer: middleware adds trace_id, the handler adds
// user_id.
func TestContextFieldsAccumulate(t *testing.T) {
	ctx := WithFields(nil, "trace_id", "abc")
	ctx = WithFields(ctx, "user_id", 7)

	fields := FieldsFrom(ctx)
	if len(fields) != 2 || fields[0].Key != "trace_id" || fields[1].Key != "user_id" {
		t.Fatalf("fields should accumulate in insertion order: %+v", fields)
	}
	// FieldsFrom returns a copy, so mutating it cannot touch the context.
	fields[0].Value = "mutated"
	if got := FieldsFrom(ctx)[0].Value; got != "abc" {
		t.Fatalf("FieldsFrom should return a copy, got %v", got)
	}
}

// An odd number of arguments means the caller forgot a value: the key is kept
// with a nil value instead of panicking.
func TestWithFieldsToleratesMissingValue(t *testing.T) {
	ctx := WithFields(nil, "trace_id", "abc", "user_id")
	fields := FieldsFrom(ctx)
	if len(fields) != 2 || fields[1].Value != nil {
		t.Fatalf("a missing value should be recorded as nil: %+v", fields)
	}
}

// ---------- Formatting ----------

// Text records carry millisecond timestamps and quote values containing
// whitespace.
func TestTextFormatterShape(t *testing.T) {
	rec := Record{
		Time:   time.Date(2026, 10, 25, 15, 4, 5, 123000000, time.UTC),
		Level:  LevelInfo,
		Msg:    "startup complete",
		Caller: "main.go:42",
		Fields: []Field{{Key: "who", Value: "zhang san"}, {Key: "n", Value: 3}},
	}
	got := string(TextFormatter{}.Format(rec))
	want := "2026-10-25 15:04:05.123 INFO main.go:42 who=\"zhang san\" n=3 startup complete\n"
	if got != want {
		t.Fatalf("text line:\n got %q\nwant %q", got, want)
	}
}

// JSON fields sit at the top level and never overwrite reserved keys, which
// are prefixed with "field." instead.
func TestJSONFormatterShape(t *testing.T) {
	rec := Record{
		Time:   time.Date(2026, 10, 25, 15, 4, 5, 0, time.UTC),
		Level:  LevelError,
		Msg:    "failed",
		Caller: "main.go:42",
		Fields: []Field{{Key: "trace_id", Value: "abc"}, {Key: "msg", Value: "collides with msg"}},
	}
	var got map[string]any
	if err := json.Unmarshal(JSONFormatter{}.Format(rec), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["level"] != "error" {
		t.Errorf("level should be lower case: %v", got["level"])
	}
	if got["caller"] != "main.go:42" || got["trace_id"] != "abc" {
		t.Errorf("field lost: %v", got)
	}
	if got["msg"] != "failed" {
		t.Errorf("reserved key msg must not be overwritten: %v", got["msg"])
	}
	if got["field.msg"] != "collides with msg" {
		t.Errorf("colliding field should survive under a prefix: %v", got)
	}
}

// A value that cannot be marshalled degrades to its string form instead of
// taking the whole record down.
func TestJSONFormatterSurvivesUnserializableValue(t *testing.T) {
	rec := Record{Level: LevelInfo, Msg: "ok", Fields: []Field{{Key: "ch", Value: make(chan int)}}}
	var got map[string]any
	if err := json.Unmarshal(JSONFormatter{}.Format(rec), &got); err != nil {
		t.Fatalf("one bad field destroyed the record: %v", err)
	}
	if got["msg"] != "ok" {
		t.Errorf("the rest of the record should still be there: %v", got)
	}
}

// A percent sign in a literal message is not treated as a verb.
func TestMessageWithPercentIsKept(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now)

	// Assigned to a variable to keep go vet from flagging the constant format
	// string: that no Sprintf happens without arguments is exactly what is
	// under test here.
	msg := "download complete: 100%"
	l.Infof(msg)

	if got := readFile(t, dirOf(t, l), "app-2026-10-25.log"); !strings.Contains(got, msg) {
		t.Fatalf("percent was treated as a verb: %q", got)
	}
}

// WithJSON selects the JSON formatter.
func TestWithJSONSwitchesFormat(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	l, _ := newTestLogger(t, now, WithJSON())
	l.Infof("a JSON line")

	var got map[string]any
	if err := json.Unmarshal([]byte(readFile(t, dirOf(t, l), "app-2026-10-25.log")), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["msg"] != "a JSON line" {
		t.Errorf("missing msg: %v", got)
	}
}

// ---------- Argument validation ----------

// An empty directory, a path that is not a directory, and a name containing a
// separator are all rejected while constructing the Logger.
func TestNewRejectsBadInput(t *testing.T) {
	if _, err := New("   "); err == nil {
		t.Error("an empty directory should be rejected")
	}
	filePath := filepath.Join(t.TempDir(), "iamafile")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filePath); err == nil {
		t.Error("a path that is not a directory should be rejected")
	}
	for _, name := range []string{"a/b", `c:\logs`, ".", ".."} {
		if _, err := New(t.TempDir(), WithName(name)); err == nil {
			t.Errorf("file name %q should be rejected", name)
		}
	}
}

// Parsing accepts both warn and warning, and reports an error for anything
// else instead of falling back silently.
func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": LevelDebug, "info": LevelInfo, "warn": LevelWarn,
		"warning": LevelWarn, "ERROR": LevelError, "fatal": LevelFatal, "panic": LevelPanic,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("an unknown level should be reported")
	}
}

func Test_setdefault(t *testing.T) {
	SetDefault(NewStderr())
	log.Printf("hi")
}
func Test_setdefault2(t *testing.T) {
	SetDefault(NewStdout())
	log.Printf("hi")
	log.Printf(os.Getwd())
}

func Test_setdefault3(t *testing.T) {
	d, _ := os.Getwd()
	l, err := Init(d+"/_logs", WithMirror(os.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	SetDefault(l)
	log.Printf("hi")
}
