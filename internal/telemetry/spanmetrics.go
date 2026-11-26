package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/vamos-build/vamos/internal/tracing"
)

// meterName names the meter of every instrument a coordinator or a worker records.
const meterName = "github.com/vamos-build/vamos"

// durationBuckets are the bounds, in seconds, of the histograms of how long something took, from a hit's
// milliseconds to a long build's hour, rather than the SDK's defaults, which are for milliseconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800,
	3600}

// memoryBuckets are the bounds, in bytes, of the histogram of an attempt's peak memory: from 1 MiB to 64 GiB, by
// fours.
var memoryBuckets = []float64{1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20, 1 << 30, 4 << 30, 16 << 30, 64 << 30}

// spanMetrics are the instruments that the bridge records from the spans it is given, whether or not their trace is
// kept, under the context of the span each measures, so that the SDK attaches an exemplar of it when it is sampled.
type spanMetrics struct {
	enabled                           bool // a meter measures them; with none, the spans aren't read
	wait, duration, cpu, memory, jobs metric.Float64Histogram
	hits, misses, joins, lost         metric.Int64Counter
	saved                             metric.Float64Counter
}

// newSpanMetrics returns the instruments of meter that spans are measured with; with a nil meter, a no-op one's.
func newSpanMetrics(meter metric.Meter) (*spanMetrics, error) {
	m := &spanMetrics{enabled: meter != nil} // before a nil meter is replaced by a no-op one
	if meter == nil {
		meter = noop.NewMeterProvider().Meter(meterName)
	}
	var errs []error
	histogram := func(name, unit, description string, buckets []float64) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description),
			metric.WithExplicitBucketBoundaries(buckets...))
		errs = append(errs, err)
		return h
	}
	counter := func(name, description string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithUnit("{target}"), metric.WithDescription(description))
		errs = append(errs, err)
		return c
	}
	m.wait = histogram("vamos.attempts.wait", "s", "How long attempts waited for a worker.", durationBuckets)
	m.duration = histogram("vamos.attempts.duration", "s", "How long attempts ran on a worker, a try each.",
		durationBuckets)
	m.cpu = histogram("vamos.attempts.cpu", "s", "The CPU time attempts used, a try each.", durationBuckets)
	m.memory = histogram("vamos.attempts.memory.peak", "By", "The peak memory attempts used, a try each.",
		memoryBuckets)
	m.jobs = histogram("vamos.jobs.duration", "s", "How long jobs took, a run each.", durationBuckets)
	m.hits = counter("vamos.cache.hits", "Targets whose cached result was reused.")
	m.misses = counter("vamos.cache.misses", "Targets that had to run, by why.")
	m.joins = counter("vamos.cache.joins", "Targets that joined another job's attempt.")
	m.lost = counter("vamos.cache.lost", "Results found but unusable, by why.")
	saved, err := meter.Float64Counter("vamos.cache.saved", metric.WithUnit("s"),
		metric.WithDescription("How long the runs that hits reused took, less the hits' own time."))
	m.saved = saved
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// Every count is exported from zero, by each of the bounded values of its labels, so that a rate reads from the
	// coordinator's start and every series is there to see before anything happens.
	ctx := context.Background()
	for _, kind := range targetKinds {
		k := attribute.String("kind", kind)
		m.hits.Add(ctx, 0, metric.WithAttributes(k))
		m.joins.Add(ctx, 0, metric.WithAttributes(k))
		m.saved.Add(ctx, 0, metric.WithAttributes(k))
		for _, cause := range missCauses {
			m.misses.Add(ctx, 0, metric.WithAttributes(k, attribute.String("cause", cause)))
		}
		for _, reason := range lostReasons {
			m.lost.Add(ctx, 0, metric.WithAttributes(k, attribute.String("reason", reason)))
		}
	}
	return m, nil
}

// The bounded values of the cache counters' labels, as sched records them on target spans.
var (
	targetKinds = []string{"layer", "task", "pull"}
	missCauses  = []string{"forced", "new", "lost", "own-inputs", "upstream", "dependency", "unknown"}
	lostReasons = []string{"missing", "corrupt", "unusable"}
)

// attempt measures the wait and try spans among spans, an attempt's, which the job j started, or none if j is nil;
// each span is its measurement's exemplar if exported, as spans are when their snapshot is queued.
func (m *spanMetrics) attempt(instance string, j *job, spans []tracing.Span, exported bool) {
	m.tries(instance, j, spans, exported)
}

