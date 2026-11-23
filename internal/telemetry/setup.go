package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config is how Setup sets a process's telemetry up.
type Config struct {
	Role Role
	// Instance is the process's service.instance.id: a coordinator's instance, or a worker's host name.
	Instance string
	Version  string // vamos's, its service.version
	// Getenv reads the OTEL_* variables; os.Getenv if nil.
	// A test passes its own, so that coordinators of different settings can run in one process at once.
	// It decides every setting Setup checks, those it leaves unset taking their defaults, and whether an unset
	// endpoint uses TLS; the exporters read the rest, such as the certificates themselves and compression, from the
	// process's environment, so a test that sets a certificate sets it in both.
	Getenv func(string) string
	// Warn reports what Setup ignores, such as a worker's OTEL_TRACES_EXPORTER; nothing if nil.
	// What fails to export later goes to the process's handler, which HandleErrors installs.
	Warn func(string)
	// Console is where the console exporters write; os.Stdout if nil.
	Console io.Writer
}

// Telemetry is what Setup set up: the bridge a coordinator exports its spans through, and the meters it and a worker
// count with.
type Telemetry struct {
	// Bridge exports a coordinator's spans; nil for a worker, and for a coordinator that exports neither traces nor
	// metrics. One that exports only metrics converts nothing.
	Bridge *Bridge
	// Meter is the provider of the process's meters, a no-op one if it exports no metrics.
	Meter metric.MeterProvider

	shutdown []tier // what to stop, in order
}

// ExportsMetrics reports whether t exports metrics, so that what measures only for them, as the stores' counts and
// the API's instrumentation do, is left out otherwise; false for a nil t.
func (t *Telemetry) ExportsMetrics() bool {
	if t == nil || t.Meter == nil {
		return false
	}
	_, off := t.Meter.(noop.MeterProvider) // as Setup leaves it with no metrics exporter
	return !off
}

// shutdownTimeout bounds how long Shutdown waits for what the exporters hold to be sent.
const shutdownTimeout = 10 * time.Second

// HandleErrors makes warn, rate-limited, the process's report of what OpenTelemetry fails to export,
// which happens apart from any build, so that the process's log alone tells of it.
// OpenTelemetry's error handler is the process's, not a Setup's, so a command calls this, once it has started.
func HandleErrors(warn func(string)) {
	otel.SetErrorHandler(newErrorHandler(warn, time.Now))
}

