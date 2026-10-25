package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fileWriter writes bytes into the file belonging to the current day.
//
// Rotation is decided per write: when the day has changed since the last
// write, the previous file is closed and the new one opened. A midnight timer
// would need a background goroutine and would still miss writes that fall
// around the boundary; per-write checks cost one time.Now and one Format, and
// involve neither.
type fileWriter struct {
	mu sync.Mutex

	dir        string
	name       string
	maxAgeDays int
	// now is replaceable so tests can exercise rotation and cleanup without
	// waiting for a real day boundary.
	now func() time.Time

	file       *os.File
	day        string
	cleanedDay string
}

// newFileWriter returns a writer that rolls over daily. A maxAgeDays of 0 or
// less disables cleanup.
func newFileWriter(dir, name string, maxAgeDays int) *fileWriter {
	return &fileWriter{dir: dir, name: name, maxAgeDays: maxAgeDays, now: time.Now}
}

// Write implements io.Writer and is safe for concurrent use: only one
// goroutine at a time holds the file.
func (w *fileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := w.today()
	if err := w.ensureFile(day); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

// Close closes the file currently open.
func (w *fileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLocked()
}

func (w *fileWriter) closeLocked() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	w.day = ""
	return err
}

func (w *fileWriter) today() string { return w.now().Format(DateLayout) }

// ensureFile opens the file for day unless it is already open.
func (w *fileWriter) ensureFile(day string) error {
	if w.file != nil && w.day == day {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := filepath.Join(w.dir, w.name+"-"+day+".log")
	// O_APPEND is what keeps concurrent writers - or a second open of the same
	// file - from overwriting each other's records.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", path, err)
	}
	w.file = f
	w.day = day

	// Cleanup runs once per day, on the first write after the change: scanning
	// the directory more often would cost I/O without changing the outcome.
	if w.cleanedDay != day {
		w.cleanedDay = day
		w.cleanupLocked()
	}
	return nil
}

// cleanupLocked removes files that have not been written to for maxAgeDays.
// The caller must hold mu.
//
// Age is read from the modification time rather than from the date in the name:
// the name may have been produced in another time zone or edited by hand,
// while mtime records how long the file has actually been idle.
//
// Only files matching <name>-YYYY-MM-DD.log exactly are candidates; everything
// else in the directory is left untouched.
func (w *fileWriter) cleanupLocked() {
	if w.maxAgeDays <= 0 {
		return
	}
	cutoff := w.now().AddDate(0, 0, -w.maxAgeDays)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		warnf("read log directory %q, skipping cleanup: %v", w.dir, err)
		return
	}

	prefix := w.name + "-"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".log")
		if _, err := time.Parse(DateLayout, date); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
				warnf("remove stale log %q: %v", name, err)
			}
		}
	}
}

// warnf reports failures within this package on stderr.
//
// It deliberately avoids this package's Logger: a logging component reporting
// through itself is where recursion starts.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "logging: "+format+"\n", args...)
}