// tries measures the wait and try spans among spans, the job j's or an attempt's, but for copies of an attempt's.
func (m *spanMetrics) tries(instance string, j *job, spans []tracing.Span, exported bool) {
	if !m.enabled {
		return
	}
	for _, s := range spans {
		if s.Args[tracing.Shared] == true {
			continue
		}
		switch s.Args[tracing.Role] {
		case tracing.RoleWait:
			m.wait.Record(spanContextOf(instance, j, s, exported), seconds(s))
		case tracing.RoleTry:
			ctx := spanContextOf(instance, j, s, exported)
			platform, kind := stringArg(s, "platform"), stringArg(s, "kind")
			m.duration.Record(ctx, seconds(s), metric.WithAttributes(attribute.String("platform", platform),
				attribute.String("kind", kind), attribute.String("outcome", stringArg(s, "outcome"))))
			usage := metric.WithAttributes(attribute.String("platform", platform), attribute.String("kind", kind))
			if cpu, ok := numberArg(s, "cpu"); ok {
				m.cpu.Record(ctx, cpu, usage)
			}
			if peak, ok := numberArg(s, "memory peak"); ok {
				m.memory.Record(ctx, peak, usage)
			}
		}
	}
}

// job measures the run of the job j, which ended as outcome, from start to end, and the target spans among spans,
// its own, and the tries of the work it gave the pool itself, as a push, an upload or a tagging, which no attempt's
// trace holds.
func (m *spanMetrics) job(instance string, j *job, spans []tracing.Span, start, end time.Time, outcome string,
	exported bool) {
	if !m.enabled {
		return
	}
	m.tries(instance, j, spans, exported)
	if j != nil { // a job the bridge started, whose kind its duration is labelled with
		ctx := context.Background()
		if exported {
			ctx = trace.ContextWithSpanContext(ctx, j.spanContext(j.span))
		}
		m.jobs.Record(ctx, max(end.Sub(start), 0).Seconds(), metric.WithAttributes(
			attribute.String("kind", j.info.Kind), attribute.String("outcome", outcome)))
	}
	for _, s := range spans {
		if s.Args[tracing.Role] != tracing.RoleTarget || s.Args[tracing.Shared] == true {
			continue
		}
		ctx := spanContextOf(instance, j, s, exported)
		kind := attribute.String("kind", stringArg(s, "kind"))
		if reason := stringArg(s, "lost"); reason != "" {
			m.lost.Add(ctx, 1, metric.WithAttributes(kind, attribute.String("reason", reason)))
		}
		// A miss is counted by its cause, and a join by its mark, which the target's span keeps whatever its outcome
		// then becomes, as a failure of the run it missed into, or joined, makes it; one that joined is a join, not a
		// miss, and one retried is counted as its retry. A pull's miss names no cause, and one that fails is no longer
		// told from any other failure.
		outcome := stringArg(s, "outcome")
		cause, missed := s.Args["miss"].(string)
		joined := s.Args["joined"] == true || outcome == "joined"
		if joined {
			m.joins.Add(ctx, 1, metric.WithAttributes(kind))
		} else if outcome == "miss" || missed && outcome != "hit" && outcome != "retried" {
			if cause == "" {
				cause = "unknown"
			}
			m.misses.Add(ctx, 1, metric.WithAttributes(kind, attribute.String("cause", cause)))
		}
		if outcome == "hit" {
			m.hits.Add(ctx, 1, metric.WithAttributes(kind))
			if saved, err := time.ParseDuration(stringArg(s, "saved")); err == nil {
				m.saved.Add(ctx, saved.Seconds(), metric.WithAttributes(kind))
			}
		}
	}
}

// spanContextOf returns a context holding the span context that s is exported under in j's trace, if it is exported,
// sampled as j is, so that a measurement under it carries s as its exemplar; a background one otherwise.
func spanContextOf(instance string, j *job, s tracing.Span, exported bool) context.Context {
	if j == nil || !exported {
		return context.Background()
	}
	return trace.ContextWithSpanContext(context.Background(), j.spanContext(spanIDOf(instance, j.info.ID, s.ID)))
}

// seconds returns how long s took, in seconds, never below zero.
func seconds(s tracing.Span) float64 {
	return max(s.End.Sub(s.Start), 0).Seconds()
}

// stringArg returns s's argument key if it is a string, and "" otherwise.
func stringArg(s tracing.Span, key string) string {
	v, _ := s.Args[key].(string)
	return v
}

// numberArg returns s's argument key as a number, whether recorded in this process or decoded from JSON, as a worker's
// relayed spans are, and whether it is one.
func numberArg(s tracing.Span, key string) (float64, bool) {
	switch v := s.Args[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	}
	return 0, false
}
