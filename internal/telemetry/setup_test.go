package telemetry

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/vamos-build/vamos/internal/tracing"
)

// freePort returns a port that nothing listens on, as far as anyone can tell.
func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// setup sets telemetry up for role in the environment env, and shuts it down when the test ends.
func setup(t *testing.T, role Role, env map[string]string, warn func(string), console io.Writer) *Telemetry {
	t.Helper()
	tel, err := Setup(Config{Role: role, Instance: "i", Version: "test", Getenv: envOf(env), Warn: warn,
		Console: console})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

// TestSetupWithNothingConfigured checks that a coordinator configured to export nothing gets no bridge and a no-op
// meter, and serves nothing on Prometheus's default port.
func TestSetupWithNothingConfigured(t *testing.T) {
	tel := setup(t, Coordinator, nil, nil, nil)
	if tel.Bridge != nil {
		t.Error("a coordinator that exports nothing has a bridge")
	}
	if _, ok := tel.Meter.(noop.MeterProvider); !ok {
		t.Errorf("its meter is a %T, want a no-op one", tel.Meter)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

// TestSetupServesPrometheus checks that a coordinator exporting to Prometheus serves its metrics at /metrics on the
// address the variables give, as text or OpenMetrics as the scrape asks, before Setup returns.
func TestSetupServesPrometheus(t *testing.T) {
	port := freePort(t)
	setup(t, Coordinator, map[string]string{
		"OTEL_METRICS_EXPORTER":         "prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST": "127.0.0.1",
		"OTEL_EXPORTER_PROMETHEUS_PORT": port,
	}, nil, nil)
	for _, c := range []struct{ accept, contentType string }{
		{accept: "", contentType: "text/plain"},
		{accept: "application/openmetrics-text; version=1.0.0", contentType: "application/openmetrics-text"},
	} {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), c.contentType) {
			t.Errorf("a scrape accepting %q answered %d, %s; want 200, %s", c.accept, resp.StatusCode,
				resp.Header.Get("Content-Type"), c.contentType)
		}
		metrics := string(body)
		if !strings.Contains(metrics, "go_goroutine") ||
			!strings.Contains(metrics, `service_name="vamos-coordinator"`) {
			t.Errorf("a scrape accepting %q holds no runtime metric of vamos-coordinator:\n%s", c.accept, body)
		}
	}
}

// TestSetupPrometheusPortInUse checks that a Prometheus address that something else listens on fails Setup, naming it.
func TestSetupPrometheusPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()
	host, port, _ := net.SplitHostPort(addr)
	_, err = Setup(Config{Role: Coordinator, Getenv: envOf(map[string]string{
		"OTEL_METRICS_EXPORTER":         "prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST": host,
		"OTEL_EXPORTER_PROMETHEUS_PORT": port,
	})})
	if err == nil || !strings.HasPrefix(err.Error(), "serving Prometheus metrics on "+addr+", ") {
		t.Errorf("Setup on an address in use = %v, want an error naming %s", err, addr)
	}
}

// TestSetupWorker checks that a worker gets no bridge, warns that it ignores OTEL_TRACES_EXPORTER, and counts with a
// meter of its own.
func TestSetupWorker(t *testing.T) {
	var warnings []string
	tel := setup(t, Worker, map[string]string{
		"OTEL_TRACES_EXPORTER":  "otlp",
		"OTEL_METRICS_EXPORTER": "console",
	}, func(w string) { warnings = append(warnings, w) }, io.Discard)
	if tel.Bridge != nil {
		t.Error("a worker has a bridge")
	}
	if _, ok := tel.Meter.(noop.MeterProvider); ok {
		t.Error("a worker exporting metrics has a no-op meter")
	}
	want := []string{"OTEL_TRACES_EXPORTER=otlp is ignored: a worker exports no traces, " +
		"since its coordinator exports its spans"}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
}

// lockedBuffer is a buffer that exporters may write to while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestShutdownFlushes checks that a coordinator's Shutdown sends the spans its exporter still holds, which a schedule
// of an hour would otherwise keep.
func TestShutdownFlushes(t *testing.T) {
	var console lockedBuffer
	tel := setup(t, Coordinator, map[string]string{
		"OTEL_TRACES_EXPORTER":    "console",
		"OTEL_BSP_SCHEDULE_DELAY": "3600000",
	}, nil, &console)
	ctx := tel.Bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	tel.Bridge.Job(ctx, []tracing.Span{{ID: "s-1", Name: "the step", Start: at(0), End: at(1)}}, at(0), at(2),
		"succeeded")
	if strings.Contains(console.String(), "the step") {
		t.Fatal("the span was exported before Shutdown; the test proves nothing")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "the step") {
		t.Errorf("Shutdown didn't send the span the exporter held:\n%s", console.String())
	}
}

