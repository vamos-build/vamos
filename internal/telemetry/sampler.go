package telemetry

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// sampler decides whether a job's trace is kept, once, as the job starts.
type sampler struct {
	sdktrace.Sampler
}

// samplersTaken are the values OTEL_TRACES_SAMPLER takes, as its error names them.
var samplersTaken = []string{
	"always_on", "always_off", "traceidratio",
	"parentbased_always_on", "parentbased_always_off", "parentbased_traceidratio",
}

// readSampler returns the sampler that OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG name,
// parentbased_always_on if they are unset, or the first mistake, naming the variable and its value.
// vamos parses them rather than the SDK, which reads them from the process's environment and falls back to its default
// with only a log line for a value it can't take, where a mistake should fail the start.
func readSampler(getenv func(string) string) (sampler, error) {
	name := strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER")))
	if name == "" {
		name = "parentbased_always_on"
	}
	base, parentBased := strings.CutPrefix(name, "parentbased_")
	var s sdktrace.Sampler
	switch base {
	case "always_on":
		s = sdktrace.AlwaysSample()
	case "always_off":
		s = sdktrace.NeverSample()
	case "traceidratio":
		ratio := 1.0
		if arg := strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER_ARG")); arg != "" {
			var err error
			ratio, err = strconv.ParseFloat(arg, 64)
			if err != nil || !(ratio >= 0 && ratio <= 1) { // as NaN is neither
				return sampler{}, fmt.Errorf(
					"OTEL_TRACES_SAMPLER_ARG=%s: not a ratio for %s; it takes a number from 0 to 1", arg, name)
			}
		}
		s = sdktrace.TraceIDRatioBased(ratio)
	default:
		return sampler{}, fmt.Errorf("OTEL_TRACES_SAMPLER=%s: not a sampler; it takes %s",
			getenv("OTEL_TRACES_SAMPLER"), orList(samplersTaken))
	}
	if parentBased {
		s = sdktrace.ParentBased(s)
	}
	return sampler{Sampler: s}, nil
}

// sampled reports whether a job's trace, whose ID is tid and whose outer parent, if it joined one, is parent, is kept.
// Ratio samplers are a function of the trace ID, and the parent's flag is the job's, so a job decides the same way
// every time it is asked, as one resumed after a restart is.
func (s sampler) sampled(tid trace.TraceID, parent trace.SpanContext) bool {
	ctx := context.Background()
	if parent.IsValid() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, parent)
	}
	r := s.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx, TraceID: tid, Name: "job"})
	return r.Decision == sdktrace.RecordAndSample
}
