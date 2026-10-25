package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DateLayout is the date part of a log file name, and the only input to
	// rotation.
	DateLayout = "2006-01-02"
	// DefaultTimeFormat is the timestamp used by text records.
	//
	// Milliseconds are the smallest unit that stays useful when measuring how
	// long something took; nanoseconds only lengthen every line.
	DefaultTimeFormat = "2006-01-02 15:04:05.000"
)

// Field is a key/value pair attached to a record, typically carried by a
// context (see WithFields).
type Field struct {
	Key   string
	Value any
}

// Record is one log entry.
//
// Fields keeps the order in which the caller added them: logs are read by
// humans, and where a key sits becomes muscle memory.
type Record struct {
	Time   time.Time
	Level  Level
	Msg    string
	Caller string
	Fields []Field
}

// Formatter renders a record into one complete line, including the trailing
// newline. Records are written to the sink one line at a time, so assembling
// the line here avoids a second buffering layer in the writer.
type Formatter interface {
	Format(rec Record) []byte
}

// ---------- Text ----------

// TextFormatter renders a line meant to be read by a person:
//
//	2026-10-25 15:04:05.000 INFO main.go:42 trace_id=abc listening on 8080
//
// Fields precede the message: scanning for a key is easier when it is not
// pushed past a message of arbitrary length.
//
// The zero value is usable.
type TextFormatter struct {
	// TimeFormat is the timestamp layout; DefaultTimeFormat when empty.
	TimeFormat string
	// NoCaller suppresses the caller field.
	NoCaller bool
}

// Format implements Formatter.
func (f TextFormatter) Format(rec Record) []byte {
	layout := f.TimeFormat
	if layout == "" {
		layout = DefaultTimeFormat
	}

	var b bytes.Buffer
	b.WriteString(rec.Time.Format(layout))
	b.WriteByte(' ')
	b.WriteString(rec.Level.String())
	if rec.Caller != "" && !f.NoCaller {
		b.WriteByte(' ')
		b.WriteString(rec.Caller)
	}
	for _, fld := range rec.Fields {
		b.WriteByte(' ')
		b.WriteString(fld.Key)
		b.WriteByte('=')
		b.WriteString(quoteFieldValue(fld.Value))
	}
	if rec.Msg != "" {
		b.WriteByte(' ')
		b.WriteString(rec.Msg)
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// quoteFieldValue renders a field value, quoting it only when necessary.
//
// A value containing whitespace, a quote or "=" has to be quoted, otherwise
// "who=a b" cannot be told apart from the key "b" following "who=a". Quoting
// everything instead would make every line noisy.
func quoteFieldValue(v any) string {
	s := fmt.Sprint(v)
	if s == "" || strings.ContainsAny(s, " \t\r\n\"'=") {
		return strconv.Quote(s)
	}
	return s
}

// ---------- JSON ----------

// JSONFormatter renders one JSON object per line, for ingestion by collectors
// such as Filebeat, Loki or Elasticsearch.
//
//	{"time":"2026-10-25T15:04:05Z","level":"info","caller":"main.go:42","trace_id":"abc","msg":"listening"}
//
// The zero value is usable.
type JSONFormatter struct {
	// TimeFormat is the timestamp layout; RFC3339 with nanoseconds when empty.
	TimeFormat string
}

// jsonReserved holds the keys JSON output owns.
var jsonReserved = map[string]bool{"time": true, "level": true, "msg": true, "caller": true}

// Format implements Formatter.
func (f JSONFormatter) Format(rec Record) []byte {
	layout := f.TimeFormat
	if layout == "" {
		layout = time.RFC3339Nano
	}

	m := make(map[string]any, len(rec.Fields)+4)
	m["time"] = rec.Time.Format(layout)
	m["level"] = rec.Level.shortString()
	if rec.Caller != "" {
		m["caller"] = rec.Caller
	}
	for _, fld := range rec.Fields {
		key := fld.Key
		// A colliding field is renamed, not overwritten: silently dropping the
		// caller's data is worse than a slightly odd key.
		if jsonReserved[key] {
			key = "field." + key
		}
		m[key] = jsonSafe(fld.Value)
	}
	m["msg"] = rec.Msg

	encoded, err := json.Marshal(m)
	if err != nil {
		// Reaching this point means not even the standard keys could be
		// encoded. Emitted rather than dropped, so the record is not lost.
		return []byte(fmt.Sprintf(
			`{"time":%q,"level":%q,"error":%q}`+"\n",
			rec.Time.Format(layout), rec.Level.shortString(), err.Error()))
	}
	return append(encoded, '\n')
}

// jsonSafe degrades values that json.Marshal cannot handle (channels, funcs,
// cyclic structures) to their string form. One bad field must not take the
// whole record down with it.
func jsonSafe(v any) any {
	if err := checkJSON(v); err != nil {
		return fmt.Sprint(v)
	}
	return v
}

func checkJSON(v any) error {
	_, err := json.Marshal(v)
	return err
}
