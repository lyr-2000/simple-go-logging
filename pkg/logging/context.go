package logging

import (
	"context"
	"fmt"
)

// fieldsKey stores fields in a context. It is a private type so that no other
// package can construct a key equal to ours.
type fieldsKey struct{}

// WithFields returns a copy of ctx carrying the given fields.
//
//	ctx = logging.WithFields(ctx, "trace_id", tid, "user_id", uid)
//	logging.InfoContextf(ctx, "order created: %s", orderID)
//
// Arguments are key/value pairs. Repeated keys are kept as they are:
// deduplicating here would leave the caller unable to tell which of the two
// writes took effect.
//
// A trailing key with no value is recorded with a nil value instead of
// panicking in the caller's request path.
func WithFields(ctx context.Context, kv ...any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	existing := FieldsFrom(ctx)
	merged := make([]Field, 0, len(existing)+len(kv)/2+1)
	merged = append(merged, existing...)

	for i := 0; i+1 < len(kv); i += 2 {
		merged = append(merged, Field{Key: fieldKey(kv[i]), Value: kv[i+1]})
	}
	if len(kv)%2 == 1 {
		last := kv[len(kv)-1]
		merged = append(merged, Field{Key: fieldKey(last), Value: nil})
	}
	return context.WithValue(ctx, fieldsKey{}, merged)
}

// FieldsFrom returns the fields attached to ctx, in the order they were added.
func FieldsFrom(ctx context.Context) []Field {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(fieldsKey{}).([]Field)
	// Return a copy: sharing the slice would let a caller mutate the fields
	// stored in the context, possibly while a record is being formatted.
	out := make([]Field, len(v))
	copy(out, v)
	return out
}

// fieldKey renders a field key. Non-string keys are stringified rather than
// rejected: a mistyped key should make one line look odd, not crash the caller.
func fieldKey(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}