// TestErrorHandlerLimitsRepeats checks that the error handler reports each distinct error at most once a minute, with
// how many like it it suppressed since it last reported it.
func TestErrorHandlerLimitsRepeats(t *testing.T) {
	var warnings []string
	now := t0
	h := newErrorHandler(func(w string) { warnings = append(warnings, w) }, func() time.Time { return now })
	refused := errors.New("connection refused")
	h.Handle(refused)
	h.Handle(refused)
	h.Handle(errors.New("timed out"))
	now = now.Add(59 * time.Second)
	h.Handle(refused)
	now = now.Add(time.Second)
	h.Handle(refused)
	h.Handle(nil)
	want := []string{
		"exporting telemetry: connection refused",
		"exporting telemetry: timed out",
		"exporting telemetry: connection refused (and 2 more like it since it was last reported)",
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}

	// An error holding a collector's whole response is reported cut, and one that differs from it only past the cut,
	// as by a request's ID, counts as the same.
	warnings = nil
	long := strings.Repeat("x", 1<<20)
	h.Handle(errors.New(long + "1"))
	h.Handle(errors.New(long + "2"))
	if len(warnings) != 1 {
		t.Fatalf("reported %d warnings, want 1", len(warnings))
	}
	if max := len("exporting telemetry: ") + maxErrorSize; len(warnings[0]) > max {
		t.Errorf("reported %d bytes, want at most %d", len(warnings[0]), max)
	}
}

// TestSetupFailureStopsWhatItStarted checks that a Setup that fails after it started a metric reader, here because
// Prometheus's address is in use, stops the reader rather than leaving its goroutine running.
// A reader that no provider has taken on has nothing to export, so its goroutine is all that would show it.
func TestSetupFailureStopsWhatItStarted(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	before := goruntime.NumGoroutine()
	_, err = Setup(Config{Role: Coordinator, Console: io.Discard, Getenv: envOf(map[string]string{
		"OTEL_METRICS_EXPORTER":         "console,prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST": host,
		"OTEL_EXPORTER_PROMETHEUS_PORT": port,
	})})
	if err == nil {
		t.Fatal("Setup on an address in use succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := goruntime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines ran before Setup and %d still run after it failed, want what it started stopped",
			before, after)
	}
}

// blockingWriter is a console that never takes what is written to it.
type blockingWriter struct{ unblock chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.unblock
	return len(p), nil
}

