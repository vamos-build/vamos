package telemetry

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// httpLabels are the only attributes that otelhttp's instruments keep: what the API's routes and answers are, never
// the Host or port a client chose, which would make a series of each.
var httpLabels = []attribute.Key{"http.request.method", "http.route", "http.response.status_code"}

// HTTPView returns the view that keeps only httpLabels on every instrument of otelhttp's scope: the duration's, and
// the request and response bodies' sizes.
func HTTPView() sdkmetric.View {
	return sdkmetric.NewView(sdkmetric.Instrument{Scope: instrumentation.Scope{Name: otelhttp.ScopeName}},
		sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(httpLabels...)})
}

// Instrument returns what wraps a handler, a ServeMux, which must be the handler it wraps so that the route it
// matched is read, in otelhttp, measured by meter, which HTTPView must view; it records no spans, since a job's
// spans are the bridge's to export.
func Instrument(meter metric.MeterProvider) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return otelhttp.NewHandler(h, "vamos", otelhttp.WithMeterProvider(meter),
			otelhttp.WithTracerProvider(tracenoop.NewTracerProvider()))
	}
}

// Transferred returns what counts the bytes of blobs moved in direction, "in" or "out", as vamos.transfer.bytes,
// measured by meter.
func Transferred(meter metric.MeterProvider) (func(direction string, n int64), error) {
	counter, err := meter.Meter(meterName).Int64Counter("vamos.transfer.bytes", metric.WithUnit("By"),
		metric.WithDescription("The bytes of blobs the API received and sent, by direction."))
	if err != nil {
		return nil, err
	}
	for _, direction := range []string{"in", "out"} {
		counter.Add(context.Background(), 0, metric.WithAttributes(attribute.String("direction", direction)))
	}
	return func(direction string, n int64) {
		counter.Add(context.Background(), n, metric.WithAttributes(attribute.String("direction", direction)))
	}, nil
}