// Setup reads the process's telemetry settings from the OTEL_* variables, through cfg.Getenv, and builds the exporters
// they name, serving Prometheus, if a coordinator's metrics are scraped, on its own address, which it listens on
// before it returns, so that an address in use fails it.
// An exporter that a variable leaves unset exports nothing unless an OTLP endpoint is set, and OTEL_SDK_DISABLED=true
// exports nothing at all; any value Setup can't take fails it, naming the variable and what it takes,
// and its value unless that may be a secret.
// A Setup that fails stops what it had started.
// The caller calls the result's Shutdown once it has stopped making spans and counting.
func Setup(cfg Config) (*Telemetry, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	warn := cfg.Warn
	if warn == nil {
		warn = func(string) {}
	}
	var console io.Writer = &lockedWriter{w: cfg.Console} // which the console exporters of both signals share
	if cfg.Console == nil {
		console = os.Stdout // which takes writes at once already
	}
	s, warnings, err := readSettings(cfg.Role, getenv)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		warn(w)
	}
	t := &Telemetry{Meter: noop.NewMeterProvider()}
	if len(s.traces) == 0 && len(s.metrics) == 0 {
		return t, nil
	}
	res := s.resourceOf(cfg)
	ctx := context.Background()
	// What has started, which a failure stops: the exporters and readers, or once the providers own them, their
	// processors and readers, with the Prometheus server, which no provider owns; and then the providers.
	var owned, servers []func(context.Context) error
	var providers []func(context.Context) error
	fail := func(err error) (*Telemetry, error) {
		return nil, errors.Join(err, stopWithin(ctx, shutdownTimeout,
			[]tier{{stops: append(owned, servers...), least: shutdownTimeout / 2},
				{stops: providers, least: shutdownTimeout / 10}}))
	}

	var spanExporters []sdktrace.SpanExporter
	for _, name := range s.traces {
		exporter, err := s.spanExporter(ctx, name, console)
		if err != nil {
			return fail(err)
		}
		owned = append(owned, exporter.Shutdown)
		spanExporters = append(spanExporters, exporter)
	}
	var readers []sdkmetric.Reader
	for _, name := range s.metrics {
		reader, server, err := s.metricReader(ctx, name, console)
		if err != nil {
			return fail(err)
		}
		owned = append(owned, reader.Shutdown)
		if server != nil {
			servers = append(servers, server)
		}
		readers = append(readers, reader)
	}

	// Each processor and reader is stopped by Shutdown itself, all at once, rather than by its provider,
	// which stops them one after another and skips the rest once its context ends,
	// so that an exporter that blocks strands no other; the providers are stopped after, to release the rest.
	var parts []func(context.Context) error
	for _, reader := range readers {
		parts = append(parts, reader.Shutdown)
	}
	var tracer *sdktrace.TracerProvider
	if len(spanExporters) > 0 {
		opts := []sdktrace.TracerProviderOption{
			sdktrace.WithResource(res),
			sdktrace.WithIDGenerator(chosenIDs{}),
			// The bridge decides once for each job and makes no span of a trace it drops, so the provider follows
			// the flag the bridge sets on each span's parent.
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
			// Given, rather than read from the process's OTEL_SPAN_*_LIMIT variables, which could drop the
			// attributes the bridge keeps first, vamos.truncated, or the links: the bridge bounds a span itself.
			sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
				AttributeValueLengthLimit:   -1,
				AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
				EventCountLimit:             sdktrace.DefaultEventCountLimit,
				LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
				AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
				AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
			}),
		}
		for _, exporter := range spanExporters {
			processor := sdktrace.NewBatchSpanProcessor(exporter, s.batch.options()...)
			parts = append(parts, processor.Shutdown)
			opts = append(opts, sdktrace.WithSpanProcessor(processor))
		}
		tracer = sdktrace.NewTracerProvider(opts...)
		providers = append(providers, tracer.Shutdown)
	}
	owned = parts
	if len(readers) > 0 {
		opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithView(HTTPView())}
		for _, reader := range readers {
			opts = append(opts, sdkmetric.WithReader(reader))
		}
		meter := sdkmetric.NewMeterProvider(opts...)
		providers = append(providers, func(ctx context.Context) error {
			if err := meter.Shutdown(ctx); !errors.Is(err, sdkmetric.ErrReaderShutdown) {
				return err
			}
			return nil // as its readers are, by the tier before
		})
		t.Meter = meter
		if err := runtime.Start(runtime.WithMeterProvider(meter)); err != nil {
			return fail(fmt.Errorf("counting the Go runtime's metrics: %w", err))
		}
	}
	if cfg.Role == Coordinator {
		var spans trace.Tracer // none if only metrics are exported, which a bridge with no tracer has no use for yet
		if tracer != nil {
			spans = tracer.Tracer("github.com/vamos-build/vamos/internal/telemetry")
		}
		var meter metric.Meter // none if no metric is exported, so that spans aren't measured for nothing
		if t.ExportsMetrics() {
			meter = t.Meter.Meter(meterName)
		}
		t.Bridge, err = newBridge(spans, s.sampler, cfg.Instance, meter)
		if err != nil {
			return fail(err)
		}
		// First, so that the processors get the spans of what the bridge still has queued; and then, once that is
		// done or out of time, and before the processors are stopped, so that they get no more, what still waits is
		// dropped.
		t.shutdown = append(t.shutdown,
			tier{stops: []func(context.Context) error{t.Bridge.close}, after: t.Bridge.abandon})
	}
	// The processors and readers get half the time at least, however long the bridge took, to send what they hold.
	t.shutdown = append(t.shutdown,
		tier{stops: append(parts, servers...), least: shutdownTimeout / 2},
		tier{stops: providers, least: shutdownTimeout / 10})
	return t, nil
}