// TestShutdownReturnsAtItsDeadline checks that Shutdown returns as its context ends, saying what wasn't sent,
// though an exporter's console never takes what it writes.
func TestShutdownReturnsAtItsDeadline(t *testing.T) {
	console := blockingWriter{unblock: make(chan struct{})}
	defer close(console.unblock)
	tel, err := Setup(Config{Role: Coordinator, Console: console, Getenv: envOf(map[string]string{
		"OTEL_METRICS_EXPORTER": "console",
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = tel.Shutdown(ctx)
	if took := time.Since(began); took > 5*time.Second || err == nil ||
		!strings.HasPrefix(err.Error(), "stopping telemetry: what it held wasn't sent in time: ") {
		t.Errorf("Shutdown of a console that blocks = %v after %s, want it to give up at its deadline", err, took)
	}
}

// TestHandleErrors checks that what OpenTelemetry fails to export reaches the process's handler,
// whatever else an earlier test's exporters, still stopping, report to it.
func TestHandleErrors(t *testing.T) {
	var mu sync.Mutex
	var warnings []string
	HandleErrors(func(w string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, w)
	})
	t.Cleanup(func() { HandleErrors(func(string) {}) })
	otel.Handle(errors.New("collector unreachable"))
	mu.Lock()
	defer mu.Unlock()
	if want := "exporting telemetry: collector unreachable"; !slices.Contains(warnings, want) {
		t.Errorf("warnings = %q, want %q among them", warnings, want)
	}
}

// TestShutdownStopsEveryPartAtItsDeadline checks that a Shutdown whose metrics console never takes what it writes
// still stops the Prometheus server, which doesn't depend on it, so that its port is free again.
func TestShutdownStopsEveryPartAtItsDeadline(t *testing.T) {
	console := blockingWriter{unblock: make(chan struct{})}
	defer close(console.unblock)
	port := freePort(t)
	tel, err := Setup(Config{Role: Coordinator, Console: console, Getenv: envOf(map[string]string{
		"OTEL_METRICS_EXPORTER":         "console,prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST": "127.0.0.1",
		"OTEL_EXPORTER_PROMETHEUS_PORT": port,
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := tel.Shutdown(ctx); err == nil {
		t.Error("Shutdown of a console that blocks succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err == nil {
			l.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Prometheus server still holds its port after Shutdown: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStopWithinGivesEachTierItsShare checks that a tier that never finishes leaves the next tier a live context of
// its own share of the time, which the SDK's providers need to stop their processors, and that stopWithin returns at
// its deadline all the same.
func TestStopWithinGivesEachTierItsShare(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	live := make(chan bool, 1)
	err := stopWithin(context.Background(), 200*time.Millisecond, []tier{
		{stops: []func(context.Context) error{func(context.Context) error { <-block; return nil }}},
		{stops: []func(context.Context) error{func(ctx context.Context) error { live <- ctx.Err() == nil; return nil }},
			least: 100 * time.Millisecond},
	})
	if err == nil {
		t.Error("stopWithin of a tier that never finishes succeeded")
	}
	select {
	case ok := <-live:
		if !ok {
			t.Error("the tier after one that ran out of time got a context that had ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tier after one that ran out of time never ran")
	}
}

// TestSetupSendsOverOTLP checks that a coordinator whose endpoint and headers its Getenv gives sends a job's spans
// there, at the signal's path and with the headers, once it shuts down.
func TestSetupSendsOverOTLP(t *testing.T) {
	type request struct {
		path, key string
		size      int
	}
	received := make(chan request, 16)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- request{path: r.URL.Path, key: r.Header.Get("Api-Key"), size: len(body)}
	}))
	defer collector.Close()
	tel, err := Setup(Config{Role: Coordinator, Getenv: envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL + "/otlp",
		"OTEL_EXPORTER_OTLP_HEADERS":  "api-key=s3cret",
		"OTEL_METRICS_EXPORTER":       "none",
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tel.Bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	tel.Bridge.Job(ctx, nil, at(0), at(1), "succeeded")
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-received:
		if r.path != "/otlp/v1/traces" || r.key != "s3cret" || r.size == 0 {
			t.Errorf("the collector received %+v, want spans at /otlp/v1/traces with api-key s3cret", r)
		}
	default:
		t.Error("the collector received nothing")
	}
}

// TestSetupIgnoresTheProcessEnvironment checks that the settings Getenv leaves unset aren't taken from the process's
// environment, which the SDK would read: neither its headers, which may be another collector's credentials, nor its
// endpoints, nor a batch setting that would fail; and that metrics go to their own endpoint, with their own headers.
func TestSetupIgnoresTheProcessEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=leaked")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/elsewhere")
	t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "-1")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "") // so that the metrics request is plain protobuf
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json") // which Getenv's unset protocol, http/protobuf, overrides
	t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "1")     // which would drop all but one of the job's attributes
	type request struct {
		path, auth, team, contentType string
		count                         int64 // vamos.test's, in a metrics request
		jobAttributes                 int   // the job's span's, in a traces request
	}
	received := make(chan request, 16)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := request{path: r.URL.Path, auth: r.Header.Get("Authorization"), team: r.Header.Get("Team"),
			contentType: r.Header.Get("Content-Type")}
		var traces collectortrace.ExportTraceServiceRequest
		if r.URL.Path == "/v1/traces" && proto.Unmarshal(body, &traces) == nil {
			for _, rs := range traces.GetResourceSpans() {
				for _, ss := range rs.GetScopeSpans() {
					for _, span := range ss.GetSpans() {
						if span.GetName() == "build" {
							req.jobAttributes = len(span.GetAttributes())
						}
					}
				}
			}
		}
		var metrics collectormetrics.ExportMetricsServiceRequest
		if r.URL.Path == "/metrics" && proto.Unmarshal(body, &metrics) == nil {
			for _, rm := range metrics.GetResourceMetrics() {
				for _, sm := range rm.GetScopeMetrics() {
					for _, m := range sm.GetMetrics() {
						if m.GetName() == "vamos.test" {
							for _, p := range m.GetSum().GetDataPoints() {
								req.count += p.GetAsInt()
							}
						}
					}
				}
			}
		}
		received <- req
	}))
	defer collector.Close()
	tel, err := Setup(Config{Role: Coordinator, Getenv: envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         collector.URL,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": collector.URL + "/metrics",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "team=metrics",
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tel.Bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	tel.Bridge.Job(ctx, nil, at(0), at(1), "succeeded")
	counter, err := tel.Meter.Meter("test").Int64Counter("vamos.test")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(received)
	got := map[string]request{}
	for r := range received {
		got[r.path] = r
	}
	const protobuf = "application/x-protobuf"
	want := map[string]request{"/v1/traces": {path: "/v1/traces", contentType: protobuf, jobAttributes: 3},
		"/metrics": {path: "/metrics", team: "metrics", contentType: protobuf, count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the collector received %+v, want %+v", got, want)
	}
}

// TestDefaultEndpointKeepsTLS checks that OTLP exporters with no endpoint set take the certificate that asks for TLS,
// though the process's environment holds an http:// endpoint, whose scheme would otherwise refuse it.
// It builds the exporters alone, so that nothing is sent to a collector that may listen on the default port.
func TestDefaultEndpointKeepsTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	certificate := t.TempDir() + "/ca.pem"
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificate, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certificate) // which the exporters read themselves
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/elsewhere")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/elsewhere")
	for _, protocol := range []string{"http/protobuf", "grpc"} {
		s, _, err := readSettings(Coordinator, envOf(map[string]string{
			"OTEL_TRACES_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": protocol,
			"OTEL_EXPORTER_OTLP_CERTIFICATE": certificate,
		}))
		if err != nil {
			t.Fatal(err)
		}
		spans, err := s.spanExporter(t.Context(), exporterOTLP, io.Discard)
		if err != nil {
			t.Fatalf("a span exporter over %s with a certificate and no endpoint: %v", protocol, err)
		}
		_ = spans.Shutdown(t.Context())
		metrics, err := s.otlpMetricExporter(t.Context())
		if err != nil {
			t.Fatalf("a metric exporter over %s with a certificate and no endpoint: %v", protocol, err)
		}
		_ = metrics.Shutdown(t.Context())
	}
}

// TestShutdownFlushesAHealthyExporterBesideABlockedOne checks that a Shutdown whose console exporter never takes
// what it writes still sends what the OTLP exporter holds.
func TestShutdownFlushesAHealthyExporterBesideABlockedOne(t *testing.T) {
	received := make(chan string, 16)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.Path
	}))
	defer collector.Close()
	console := blockingWriter{unblock: make(chan struct{})}
	defer close(console.unblock)
	tel, err := Setup(Config{Role: Coordinator, Console: console, Getenv: envOf(map[string]string{
		"OTEL_TRACES_EXPORTER":        "console,otlp",
		"OTEL_METRICS_EXPORTER":       "none",
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL,
		"OTEL_BSP_SCHEDULE_DELAY":     "3600000",
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tel.Bridge.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	tel.Bridge.Job(ctx, nil, at(0), at(1), "succeeded")
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tel.Shutdown(shutdown) // which fails, as the console never finishes
	select {
	case path := <-received:
		if path != "/v1/traces" {
			t.Errorf("the collector received a request for %s, want /v1/traces", path)
		}
	case <-time.After(5 * time.Second):
		t.Error("the collector received nothing, as the console exporter blocked the OTLP one")
	}
}

// TestStopWithinRunsAfterBeforeTheNextTier checks that a tier's after runs once the tier is over, though one of its
// stops never finishes, and before any stop of the next tier starts.
func TestStopWithinRunsAfterBeforeTheNextTier(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	_ = stopWithin(context.Background(), 400*time.Millisecond, []tier{
		{stops: []func(context.Context) error{func(context.Context) error { <-block; return nil }},
			after: func() { time.Sleep(50 * time.Millisecond); note("after") }},
		{stops: []func(context.Context) error{func(context.Context) error { note("next"); return nil }},
			least: 200 * time.Millisecond},
	})
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"after", "next"}; !slices.Equal(order, want) {
		t.Errorf("ran %q, want %q", order, want)
	}
}
