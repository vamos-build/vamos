package telemetry

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/vamos-build/vamos/internal/tracing"
)

// mustBridge returns newBridge's bridge, failing tb if it can't be made.
func mustBridge(tb testing.TB, tracer trace.Tracer, s sampler, instance string, meter metric.Meter) *Bridge {
	tb.Helper()
	b, err := newBridge(tracer, s, instance, meter)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// measuredSpans are an attempt's spans, a wait and a try that used resources, and a job's targets, one of each
// outcome that is counted, and one hit's copy of an attempt's, which isn't.
func measuredSpans() (attempt, job []tracing.Span) {
	t0 := time.Unix(1000, 0)
	span := func(id string, d time.Duration, args ...any) tracing.Span {
		s := tracing.Span{ID: id, Start: t0, End: t0.Add(d), Args: map[string]any{}}
		for i := 0; i+1 < len(args); i += 2 {
			s.Args[args[i].(string)] = args[i+1]
		}
		return s
	}
	target := func(id string, args ...any) tracing.Span {
		return span(id, time.Second, append([]any{tracing.Role, tracing.RoleTarget, "kind", "layer"}, args...)...)
	}
	attempt = []tracing.Span{
		span("w", 3*time.Second, tracing.Role, tracing.RoleWait),
		span("t", 5*time.Second, tracing.Role, tracing.RoleTry, "platform", "linux/amd64", "kind", "layer",
			"outcome", "succeeded", "cpu", 1.5, "memory peak", float64(64<<20)), // as decoded from JSON
		span("other", time.Hour), // measured by nothing
	}
	job = []tracing.Span{
		target("hit", "outcome", "hit", "saved", "2s"),
		target("miss", "outcome", "miss", "miss", "new"),
		target("lost", "outcome", "miss", "miss", "lost", "lost", "missing"),
		target("joined", "outcome", "joined"),
		target("missed then joined", "outcome", "joined", "miss", "upstream"),                 // a join, not a miss
		target("joined then failed", "outcome", "failed", "miss", "upstream", "joined", true), // a join still
		// counted as its retry is
		target("retried", "outcome", "retried", "miss", "upstream"),
		target("failed", "outcome", "failed"),
		target("missed then failed", "outcome", "failed", "miss", "own-inputs"), // a miss, though its run failed
		target("copy", "outcome", "hit", tracing.Shared, true),
	}
	return attempt, job
}

// collect returns the metrics reader has, by name.
func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = m.Data
		}
	}
	return got
}

// measuringBridge returns a bridge that measures into the reader it returns, keeping every trace if sampled, and
// none otherwise, and the context of a build job it started.
func measuringBridge(t *testing.T, sampled bool) (*Bridge, *sdkmetric.ManualReader, context.Context) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tracer, _ := newTestTracer(t)
	s := sampler{Sampler: sdktrace.NeverSample()}
	if sampled {
		s = sampler{Sampler: sdktrace.AlwaysSample()}
	}
	b := mustBridge(t, tracer, s, "i", provider.Meter(meterName))
	t.Cleanup(func() { _ = b.close(context.Background()) })
	return b, reader, b.StartJob(t.Context(), JobInfo{ID: "j", Kind: "build"})
}

