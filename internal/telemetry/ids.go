package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
	"strconv"
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

// The IDs a job's export derives, each the first bytes of a SHA-256 of a label and what it names,
// so that a job resumed after a restart that kept the coordinator's state is exported under the same trace,
// and a joiner finds an attempt's span without asking its starter.

// traceIDOf returns the trace ID of the job id of the coordinator instance, when it joined no outer trace.
func traceIDOf(instance, job string) trace.TraceID {
	var tid trace.TraceID
	sum := hashOf("vamos/trace", instance, job)
	copy(tid[:], sum[:])
	if !tid.IsValid() {
		tid[len(tid)-1] ^= 1
	}
	return tid
}

// jobSpanIDOf returns the ID of the job's own span.
// Each run of a job, its first 0 and each resumption after a restart one more, has a span of its own, all in the
// job's one trace, so that a run cut short by a restart and the run that resumed it are two spans, not two of one ID;
// a first run's ID is derived without the run, as before resumptions had spans of their own.
func jobSpanIDOf(instance, job string, run int64) trace.SpanID {
	if run == 0 {
		return spanIDFrom(hashOf("vamos/job", instance, job))
	}
	return spanIDFrom(hashOf("vamos/job", instance, job, strconv.FormatInt(run, 10)))
}

// attemptSpanIDOf returns the ID of an attempt's span; attempt IDs are 128 random bits, unique across instances.
func attemptSpanIDOf(attempt string) trace.SpanID {
	return spanIDFrom(hashOf("vamos/attempt", attempt))
}

// spanIDOf returns the ID of the Vamos span whose ID is id, recorded by the job, or by an attempt the job started.
func spanIDOf(instance, job, id string) trace.SpanID {
	return spanIDFrom(hashOf("vamos/span", instance, job, id))
}

// hashOf returns the SHA-256 of label and parts, each after a NUL, which none of them holds.
func hashOf(label string, parts ...string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(label))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// spanIDFrom returns the span ID that sum begins with, with its last bit flipped if it would be zero,
// which OpenTelemetry takes as no ID, so that a derived ID is always valid and the same every time.
func spanIDFrom(sum [sha256.Size]byte) trace.SpanID {
	var sid trace.SpanID
	copy(sid[:], sum[:])
	if !sid.IsValid() {
		sid[len(sid)-1] ^= 1
	}
	return sid
}
