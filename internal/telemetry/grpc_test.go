package telemetry

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/vamos-build/vamos/internal/tracing"
)

// countingCollector is an OTLP/gRPC trace receiver that counts the spans it is sent.
type countingCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	spans atomic.Int64
}

func (c *countingCollector) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (
	*collectortrace.ExportTraceServiceResponse, error) {
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			c.spans.Add(int64(len(ss.Spans)))
		}
	}
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

// TestSetupSendsLargeSpansOverGRPC checks that a coordinator sending over gRPC gets every span of a job whose spans
// each hold as much as the bridge's budget lets them to a receiver that takes gRPC's default 4 MiB a message,
// which the SDK's default batch of 512 such spans would pass.
func TestSetupSendsLargeSpansOverGRPC(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collector := &countingCollector{}
	collectortrace.RegisterTraceServiceServer(server, collector)
	go server.Serve(l) //nolint:errcheck // Serve returns once Stop stops it
	t.Cleanup(server.Stop)

	tel, err := Setup(Config{Role: Coordinator, Getenv: envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + l.Addr().String(),
		"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
		"OTEL_METRICS_EXPORTER":       "none",
	})})
	if err != nil {
		t.Fatal(err)
	}
	var spans []tracing.Span
	for i := range 513 {
		args := map[string]any{}
		for j := range 16 {
			args[fmt.Sprint("output ", j)] = strings.Repeat("o", maxValueSize)
		}
		spans = append(spans, tracing.Span{ID: fmt.Sprint("s-", i), Name: "step", Start: at(i), End: at(i + 1),
			Args: args})
	}
	ctx := tel.Bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	tel.Bridge.Job(ctx, spans, at(0), at(514), "succeeded")
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := collector.spans.Load(); got != 514 {
		t.Errorf("the collector received %d spans, want the job's and its 513", got)
	}
}
