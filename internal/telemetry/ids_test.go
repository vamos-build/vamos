package telemetry

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestTracer returns a tracer that gives spans chosen IDs, as the bridge's does, and the exporter it ends spans to.
func newTestTracer(t *testing.T) (trace.Tracer, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(chosenIDs{}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithSyncer(exporter),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider.Tracer("test"), exporter
}

// TestChosenIDs proves the span path the bridge relies on against the pinned SDK:
// a span started under a remote parent, with IDs chosen through the context and its start and end given,
// is exported with exactly those IDs, that parent and those times.
// So is a span with no parent, which takes the chosen trace ID too.
func TestChosenIDs(t *testing.T) {
	tracer, exporter := newTestTracer(t)
	outerTrace, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	outerSpan, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ownTrace, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	chosenSpan, _ := trace.SpanIDFromHex("0807060504030201")
	start := time.Date(2026, 11, 22, 12, 0, 0, 123456789, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	outer := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    outerTrace,
		SpanID:     outerSpan,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})

	cases := []struct {
		name      string
		parent    trace.SpanContext // the zero value for none
		wantTrace trace.TraceID
	}{
		{
			name:      "under a remote parent",
			parent:    outer,
			wantTrace: outerTrace,
		},
		{
			name:      "without a parent",
			wantTrace: ownTrace,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exporter.Reset()
			ctx := context.Background()
			if c.parent.IsValid() {
				ctx = trace.ContextWithRemoteSpanContext(ctx, c.parent)
			}
			_, span := tracer.Start(withIDs(ctx, ownTrace, chosenSpan), "step", trace.WithTimestamp(start))
			span.End(trace.WithTimestamp(end))

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("exported %d spans, want 1", len(spans))
			}
			got := spans[0]
			if got.SpanContext.TraceID() != c.wantTrace {
				t.Errorf("trace ID = %s, want %s", got.SpanContext.TraceID(), c.wantTrace)
			}
			if got.SpanContext.SpanID() != chosenSpan {
				t.Errorf("span ID = %s, want %s", got.SpanContext.SpanID(), chosenSpan)
			}
			if !got.Parent.Equal(c.parent) {
				t.Errorf("parent = %+v, want %+v", got.Parent, c.parent)
			}
			if !got.StartTime.Equal(start) || !got.EndTime.Equal(end) {
				t.Errorf("times = %s..%s, want %s..%s", got.StartTime, got.EndTime, start, end)
			}
		})
	}
}

// TestChosenIDsAreGivenOnce checks that a span started under a context whose chosen IDs a span has taken,
// such as the context its parent's start returned, or the same context again, as a retry would,
// or under a context with none, gets valid random IDs of its own,
// rather than its parent's span ID, the first span's or zero IDs;
// and that a zero ID chosen is replaced by a random one, keeping the other ID chosen.
func TestChosenIDsAreGivenOnce(t *testing.T) {
	tracer, exporter := newTestTracer(t)
	ownTrace, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	chosenSpan, _ := trace.SpanIDFromHex("0807060504030201")

	chosen := withIDs(context.Background(), ownTrace, chosenSpan)
	ctx, parent := tracer.Start(chosen, "parent")
	_, child := tracer.Start(ctx, "child")
	child.End()
	parent.End()
	_, retry := tracer.Start(chosen, "retry")
	retry.End()
	_, other := tracer.Start(context.Background(), "without chosen IDs")
	other.End()
	_, zero := tracer.Start(withIDs(context.Background(), trace.TraceID{}, trace.SpanID{}), "zero IDs chosen")
	zero.End()
	_, zeroTrace := tracer.Start(withIDs(context.Background(), trace.TraceID{}, chosenSpan), "zero trace ID chosen")
	zeroTrace.End()
	_, zeroSpan := tracer.Start(withIDs(context.Background(), ownTrace, trace.SpanID{}), "zero span ID chosen")
	zeroSpan.End()
	_, zeroChild := tracer.Start(withIDs(ctx, ownTrace, trace.SpanID{}), "zero span ID chosen under a parent")
	zeroChild.End()

	spans := exporter.GetSpans()
	if len(spans) != 8 {
		t.Fatalf("exported %d spans, want 8", len(spans))
	}
	got := map[string]tracetest.SpanStub{}
	pairs := map[trace.SpanID]map[trace.TraceID]string{} // the span that has each pair of IDs
	for _, s := range spans {
		got[s.Name] = s
		sc := s.SpanContext
		if pairs[sc.SpanID()] == nil {
			pairs[sc.SpanID()] = map[trace.TraceID]string{}
		}
		if other, ok := pairs[sc.SpanID()][sc.TraceID()]; ok {
			t.Errorf("%s and %s are both %s/%s, want IDs of their own", s.Name, other, sc.TraceID(), sc.SpanID())
		}
		pairs[sc.SpanID()][sc.TraceID()] = s.Name
	}
	if c := got["child"]; c.SpanContext.TraceID() != ownTrace || c.Parent.SpanID() != chosenSpan ||
		!c.SpanContext.SpanID().IsValid() || c.SpanContext.SpanID() == chosenSpan {
		t.Errorf("child = %s/%s under %s, want a new valid span ID in trace %s under %s",
			c.SpanContext.TraceID(), c.SpanContext.SpanID(), c.Parent.SpanID(), ownTrace, chosenSpan)
	}
	seen := map[string]trace.SpanContext{}
	for _, name := range []string{"retry", "without chosen IDs", "zero IDs chosen"} {
		sc := got[name].SpanContext
		if !sc.TraceID().IsValid() || !sc.SpanID().IsValid() || sc.TraceID() == ownTrace || sc.SpanID() == chosenSpan {
			t.Errorf("%s has %s/%s, want valid random IDs", name, sc.TraceID(), sc.SpanID())
		}
		for otherName, other := range seen {
			if other.TraceID() == sc.TraceID() || other.SpanID() == sc.SpanID() {
				t.Errorf("%s and %s share an ID: %s/%s and %s/%s, want random IDs of their own",
					name, otherName, sc.TraceID(), sc.SpanID(), other.TraceID(), other.SpanID())
			}
		}
		seen[name] = sc
	}
	// A zero ID is replaced alone: the other chosen ID is kept.
	if sc := got["zero trace ID chosen"].SpanContext; !sc.TraceID().IsValid() || sc.SpanID() != chosenSpan {
		t.Errorf("zero trace ID chosen has %s/%s, want a random trace ID and span %s",
			sc.TraceID(), sc.SpanID(), chosenSpan)
	}
	if sc := got["zero span ID chosen"].SpanContext; sc.TraceID() != ownTrace || !sc.SpanID().IsValid() {
		t.Errorf("zero span ID chosen has %s/%s, want trace %s and a random span ID",
			sc.TraceID(), sc.SpanID(), ownTrace)
	}
	c := got["zero span ID chosen under a parent"]
	if c.SpanContext.TraceID() != ownTrace || c.Parent.SpanID() != chosenSpan || !c.SpanContext.SpanID().IsValid() {
		t.Errorf("zero span ID chosen under a parent has %s/%s under %s, want a random span ID in trace %s under %s",
			c.SpanContext.TraceID(), c.SpanContext.SpanID(), c.Parent.SpanID(), ownTrace, chosenSpan)
	}
}
