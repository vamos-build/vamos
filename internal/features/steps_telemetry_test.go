package features

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// Steps about the telemetry that a coordinator exports over OpenTelemetry: an OTLP collector of the scenario's own,
// the coordinator's OTEL_* environment, and what the collector received.

// otlpCollector is an OTLP/HTTP receiver, in this process, that keeps the spans it is sent, decoded.
type otlpCollector struct {
	url     string
	mu      sync.Mutex
	got     []collectedSpan
	metrics map[string]float64 // the metrics it received, by name, each a sum's last value, or 0
}

// collectedSpan is a span the collector received, its IDs in hex, and its links' as trace/span.
type collectedSpan struct {
	name              string
	trace, id, parent string
	state             string
	start, end        time.Time
	attributes        map[string]string // as text, a list's members joined by commas
	links             []string          // the span IDs it links to
}

// newOTLPCollector starts a collector listening at addr, until the scenario ends.
func (w *world) newOTLPCollector(addr string) *otlpCollector {
	w.t.Helper()
	c := &otlpCollector{}
	listener, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) { // as a collector of the machine's own may
		testenv.Unavailable(w.t, "an OTLP collector at "+addr, err)
	} else if err != nil {
		w.t.Fatalf("starting an OTLP collector at %s: %v", addr, err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(c.serveHTTP))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	w.t.Cleanup(server.Close)
	c.url = server.URL
	w.collector = c
	return c
}

// serveHTTP takes an export request: spans it keeps, metrics whose names it keeps, and anything else it accepts unread.
func (c *otlpCollector) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/v1/metrics" {
		var req collectormetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		if c.metrics == nil {
			c.metrics = map[string]float64{}
		}
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					// A sum is kept by the value of its last point, and anything else as received alone.
					if points := m.GetSum().GetDataPoints(); len(points) > 0 {
						last := points[len(points)-1]
						c.metrics[m.GetName()] = float64(last.GetAsInt()) + last.GetAsDouble()
					} else if _, kept := c.metrics[m.GetName()]; !kept {
						c.metrics[m.GetName()] = math.NaN()
					}
				}
			}
		}
		c.mu.Unlock()
	}
	if r.URL.Path != "/v1/traces" {
		rw.WriteHeader(http.StatusOK)
		return
	}
	var req collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var spans []collectedSpan
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				cs := collectedSpan{name: s.GetName(), trace: hex.EncodeToString(s.GetTraceId()),
					id: hex.EncodeToString(s.GetSpanId()), parent: hex.EncodeToString(s.GetParentSpanId()),
					state: s.GetTraceState(), start: time.Unix(0, int64(s.GetStartTimeUnixNano())),
					end: time.Unix(0, int64(s.GetEndTimeUnixNano())), attributes: map[string]string{}}
				for _, a := range s.GetAttributes() {
					cs.attributes[a.GetKey()] = valueText(a.GetValue())
				}
				for _, l := range s.GetLinks() {
					link := hex.EncodeToString(l.GetTraceId()) + "/" + hex.EncodeToString(l.GetSpanId())
					cs.links = append(cs.links, link)
				}
				spans = append(spans, cs)
			}
		}
	}
	c.mu.Lock()
	c.got = append(c.got, spans...)
	c.mu.Unlock()
	rw.WriteHeader(http.StatusOK)
}

// valueText returns v as text, a list's members joined by commas.
func valueText(v *commonpb.AnyValue) string {
	if list := v.GetArrayValue(); list != nil {
		var members []string
		for _, m := range list.GetValues() {
			members = append(members, valueText(m))
		}
		return strings.Join(members, ",")
	}
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprint(v.GetIntValue())
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprint(v.GetBoolValue())
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprint(v.GetDoubleValue())
	}
	return v.String()
}

// hasMetric reports whether the collector has received a metric named name.
func (c *otlpCollector) hasMetric(name string) bool {
	_, ok := c.metric(name)
	return ok
}

// metric returns the last value of the sum named name that the collector received, NaN for another kind of metric
// or a sum with no point, and whether it received one.
func (c *otlpCollector) metric(name string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.metrics[name]
	return v, ok
}

