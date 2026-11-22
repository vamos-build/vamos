package telemetry

import (
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestSampler checks each sampler's decision for jobs of their own and jobs that joined an outer trace, sampled or
// not, that a ratio keeps about its share of traces, and that a job decides the same way each time it is asked.
func TestSampler(t *testing.T) {
	// The W3C example's trace ID, which a ratio of 0.5 drops whatever its parent says,
	// since the ratio samplers take the trace ID's low 63 bits, here above half their range.
	outer, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sampledParent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    outer,
		SpanID:     jobSpanIDOf("i", "outer"),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	unsampledParent := sampledParent.WithTraceFlags(0)
	cases := []struct {
		name, sampler, arg string
		// kept is how many of 1000 jobs of their own it keeps, at least and at most.
		keptAtLeast, keptAtMost int
		// sampledOuter and unsampledOuter are whether it keeps a job that joined a sampled or an unsampled trace.
		sampledOuter, unsampledOuter bool
	}{
		{name: "unset", keptAtLeast: 1000, keptAtMost: 1000, sampledOuter: true},
		{name: "always_on", sampler: "always_on", keptAtLeast: 1000, keptAtMost: 1000,
			sampledOuter: true, unsampledOuter: true},
		{name: "always_off", sampler: "always_off"},
		{name: "ratio 0", sampler: "traceidratio", arg: "0"},
		{name: "ratio 0.5", sampler: "traceidratio", arg: "0.5", keptAtLeast: 400, keptAtMost: 600},
		{name: "parentbased_traceidratio 0.5", sampler: "parentbased_traceidratio", arg: "0.5",
			keptAtLeast: 400, keptAtMost: 600, sampledOuter: true},
		{name: "ratio 1", sampler: "traceidratio", arg: "1", keptAtLeast: 1000, keptAtMost: 1000,
			sampledOuter: true, unsampledOuter: true},
		{name: "ratio unset", sampler: "traceidratio", keptAtLeast: 1000, keptAtMost: 1000,
			sampledOuter: true, unsampledOuter: true},
		{name: "parentbased_always_off", sampler: "parentbased_always_off", sampledOuter: true},
		{name: "parentbased_traceidratio 0", sampler: "parentbased_traceidratio", arg: "0", sampledOuter: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := readSampler(envOf(map[string]string{"OTEL_TRACES_SAMPLER": c.sampler,
				"OTEL_TRACES_SAMPLER_ARG": c.arg}))
			if err != nil {
				t.Fatal(err)
			}
			kept := 0
			for i := range 1000 {
				tid := traceIDOf("i", fmt.Sprint("job-", i))
				first := s.sampled(tid, trace.SpanContext{})
				if again := s.sampled(tid, trace.SpanContext{}); again != first {
					t.Fatalf("job-%d decided %t, then %t", i, first, again)
				}
				if first {
					kept++
				}
			}
			if kept < c.keptAtLeast || kept > c.keptAtMost {
				t.Errorf("kept %d of 1000 jobs, want %d to %d", kept, c.keptAtLeast, c.keptAtMost)
			}
			if got := s.sampled(sampledParent.TraceID(), sampledParent); got != c.sampledOuter {
				t.Errorf("a job under a sampled outer trace is kept: %t, want %t", got, c.sampledOuter)
			}
			if got := s.sampled(unsampledParent.TraceID(), unsampledParent); got != c.unsampledOuter {
				t.Errorf("a job under an unsampled outer trace is kept: %t, want %t", got, c.unsampledOuter)
			}
		})
	}
}

// TestSamplerMistakes checks that a sampler or ratio it can't take fails, naming the variable and its value.
func TestSamplerMistakes(t *testing.T) {
	cases := []struct {
		name, sampler, arg, want string
	}{
		{
			name:    "an unknown sampler",
			sampler: "sometimes",
			want: "OTEL_TRACES_SAMPLER=sometimes: not a sampler; it takes always_on, always_off, traceidratio, " +
				"parentbased_always_on, parentbased_always_off or parentbased_traceidratio",
		},
		{
			name:    "a ratio over 1",
			sampler: "traceidratio",
			arg:     "1.5",
			want:    "OTEL_TRACES_SAMPLER_ARG=1.5: not a ratio for traceidratio; it takes a number from 0 to 1",
		},
		{
			name:    "a negative ratio",
			sampler: "parentbased_traceidratio",
			arg:     "-0.1",
			want: "OTEL_TRACES_SAMPLER_ARG=-0.1: not a ratio for parentbased_traceidratio; " +
				"it takes a number from 0 to 1",
		},
		{
			name:    "a ratio of NaN",
			sampler: "traceidratio",
			arg:     "NaN",
			want:    "OTEL_TRACES_SAMPLER_ARG=NaN: not a ratio for traceidratio; it takes a number from 0 to 1",
		},
		{
			name:    "a ratio that isn't a number",
			sampler: "traceidratio",
			arg:     "half",
			want:    "OTEL_TRACES_SAMPLER_ARG=half: not a ratio for traceidratio; it takes a number from 0 to 1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readSampler(envOf(map[string]string{"OTEL_TRACES_SAMPLER": c.sampler,
				"OTEL_TRACES_SAMPLER_ARG": c.arg}))
			if err == nil || err.Error() != c.want {
				t.Errorf("readSampler = %v, want %s", err, c.want)
			}
		})
	}
}
