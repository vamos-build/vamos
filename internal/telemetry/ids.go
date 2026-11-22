package telemetry

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"
)

// chosenIDs is an IDGenerator that gives a span the IDs that withIDs put in the context it starts under,
// so that an exported span keeps the IDs derived for it rather than random ones.
// A span started under a remote parent takes the parent's trace ID, and asks only for its span ID.
// The IDs are given once: the context a span's start returns still holds them,
// so a span started under that context, or one started under a context that holds none,
// gets random IDs, as the SDK's own generator would give it, rather than its parent's ID or zero IDs;
// so does a chosen ID that is zero, which no backend takes.
// That is a backstop, not how derived IDs avoid zero: a random ID would differ when a resumed job is exported again,
// so a derivation replaces a zero ID itself, the same way every time.
type chosenIDs struct{}

type idsKey struct{}

// spanIDs are the IDs chosen for a span, and whether a span has taken them.
type spanIDs struct {
	trace trace.TraceID
	span  trace.SpanID
	taken atomic.Bool
}

// withIDs returns ctx under which the next span started is given the span ID sid,
// and the trace ID tid if it has no parent.
// Only that one span is given them: call withIDs right before each start,
// since a span started again under the same context, such as a retry, gets random IDs.
func withIDs(ctx context.Context, tid trace.TraceID, sid trace.SpanID) context.Context {
	return context.WithValue(ctx, idsKey{}, &spanIDs{trace: tid, span: sid})
}

// take returns the IDs that ctx holds, if a span hasn't taken them already.
func take(ctx context.Context) (*spanIDs, bool) {
	ids, ok := ctx.Value(idsKey{}).(*spanIDs)
	return ids, ok && ids.taken.CompareAndSwap(false, true)
}

// NewIDs returns the IDs chosen for a span with no parent, or random ones for each that is missing or invalid.
func (chosenIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	var tid trace.TraceID
	var sid trace.SpanID
	if ids, ok := take(ctx); ok {
		tid, sid = ids.trace, ids.span
	}
	if !tid.IsValid() {
		tid = randomTraceID()
	}
	if !sid.IsValid() {
		sid = randomSpanID()
	}
	return tid, sid
}

// NewSpanID returns the span ID chosen for a span with a parent, or a random one if it is missing or invalid.
func (chosenIDs) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if ids, ok := take(ctx); ok && ids.span.IsValid() {
		return ids.span
	}
	return randomSpanID()
}

// randomTraceID returns a random trace ID, never the invalid zero one.
func randomTraceID() trace.TraceID {
	for {
		var tid trace.TraceID
		binary.LittleEndian.PutUint64(tid[:8], rand.Uint64())
		binary.LittleEndian.PutUint64(tid[8:], rand.Uint64())
		if tid.IsValid() {
			return tid
		}
	}
}

// randomSpanID returns a random span ID, never the invalid zero one.
func randomSpanID() trace.SpanID {
	for {
		var sid trace.SpanID
		binary.LittleEndian.PutUint64(sid[:], rand.Uint64())
		if sid.IsValid() {
			return sid
		}
	}
}