// spans returns what the collector has received.
func (c *otlpCollector) spans() []collectedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

// telemetryGetenv is the scenario's coordinator's Getenv: the OTEL_* variables the scenario set, and
// nothing of the process's.
func (w *world) telemetryGetenv(name string) string {
	return w.telemetryEnv[name]
}

// workerTelemetryGetenv is the scenario's workers' Getenv, as telemetryGetenv is the coordinator's.
func (w *world) workerTelemetryGetenv(name string) string {
	return w.workerTelemetryEnv[name]
}

// GivenTheWorkerEnvironmentSets sets the variable name of the scenario's workers, which a later step starts, to value.
func (w *world) GivenTheWorkerEnvironmentSets(name, value string) {
	w.t.Helper()
	if w.workerTelemetryEnv == nil {
		w.workerTelemetryEnv = map[string]string{}
	}
	w.workerTelemetryEnv[name] = value
}

// setTelemetryEnv sets the variable name of the scenario's coordinator to value.
func (w *world) setTelemetryEnv(name, value string) {
	if w.telemetryEnv == nil {
		w.telemetryEnv = map[string]string{}
	}
	w.telemetryEnv[name] = value
}

// exportDelay is how long the coordinator of a scenario with a collector holds spans before it sends them, short, so
// that a scenario that waits for them waits little.
const exportDelay = 50 * time.Millisecond

// GivenAnOTLPCollector starts an OTLP collector, until the scenario ends, which the coordinator that a later step
// starts sends its traces to, as OTEL_EXPORTER_OTLP_ENDPOINT names it, at once, as OTEL_BSP_SCHEDULE_DELAY says.
func (w *world) GivenAnOTLPCollector() {
	w.t.Helper()
	w.hasNoCoordinator()
	c := w.newOTLPCollector("127.0.0.1:0")
	w.setTelemetryEnv("OTEL_EXPORTER_OTLP_ENDPOINT", c.url)
	w.setTelemetryEnv("OTEL_BSP_SCHEDULE_DELAY", fmt.Sprint(exportDelay.Milliseconds()))
}

// GivenAnOTLPCollectorAtTheDefaultEndpoint starts an OTLP collector at localhost:4318, the default endpoint, until
// the scenario ends, naming it to nothing: a scenario that uses it runs alone, and is skipped where something else,
// such as a developer's own collector, listens there, at localhost's IPv4 address or its IPv6 one, both of which it
// serves.
func (w *world) GivenAnOTLPCollectorAtTheDefaultEndpoint() {
	w.t.Helper()
	c := w.newOTLPCollector("127.0.0.1:4318")
	// The same collector at localhost's IPv6 address, where the coordinator may resolve localhost to.
	l, err := net.Listen("tcp", "[::1]:4318")
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		testenv.Unavailable(w.t, "an OTLP collector at localhost:4318", err)
	case err == nil:
		server := httptest.NewUnstartedServer(http.HandlerFunc(c.serveHTTP))
		server.Listener.Close()
		server.Listener = l
		server.Start()
		w.t.Cleanup(server.Close)
	} // a host without IPv6 has no such address to listen at
}

// GivenTheCoordinatorEnvironmentSets sets the variable name to value in the environment of the coordinator that a
// later step starts, rather than the process's, so that the scenario may run beside others.
func (w *world) GivenTheCoordinatorEnvironmentSets(name, value string) {
	w.t.Helper()
	w.hasNoCoordinator()
	w.setTelemetryEnv(name, value)
}

// GivenTRACEPARENT sets TRACEPARENT in the process's environment, as a CI tool that traces its pipeline sets it, for
// the commands the scenario runs: a rule that uses it runs alone.
func (w *world) GivenTRACEPARENT(value string) {
	w.t.Helper()
	w.GivenTheEnvironmentSets("TRACEPARENT", value)
}