// Shutdown converts the spans the bridge has queued, sends what the exporters hold and stops them, and the Prometheus
// server, returning whatever failed to stop, or a metric reader's last export, which fails as it stops; spans that
// fail to send go, as every export's failure but that does, to the error handler that HandleErrors installs.
// It returns within shutdownTimeout of being called, or as ctx ends,
// whether or not they have finished, as an exporter whose collector doesn't answer, or whose console doesn't take what
// it writes, may not.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	tiers := t.shutdown
	t.shutdown = nil
	return stopWithin(ctx, shutdownTimeout, tiers)
}

// tier is a step of shutting down: what to stop, all at once, after the steps before,
// the least time of the budget it is left, however long those took,
// and what to do once the step is over, done or out of time, before the next starts, which mustn't block.
type tier struct {
	stops []func(context.Context) error
	least time.Duration
	after func()
}

// stopWithin calls each tier's stops at once, each tier once the one before has finished or run out of time,
// under a context of its own, whose deadline leaves each later tier its least time of budget,
// so that the time a tier doesn't use passes to the next, and a tier that runs out of time leaves the next a live
// context, which the SDK's providers need to stop their processors at all.
// It returns what they returned, or, once budget has passed or ctx has ended, that they didn't finish,
// leaving what hasn't to go on in the background.
func stopWithin(ctx context.Context, budget time.Duration, tiers []tier) error {
	if len(tiers) == 0 {
		return nil
	}
	end := time.Now().Add(budget)
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var errs []error
		for i, t := range tiers {
			deadline := end
			for _, later := range tiers[i+1:] {
				deadline = deadline.Add(-later.least)
			}
			tierCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
			results := make(chan error, len(t.stops))
			for _, stop := range t.stops {
				go func() { results <- stop(tierCtx) }()
			}
		wait:
			for range t.stops {
				select {
				case err := <-results:
					errs = append(errs, err)
				case <-tierCtx.Done():
					errs = append(errs, tierCtx.Err())
					break wait
				}
			}
			cancel()
			if t.after != nil {
				t.after()
			}
		}
		done <- errors.Join(errs...)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("stopping telemetry: what it held wasn't sent in time: %w", ctx.Err())
	}
}

// resourceOf returns the resource that every span and metric of the process carries.
func (s settings) resourceOf(cfg Config) *resource.Resource {
	name := s.serviceName
	if name == "" {
		name = s.resource["service.name"]
	}
	if name == "" {
		name = "vamos-" + string(cfg.Role)
	}
	attrs := make([]attribute.KeyValue, 0, len(s.resource)+3)
	for k, v := range s.resource {
		attrs = append(attrs, attribute.String(k, v))
	}
	attrs = append(attrs, // after OTEL_RESOURCE_ATTRIBUTES', so that these win
		attribute.String("service.name", name),
		attribute.String("service.version", cfg.Version),
		attribute.String("service.instance.id", cfg.Instance))
	return resource.NewSchemaless(attrs...)
}

// options returns the batch span processor's options that the OTEL_BSP_* variables set, all of them given,
// so that the SDK doesn't read the process's environment for one Getenv left unset.
// The processor blocks the bridge's converter when its queue is full, rather than drop a span, as it otherwise would,
// so that a snapshot isn't handed over in part; the bridge drops whole snapshots instead, off the build's path.
// Its queue holds the SDK's 2,048 spans unless OTEL_BSP_MAX_QUEUE_SIZE says otherwise, so that what it holds is
// bounded, each span by the bridge's budget of a span, maxAttributesSize and the sizes beside it, some 40 MiB in all,
// though a full one blocks the converter, which every exporter shares.
// It sends defaultMaxExportBatch spans a request unless OTEL_BSP_MAX_EXPORT_BATCH_SIZE says otherwise, rather than the
// SDK's 512, which, of spans of some 20 KiB, would pass the 4 MiB a gRPC receiver takes by default.
func (b batchSettings) options() []sdktrace.BatchSpanProcessorOption {
	return []sdktrace.BatchSpanProcessorOption{sdktrace.WithBlocking(),
		sdktrace.WithBatchTimeout(b.scheduleDelay), sdktrace.WithExportTimeout(b.exportTimeout),
		sdktrace.WithMaxQueueSize(b.maxQueueSize), sdktrace.WithMaxExportBatchSize(b.maxExportBatch)}
}