// TestSpanMetrics checks what the bridge measures of an attempt's and a job's spans: the wait, the try's duration, CPU
// and memory, by platform, kind and outcome; the job's duration by kind and outcome; and the targets' hits, misses by
// cause, joins, what hits saved and results lost by reason, by kind, but for a copy of an attempt's span; and that
// a sampled span is each one's exemplar.
func TestSpanMetrics(t *testing.T) {
	b, reader, ctx := measuringBridge(t, true)
	attempt, job := measuredSpans()
	start := time.Unix(1000, 0)
	b.Attempt(ctx, "a", attempt, start, start.Add(10*time.Second))
	b.Job(ctx, job, start, start.Add(20*time.Second), "succeeded")
	got := collect(t, reader)

	try := attribute.NewSet(attribute.String("platform", "linux/amd64"), attribute.String("kind", "layer"),
		attribute.String("outcome", "succeeded"))
	usage := attribute.NewSet(attribute.String("platform", "linux/amd64"), attribute.String("kind", "layer"))
	jobSet := attribute.NewSet(attribute.String("kind", "build"), attribute.String("outcome", "succeeded"))
	for _, c := range []struct {
		name string
		set  attribute.Set
		sum  float64
		span string
	}{
		{"vamos.attempts.wait", attribute.NewSet(), 3, "w"},
		{"vamos.attempts.duration", try, 5, "t"},
		{"vamos.attempts.cpu", usage, 1.5, "t"},
		{"vamos.attempts.memory.peak", usage, 64 << 20, "t"},
	} {
		checkHistogram(t, got, c.name, c.set, c.sum, spanIDOf("i", "j", c.span))
	}
	checkHistogram(t, got, "vamos.jobs.duration", jobSet, 20, jobSpanIDOf("i", "j", 0))

	layer := attribute.String("kind", "layer")
	for _, c := range []struct {
		name string
		set  attribute.Set
		want int64
	}{
		{"vamos.cache.hits", attribute.NewSet(layer), 1},
		{"vamos.cache.misses", attribute.NewSet(layer, attribute.String("cause", "new")), 1},
		{"vamos.cache.misses", attribute.NewSet(layer, attribute.String("cause", "lost")), 1},
		{"vamos.cache.misses", attribute.NewSet(layer, attribute.String("cause", "own-inputs")), 1},
		{"vamos.cache.joins", attribute.NewSet(layer), 3},
		{"vamos.cache.misses", attribute.NewSet(layer, attribute.String("cause", "upstream")), 0},
		{"vamos.cache.lost", attribute.NewSet(layer, attribute.String("reason", "missing")), 1},
	} {
		sum, ok := got[c.name].(metricdata.Sum[int64])
		i := slices.IndexFunc(sum.DataPoints, func(p metricdata.DataPoint[int64]) bool {
			return p.Attributes.Equals(&c.set)
		})
		if !ok || i < 0 || sum.DataPoints[i].Value != c.want {
			t.Errorf("%s for %v = %+v; want %d", c.name, c.set.ToSlice(), got[c.name], c.want)
		}
	}
	saved, _ := got["vamos.cache.saved"].(metricdata.Sum[float64])
	layerSet := attribute.NewSet(layer)
	for _, p := range saved.DataPoints {
		want := 0.0 // every kind's is exported from zero
		if p.Attributes.Equals(&layerSet) {
			want = 2 // the hit's alone, not its copy's
		}
		if p.Value != want {
			t.Errorf("vamos.cache.saved for %v = %v; want %v", p.Attributes.ToSlice(), p.Value, want)
		}
	}
	if len(saved.DataPoints) != len(targetKinds) {
		t.Errorf("vamos.cache.saved has %d points; want one for each kind, %v", len(saved.DataPoints), targetKinds)
	}
}