// received waits until the collector has received spans for which done holds, or receiveDeadline has passed, and
// returns them.
func (w *world) received(done func([]collectedSpan) bool) []collectedSpan {
	w.t.Helper()
	if w.collector == nil {
		w.t.Fatal("no GivenAnOTLPCollector step started a collector")
	}
	for deadline := time.Now().Add(receiveDeadline); ; time.Sleep(20 * time.Millisecond) {
		spans := w.collector.spans()
		if done(spans) || time.Now().After(deadline) {
			return spans
		}
	}
}

// receiveDeadline bounds how long a step waits for spans the collector should receive.
const receiveDeadline = 15 * time.Second

// jobSpans returns the job spans among spans, by the kind of job.
func jobSpans(spans []collectedSpan) map[string][]collectedSpan {
	jobs := map[string][]collectedSpan{}
	for _, s := range spans {
		if kind, ok := s.attributes["vamos.job.kind"]; ok {
			jobs[kind] = append(jobs[kind], s)
		}
	}
	return jobs
}

// collected returns the spans the collector received once a span of each of the jobs the coordinator accepted during
// the last command, and also, has come, and then no more for a while, so that the rest of each job's have come too.
// It stops the scenario if one of those jobs' never comes.
func (w *world) collected(also ...string) []collectedSpan {
	w.t.Helper()
	jobs := append(slices.Clone(w.acceptedDuring), also...)
	missing := func(spans []collectedSpan) []string {
		var ids []string
		for _, id := range jobs {
			if !slices.ContainsFunc(spans, func(s collectedSpan) bool { return s.attributes["vamos.job"] == id }) {
				ids = append(ids, id)
			}
		}
		return ids
	}
	if ids := missing(w.received(func(spans []collectedSpan) bool { return len(missing(spans)) == 0 })); ids != nil {
		w.t.Fatalf("the collector received no span of the jobs %q", ids)
	}
	return w.settled()
}

// settled returns the spans the collector received once it has received none for a while.
func (w *world) settled() []collectedSpan {
	w.t.Helper()
	quiet := 10 * exportDelay
	if w.telemetryEnv["OTEL_BSP_SCHEDULE_DELAY"] == "" {
		quiet = 6 * time.Second // the default schedule delay, past which a coordinator exporting unasked would send
	}
	n := -1
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(quiet) {
		spans := w.collector.spans()
		if len(spans) == n {
			return spans
		}
		n = len(spans)
	}
	return w.collector.spans()
}

// ThenTheCollectorReceivedNoSpan checks that the collector has received no span, once the coordinator would have sent
// the last command's, which stops the scenario if it has.
func (w *world) ThenTheCollectorReceivedNoSpan() {
	w.t.Helper()
	if w.collector == nil {
		w.t.Fatal("no GivenAnOTLPCollector step started a collector")
	}
	if got := w.settled(); len(got) != 0 {
		w.t.Fatalf("the collector received %d spans, such as %q; want none", len(got), got[0].name)
	}
}

