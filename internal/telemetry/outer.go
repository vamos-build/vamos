package telemetry

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ErrNotTraceparent says that a TRACEPARENT, or a request's traceparent, isn't a W3C traceparent that a job can join.
var ErrNotTraceparent = errors.New("not a W3C traceparent")

// ErrNotTracestate says that a TRACESTATE, or a request's tracestate, beside a valid traceparent, isn't a W3C
// tracestate, which a job then joins the outer trace without.
var ErrNotTracestate = errors.New("not a W3C tracestate")

// OuterTrace returns parent and state, a W3C traceparent and tracestate, as a job joins them, as version 00, with the
// flags it defines, and the W3C format write them; or ErrNotTraceparent if parent isn't one, as
// propagation.TraceContext reads it, which takes a later version's first fields as the W3C recommends, but no all-zero
// ID, and version 00 of its exact length; or parent, no state and ErrNotTracestate if state, which isn't empty, isn't
// one.
// An empty parent is none, and gives no error.
func OuterTrace(parent, state string) (string, string, error) {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "", "", nil
	}
	// Version 00 is exactly its four fields, which the propagator doesn't hold it to.
	if strings.HasPrefix(parent, "00-") && len(parent) != len("00-")+32+len("-")+16+len("-")+2 {
		return "", "", ErrNotTraceparent
	}
	sc := outerSpanContext(parent, "")
	if !sc.IsValid() {
		return "", "", ErrNotTraceparent
	}
	// Written as version 00, its IDs and the flags the propagator read, those version 00 defines, so that a later
	// version's fields or flags that it passed over aren't kept, and what a job joins is bounded and reads again.
	parent = "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
	if strings.Trim(state, " \t") == "" { // the W3C's optional whitespace alone; a newline isn't
		return parent, "", nil
	}
	if len(state) > maxTracestateSize { // longer than any the W3C allows, refused before anything is copied
		return parent, "", ErrNotTracestate
	}
	// Without the members of spaces and tabs alone, which the W3C allows as empty and the parser refuses, each
	// trimmed of the optional whitespace around it, counting every list slot, empty or not, against the W3C's 32.
	var members []string
	slots := 0
	for m := range strings.SplitSeq(state, ",") {
		if slots++; slots > maxTracestateSlots {
			return parent, "", ErrNotTracestate
		}
		if m = strings.Trim(m, " \t"); m != "" {
			members = append(members, m)
		}
	}
	ts, err := trace.ParseTraceState(strings.Join(members, ","))
	if err != nil {
		return parent, "", ErrNotTracestate
	}
	return parent, ts.String(), nil
}

// outerSpanContext returns the span context that parent and state, a W3C traceparent and tracestate, give, which
// isn't valid if parent isn't one.
func outerSpanContext(parent, state string) trace.SpanContext {
	carrier := propagation.MapCarrier{"traceparent": parent, "tracestate": state}
	return trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
}

// maxTracestateSlots is how many list members, empty or not, a tracestate may have, as the W3C bounds it.
const maxTracestateSlots = 32

// maxTracestateSize bounds a tracestate's length: 32 members of a 256-byte key, "=" and a 256-byte value, the commas
// between them, and room for the optional whitespace around each.
const maxTracestateSize = maxTracestateSlots*(256+1+256) + (maxTracestateSlots - 1) + 1<<10

// maxShownTrace bounds how much of a traceparent or tracestate ShowTraceValue shows, as one may be a megabyte.
const maxShownTrace = 128

// ShowTraceValue returns value, a traceparent or tracestate that isn't one, as a warning shows it: quoted, and cut at
// a character's start to at most maxShownTrace bytes, so that one of any size or content can be reported.
func ShowTraceValue(value string) string {
	if len(value) <= maxShownTrace {
		return strconv.Quote(value)
	}
	n := maxShownTrace
	for n > 0 && !utf8.RuneStart(value[n]) {
		n--
	}
	if n == 0 { // no character starts within it, which Quote escapes byte by byte
		n = maxShownTrace
	}
	return strconv.Quote(value[:n]) + "…"
}