// checkHistogram checks that got's histogram name has one point for set, of one measurement whose sum is sum, and
// whose exemplar, if span isn't zero, names span in the job's trace, or, if it is, that it has none.
func checkHistogram(t *testing.T, got map[string]metricdata.Aggregation, name string, set attribute.Set, sum float64,
	span trace.SpanID) {
	t.Helper()
	h, ok := got[name].(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 {
		t.Errorf("%s = %+v; want one point", name, got[name])
		return
	}
	p := h.DataPoints[0]
	if !p.Attributes.Equals(&set) || p.Count != 1 || p.Sum != sum {
		t.Errorf("%s = %v, count %d, sum %v; want %v, count 1, sum %v", name, p.Attributes.ToSlice(), p.Count, p.Sum,
			set.ToSlice(), sum)
	}
	var exemplars []trace.SpanID
	for _, e := range p.Exemplars {
		var id trace.SpanID
		copy(id[:], e.SpanID)
		exemplars = append(exemplars, id)
		if tid := traceIDOf("i", "j"); string(e.TraceID) != string(tid[:]) {
			t.Errorf("%s's exemplar is in the trace %x; want %s", name, e.TraceID, tid)
		}
	}
	want := []trace.SpanID{span}
	if !span.IsValid() {
		want = nil
	}
	if !slices.Equal(exemplars, want) {
		t.Errorf("%s's exemplars name the spans %v; want %v", name, exemplars, want)
	}
}

// TestSpanMetricsUnsampled checks that the spans of a trace that isn't kept are measured all the same, with no
// exemplar, since none leads anywhere.
func TestSpanMetricsUnsampled(t *testing.T) {
	b, reader, ctx := measuringBridge(t, false)
	attempt, job := measuredSpans()
	start := time.Unix(1000, 0)
	b.Attempt(ctx, "a", attempt, start, start.Add(10*time.Second))
	b.Job(ctx, job, start, start.Add(20*time.Second), "failed")
	got := collect(t, reader)
	try := attribute.NewSet(attribute.String("platform", "linux/amd64"), attribute.String("kind", "layer"),
		attribute.String("outcome", "succeeded"))
	checkHistogram(t, got, "vamos.attempts.duration", try, 5, trace.SpanID{})
	checkHistogram(t, got, "vamos.jobs.duration",
		attribute.NewSet(attribute.String("kind", "build"), attribute.String("outcome", "failed")), 20,
		trace.SpanID{})
}

// TestSpanMetricsOfAJobsOwnTries checks that the tries of work a job gave the pool itself, as a push does, which its
// own trace holds, are measured, but for a copy of an attempt's.
func TestSpanMetricsOfAJobsOwnTries(t *testing.T) {
	b, reader, ctx := measuringBridge(t, true)
	start := time.Unix(1000, 0)
	try := func(id string, args ...any) tracing.Span {
		s := tracing.Span{ID: id, Start: start, End: start.Add(4 * time.Second), Args: map[string]any{
			tracing.Role: tracing.RoleTry, "platform": "any", "kind": "push", "outcome": "succeeded"}}
		for i := 0; i+1 < len(args); i += 2 {
			s.Args[args[i].(string)] = args[i+1]
		}
		return s
	}
	b.Job(ctx, []tracing.Span{try("push"), try("copy", tracing.Shared, true)}, start, start.Add(5*time.Second),
		"succeeded")
	push := attribute.NewSet(attribute.String("platform", "any"), attribute.String("kind", "push"),
		attribute.String("outcome", "succeeded"))
	checkHistogram(t, collect(t, reader), "vamos.attempts.duration", push, 4, spanIDOf("i", "j", "push"))
}

// TestSpanMetricsOfADroppedSnapshot checks that the spans of a snapshot the bridge drops, as it does every later one
// of a job that lost one, are measured all the same, with no exemplar, since the span it would name isn't exported.
func TestSpanMetricsOfADroppedSnapshot(t *testing.T) {
	b, reader, ctx := measuringBridge(t, true)
	b.queueMu.Lock()
	jobOf(ctx).cut = true // it lost a snapshot before
	b.queueMu.Unlock()
	attempt, _ := measuredSpans()
	start := time.Unix(1000, 0)
	b.Attempt(ctx, "a", attempt, start, start.Add(10*time.Second))
	try := attribute.NewSet(attribute.String("platform", "linux/amd64"), attribute.String("kind", "layer"),
		attribute.String("outcome", "succeeded"))
	checkHistogram(t, collect(t, reader), "vamos.attempts.duration", try, 5, trace.SpanID{})
}

// TestSpanMetricsOffWithoutAMeter checks that a bridge given no meter reads no span to measure it.
func TestSpanMetricsOffWithoutAMeter(t *testing.T) {
	if m, err := newSpanMetrics(nil); err != nil || m.enabled {
		t.Errorf("span metrics without a meter = %+v, %v; want them off", m, err)
	}
}