// ThenTheCollectorReceivedTheTracesOf checks that the collector received a trace for each job of the kinds given, a
// build's, a push's, and no other, one for each kind given, each whose root span is the job's, and stops the scenario
// if not.
func (w *world) ThenTheCollectorReceivedTheTracesOf(kinds ...string) {
	w.t.Helper()
	spans := w.collected()
	jobs := jobSpans(spans)
	var got []string
	traces := map[string]string{} // the kind of job each trace is of
	for kind, spans := range jobs {
		for _, s := range spans {
			got = append(got, kind)
			if s.parent != "" {
				w.t.Errorf("the %s job's span is under %s; want it the root of its trace", kind, s.parent)
			}
			if other, ok := traces[s.trace]; ok {
				w.t.Errorf("the %s job's span is in the trace %s of a %s job; want a trace of its own", kind, s.trace,
					other)
			}
			traces[s.trace] = kind
		}
	}
	for _, s := range spans {
		if _, ok := traces[s.trace]; !ok {
			w.t.Errorf("%q is in the trace %s, of no job; want every span in a job's trace", s.name, s.trace)
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(kinds))
	if !slices.Equal(got, want) {
		w.t.Fatalf("the collector received the job spans of %q; want %q", got, want)
	}
}

// theJobSpan returns the span of the one job of kind that the collector received, stopping the scenario if it
// received none, or several.
func (w *world) theJobSpan(kind string) collectedSpan {
	w.t.Helper()
	return w.theJobSpanOf(w.collected(), kind)
}

// theJobSpanOf returns the span of the one job of kind among spans, as theJobSpan does.
func (w *world) theJobSpanOf(spans []collectedSpan, kind string) collectedSpan {
	w.t.Helper()
	jobs := jobSpans(spans)[kind]
	if len(jobs) != 1 {
		w.t.Fatalf("the collector received %d %s job spans; want one", len(jobs), kind)
	}
	return jobs[0]
}

// exportedSpan returns the one span named name, or starting with name, if it ends in "*", placeholders standing for
// what atPlaceholders says, that the collector received, stopping the scenario if it received none, or several.
func (w *world) exportedSpan(name string) collectedSpan {
	w.t.Helper()
	return w.spanNamed(w.collected(), name)
}

// spanNamed returns the one span of spans named name, as exportedSpan does.
func (w *world) spanNamed(spans []collectedSpan, name string) collectedSpan {
	w.t.Helper()
	name = w.atPlaceholders(name)
	var found []collectedSpan
	for _, s := range spans {
		if matches(s.name, name) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("the collector received %d spans named %q; want one", len(found), name)
	}
	return found[0]
}

// ThenTheJobSpanIsUnder checks that the span of the one job of kind is in the trace trace, a child of the span
// parent, both in hex, as a traceparent names them.
func (w *world) ThenTheJobSpanIsUnder(kind, trace, parent string) {
	w.t.Helper()
	s := w.theJobSpan(kind)
	if s.trace != trace || s.parent != parent {
		w.t.Errorf("the %s job's span is in %s under %q; want in %s under %s", kind, s.trace, s.parent, trace, parent)
	}
}

// ThenTheJobSpanIsTheRootOfItsOwnTrace checks that the span of the one job of kind has no parent, and isn't in the
// trace other.
func (w *world) ThenTheJobSpanIsTheRootOfItsOwnTrace(kind, other string) {
	w.t.Helper()
	s := w.theJobSpan(kind)
	if s.parent != "" || s.trace == other {
		w.t.Errorf("the %s job's span is in %s under %q; want the root of a trace of its own", kind, s.trace, s.parent)
	}
}

// ThenTheJobSpanHasTraceState checks that the span of the one job of kind carries the tracestate state exactly.
func (w *world) ThenTheJobSpanHasTraceState(kind, state string) {
	w.t.Helper()
	if s := w.theJobSpan(kind); s.state != state {
		w.t.Errorf("the %s job's span has tracestate %q; want %q", kind, s.state, state)
	}
}

// ThenTheExportedSpanIsWithin checks that the span named name lies within the span named outer, in its trace, a
// descendant of it, starting no earlier and ending no later.
func (w *world) ThenTheExportedSpanIsWithin(name, outer string) {
	w.t.Helper()
	spans := w.collected()
	byID := map[string]collectedSpan{}
	for _, s := range spans {
		byID[s.id] = s
	}
	inner, out := w.spanNamed(spans, name), w.spanNamed(spans, outer)
	descends := false
	// At most as many hops as there are spans, so that a cycle of parents fails rather than hangs.
	for p, hops := byID[inner.parent], 0; p.id != "" && hops < len(spans); p, hops = byID[p.parent], hops+1 {
		if p.id == out.id {
			descends = true
			break
		}
	}
	if !descends || inner.trace != out.trace || inner.start.Before(out.start) || inner.end.After(out.end) {
		w.t.Errorf("%q, from %s to %s, isn't within %q, from %s to %s, in its trace", name, inner.start, inner.end,
			outer, out.start, out.end)
	}
}

// ThenTheExportedSpanLastsAtLeast checks that the span named name lasts d or more.
func (w *world) ThenTheExportedSpanLastsAtLeast(name string, d time.Duration) {
	w.t.Helper()
	if s := w.exportedSpan(name); s.end.Sub(s.start) < d {
		w.t.Errorf("%q lasts %s; want at least %s", name, s.end.Sub(s.start), d)
	}
}

// ThenTheExportedSpanLinksTo checks that the span named name links to the span named target, whichever trace it is
// in.
func (w *world) ThenTheExportedSpanLinksTo(name, target string) {
	w.t.Helper()
	spans := w.collected()
	s, to := w.spanNamed(spans, name), w.spanNamed(spans, target)
	if !slices.Contains(s.links, to.trace+"/"+to.id) {
		w.t.Errorf("%q links to %q; want it to link to %q, %s", name, s.links, target, to.id)
	}
}

// ThenTheExportedSpanLinksToNothing checks that the span named name has no link.
func (w *world) ThenTheExportedSpanLinksToNothing(name string) {
	w.t.Helper()
	if s := w.exportedSpan(name); len(s.links) != 0 {
		w.t.Errorf("%q links to %q; want nothing", name, s.links)
	}
}

// ThenTheExportedSpanSaysAtLeast checks that the span named name has at least the attributes want, as text.
func (w *world) ThenTheExportedSpanSaysAtLeast(name string, want map[string]string) {
	w.t.Helper()
	s := w.exportedSpan(name)
	for key, value := range want {
		if got, ok := s.attributes[key]; !ok || got != value {
			w.t.Errorf("%q has %s %q; want %q", name, key, got, value)
		}
	}
}

// ThenNoExportedSpanHas checks that none of the spans the collector received has the attribute key.
func (w *world) ThenNoExportedSpanHas(key string) {
	w.t.Helper()
	for _, s := range w.collected() {
		if v, ok := s.attributes[key]; ok {
			w.t.Errorf("%q has %s %q; want no span to have it", s.name, key, v)
		}
	}
}

// ThenTheCollectorReceivedOneSpanNamed checks that the collector received the span named name once, whichever trace
// it is in, as an attempt's steps are, however many jobs it served, the other client's among them.
func (w *world) ThenTheCollectorReceivedOneSpanNamed(name string) {
	w.t.Helper()
	w.spanNamed(w.collected(w.otherJob), name)
}

// ThenTheTraceHoldsASpanNamed checks that the trace of the one job of kind holds a span named name.
func (w *world) ThenTheTraceHoldsASpanNamed(kind, name string) {
	w.t.Helper()
	spans := w.collected()
	job := w.theJobSpanOf(spans, kind)
	for _, s := range spans {
		if s.trace == job.trace && matches(s.name, name) {
			return
		}
	}
	w.t.Errorf("the %s job's trace, %s, holds no span named %q", kind, job.trace, name)
}

// ThenEachTraceIsWholeOrAbsent checks that every trace the collector received holds its job's span and a span named
// each of names, and stops the scenario if it received none of the jobs' traces, of which there are jobs, or all.
func (w *world) ThenEachTraceIsWholeOrAbsent(jobs int, names ...string) {
	w.t.Helper()
	spans := w.settled()
	byTrace := map[string][]collectedSpan{}
	for _, s := range spans {
		byTrace[s.trace] = append(byTrace[s.trace], s)
	}
	for trace, spans := range byTrace {
		var have []string
		for _, s := range spans {
			have = append(have, s.name)
		}
		if !slices.ContainsFunc(spans, func(s collectedSpan) bool { return s.attributes["vamos.job"] != "" }) {
			w.t.Errorf("the trace %s holds no job span: %q", trace, have)
		}
		for _, name := range names {
			if !slices.ContainsFunc(have, func(h string) bool { return matches(h, name) }) {
				w.t.Errorf("the trace %s holds no span named %q: %q", trace, name, have)
			}
		}
	}
	if n := len(byTrace); n == 0 || n == jobs {
		w.t.Fatalf("the collector received %d of the %d jobs' traces; want some, but not all", n, jobs)
	}
}

// ThenTheTraceFileNamesTheExportedTrace checks that the top-level spans of the build's job in the trace that --trace
// wrote name the trace that the collector received the job in, sampled.
func (w *world) ThenTheTraceFileNamesTheExportedTrace() {
	w.t.Helper()
	w.ThenTheTraceFileNamesTrace(w.theJobSpan("build").trace, true)
}

// ThenTheTraceFileNamesTrace checks that a top-level span of the executor in the trace that --trace wrote names the
// OpenTelemetry trace trace, as sampled says, and that no span names another.
func (w *world) ThenTheTraceFileNamesTrace(trace string, sampled bool) {
	w.t.Helper()
	named := 0
	for _, s := range w.theTrace().slices {
		got, ok := s.Args["otel trace"]
		if !ok {
			continue
		}
		named++
		if got != trace || s.Args["otel sampled"] != sampled {
			w.t.Errorf("%q names the trace %v, sampled %v; want %s, sampled %v", s.Name, got, s.Args["otel sampled"],
				trace, sampled)
		}
	}
	if named == 0 {
		w.t.Errorf("no span of the trace file names an OpenTelemetry trace; want the job's top-level spans to")
	}
}

// ThenTheTraceFileNamesNoExportedTrace checks that no span of the trace that --trace wrote names an OpenTelemetry
// trace.
func (w *world) ThenTheTraceFileNamesNoExportedTrace() {
	w.t.Helper()
	for _, s := range w.theTrace().slices {
		if got, ok := s.Args["otel trace"]; ok {
			w.t.Errorf("%q names the OpenTelemetry trace %v; want none to", s.Name, got)
		}
	}
}

// GivenTheCollectorForgetsTheLastCommandsSpans waits until the collector has received the last command's jobs'
// spans, and forgets them, so that the steps that follow see only what later commands export.
func (w *world) GivenTheCollectorForgetsTheLastCommandsSpans() {
	w.t.Helper()
	w.collected() // which waits until no more come
	w.collector.mu.Lock()
	w.collector.got = nil
	w.collector.mu.Unlock()
}

// spanWithin returns the one span named name that lies within the span named target, its ancestor, stopping the
// scenario if there is none, or several.
func (w *world) spanWithin(spans []collectedSpan, name, target string) collectedSpan {
	w.t.Helper()
	byID := map[string]collectedSpan{}
	for _, s := range spans {
		byID[s.id] = s
	}
	var found []collectedSpan
	for _, s := range spans {
		if !matches(s.name, name) {
			continue
		}
		for p, hops := byID[s.parent], 0; p.id != "" && hops < len(spans); p, hops = byID[p.parent], hops+1 {
			if matches(p.name, target) {
				found = append(found, s)
				break
			}
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("the collector received %d spans named %q within %q; want one", len(found), name, target)
	}
	return found[0]
}

// ThenTheSpanWithinLinksToTheSpanWithin checks that the span named name within the span named target links to the
// span named other within the span named otherTarget.
func (w *world) ThenTheSpanWithinLinksToTheSpanWithin(name, target, other, otherTarget string) {
	w.t.Helper()
	spans := w.collected()
	from, to := w.spanWithin(spans, name, target), w.spanWithin(spans, other, otherTarget)
	if !slices.Contains(from.links, to.trace+"/"+to.id) {
		w.t.Errorf("%q within %q links to %q; want it to link to %q within %q, %s", name, target, from.links, other,
			otherTarget, to.id)
	}
}

// ThenATargetSpanLinksToAnAttemptInAnotherTrace checks that a span named name links to the span of an attempt, in
// another trace than its own, as the target span of a job that joined another's attempt does.
func (w *world) ThenATargetSpanLinksToAnAttemptInAnotherTrace(name string) {
	w.t.Helper()
	spans := w.collected(w.otherJob)     // whose trace holds the attempt
	byLink := map[string]collectedSpan{} // by trace/span, as a link names it
	for _, s := range spans {
		byLink[s.trace+"/"+s.id] = s
	}
	for _, s := range spans {
		if !matches(s.name, name) {
			continue
		}
		for _, l := range s.links {
			if a, ok := byLink[l]; ok && a.name == "attempt" && a.trace != s.trace {
				return
			}
		}
	}
	w.t.Errorf("no span named %q links to an attempt's span in another trace", name)
}