// spanExporter returns the span exporter name.
func (s settings) spanExporter(ctx context.Context, name string, console io.Writer) (sdktrace.SpanExporter, error) {
	if name == exporterConsole {
		return stdouttrace.New(stdouttrace.WithWriter(console))
	}
	o := s.otlp[signalTraces]
	if o.protocol == "grpc" {
		return otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(o.endpoint),
			otlptracegrpc.WithHeaders(o.headers), otlptracegrpc.WithTimeout(o.timeout))
	}
	// Protobuf whatever OTEL_EXPORTER_OTLP_PROTOCOL the process's environment holds, which the exporter would
	// otherwise read as http/json.
	return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(o.endpoint), otlptracehttp.WithHeaders(o.headers),
		otlptracehttp.WithTimeout(o.timeout), otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf))
}

// metricReader returns the metric reader name, and what stops what it serves, if anything.
func (s settings) metricReader(ctx context.Context, name string, console io.Writer) (sdkmetric.Reader,
	func(context.Context) error, error) {
	var exporter sdkmetric.Exporter
	var err error
	switch name {
	case exporterPrometheus:
		return servePrometheus(s.prometheusAddr)
	case exporterConsole:
		exporter, err = stdoutmetric.New(stdoutmetric.WithWriter(console))
	default:
		exporter, err = s.otlpMetricExporter(ctx)
	}
	if err != nil {
		return nil, nil, err
	}
	return sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(s.metricInterval),
		sdkmetric.WithTimeout(s.metricTimeout)), nil, nil
}

// otlpMetricExporter returns the OTLP metric exporter that the settings name.
func (s settings) otlpMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	o := s.otlp[signalMetrics]
	if o.protocol == "grpc" {
		return otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(o.endpoint),
			otlpmetricgrpc.WithHeaders(o.headers), otlpmetricgrpc.WithTimeout(o.timeout))
	}
	return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(o.endpoint), otlpmetrichttp.WithHeaders(o.headers),
		otlpmetrichttp.WithTimeout(o.timeout))
}

// servePrometheus returns a reader that Prometheus scrapes at /metrics on addr, served by a server of its own,
// listening when it returns, and what stops the server.
// It serves OpenMetrics to a scrape that asks for it, which alone carries exemplars.
func servePrometheus(addr string) (sdkmetric.Reader, func(context.Context) error, error) {
	registry := promclient.NewRegistry()
	reader, err := prometheus.New(prometheus.WithRegisterer(registry))
	if err != nil {
		return nil, nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("serving Prometheus metrics on %s, as OTEL_EXPORTER_PROMETHEUS_HOST and "+
			"OTEL_EXPORTER_PROMETHEUS_PORT say: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(l) //nolint:errcheck // Serve returns only once Shutdown stops it
	// A scrape still being written as the deadline passes is cut off, so that a slow scraper doesn't outlive it.
	stop := func(ctx context.Context) error {
		err := server.Shutdown(ctx)
		if err != nil {
			_ = server.Close()
		}
		return err
	}
	return reader, stop, nil
}

// defaultMaxQueueSize is how many spans the batch span processor holds unless OTEL_BSP_MAX_QUEUE_SIZE says otherwise,
// the SDK's own default.
const defaultMaxQueueSize = 2048

// defaultMaxExportBatch is how many spans a request sends unless OTEL_BSP_MAX_EXPORT_BATCH_SIZE says otherwise.
const defaultMaxExportBatch = 128

// lockedWriter is a writer that takes one write at a time, so that exporters may share a writer that doesn't.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
