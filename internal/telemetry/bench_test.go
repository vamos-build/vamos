package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vamos-build/vamos/internal/tracing"
)

// BenchmarkConvertAttempt measures what exporting the largest attempt trace that is stored costs a coordinator:
// spans of a few arguments each, nested two deep, trimmed to tracing.MaxTraceSize, converted with export on and
// sampled, to an exporter that discards them as they end.
func BenchmarkConvertAttempt(b *testing.B) {
	var spans []tracing.Span
	for i := range 1 << 16 {
		s := tracing.Span{ID: fmt.Sprint("s-", i), Process: "executor", Track: "main", Name: fmt.Sprintf("run %d", i),
			Start: at(i), End: at(i + 1), Args: map[string]any{"argv": []any{"make", fmt.Sprint("target-", i)},
				"exit code": 0.0, "output": strings.Repeat("x", 200)}}
		if i%8 != 0 {
			s.Parent = fmt.Sprint("s-", i-i%8)
		}
		spans = append(spans, s)
	}
	spans = tracing.Trim(spans, tracing.Lost{}, tracing.MaxTraceSize)
	provider := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(chosenIDs{}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithSyncer(tracetest.NewNoopExporter()))
	b.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	bridge := newBridge(provider.Tracer("bench"), sampler{Sampler: sdktrace.AlwaysSample()}, "i")
	b.Cleanup(func() { _ = bridge.close(context.Background()) })
	ctx := bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		bridge.Attempt(ctx, fmt.Sprint("att-", i), spans, at(0), at(len(spans)))
		if err := bridge.flush(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(spans)), "spans/op") // after the timer's reset, which drops it
}
