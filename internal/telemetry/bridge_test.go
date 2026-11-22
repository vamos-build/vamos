package telemetry

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/vamos-build/vamos/internal/tracing"
)

// newTestBridge returns a bridge that exports to the exporter it returns, sampling as the environment env says.
func newTestBridge(t *testing.T, env map[string]string) (*Bridge, *tracetest.InMemoryExporter) {
	tracer, exporter := newTestTracer(t)
	s, err := readSampler(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	b := newBridge(tracer, s, "i")
	t.Cleanup(func() { _ = b.close(context.Background()) })
	return b, exporter
}

// exportedBy returns the spans exporter holds once b has converted what it was given.
func exportedBy(t *testing.T, b *Bridge, exporter *tracetest.InMemoryExporter) tracetest.SpanStubs {
	t.Helper()
	if err := b.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	return exporter.GetSpans()
}

// byName returns the spans exporter holds once b has converted what it was given, by name, failing if two share one.
func byName(t *testing.T, b *Bridge, exporter *tracetest.InMemoryExporter) map[string]tracetest.SpanStub {
	t.Helper()
	spans := map[string]tracetest.SpanStub{}
	for _, s := range exportedBy(t, b, exporter) {
		if _, ok := spans[s.Name]; ok {
			t.Fatalf("two spans are named %q", s.Name)
		}
		spans[s.Name] = s
	}
	return spans
}

// attrsOf returns s's attributes by key.
func attrsOf(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		attrs[kv.Key] = kv.Value
	}
	return attrs
}

// kind is a named string, as graph's kinds are.
type kind string

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// at returns the time n milliseconds after t0.
func at(n int) time.Time {
	return t0.Add(time.Duration(n) * time.Millisecond)
}

// TestBridgeJob checks how a job's own spans are exported: under the job's span, which is the trace's root, each
// under the span it is part of, with vamos's times, an instant as a zero-length span, an error as the span's status,
// and the attributes the design names, the workspace's on every span.
func TestBridgeJob(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build",
		Workspace: "com.acme.storefront"})
	exported := tracing.ExportedOf(ctx)
	wantTrace, wantJob := traceIDOf("i", "job-1"), jobSpanIDOf("i", "job-1")
	if exported != (tracing.Exported{TraceID: wantTrace.String(), SpanID: wantJob.String(), Sampled: true}) {
		t.Errorf("ExportedOf the job's context = %+v, want trace %s, span %s, sampled", exported, wantTrace, wantJob)
	}

	spans := []tracing.Span{
		{ID: "s-2", Parent: "s-1", Process: "vamos", Track: "main", Name: "run", Start: at(20), End: at(80),
			Args: map[string]any{"error": "exit status 1", "exit code": 1.0, "argv": []any{"make", "all"},
				"cpu": 0.25, "outputs": map[string]any{"bin": "app"}, "signal": int32(9), "bytes": uint64(1 << 40),
				"load": float32(0.5), "took": 1500 * time.Millisecond, "kind": kind("layer"),
				"kinds": []kind{"layer", "task"}}},
		{ID: "s-1", Parent: "client-span", Process: "vamos", Track: "main", Name: `layer "app"`, Start: at(10),
			End: at(90), Args: map[string]any{"target": "//:app", "miss": "own-inputs",
				"changed": []string{"a.txt"}, "changed count": 1, "attempt": "att-1", "hit": false}},
		{ID: "s-3", Process: "vamos", Track: "main", Name: "skipped", Start: at(50), End: at(50), Instant: true,
			Args: map[string]any{"error": map[string]any{"code": 2}}}, // not a string, which fails it all the same
		{ID: "s-4", Parent: "s-1", Process: "vamos", Track: "main", Name: strings.Repeat("é", 1000), Start: at(30),
			End: at(40)},
	}
	b.Job(ctx, spans, at(0), at(100), "failed")

	got := byName(t, b, exporter)
	if len(got) != 5 {
		t.Fatalf("exported %d spans, want the job's and its 4", len(got))
	}
	if _, ok := got[strings.Repeat("é", (maxNameSize-len("…"))/2)+"…"]; !ok {
		t.Errorf("no span has the long name cut to whole runes and an ellipsis within %d bytes", maxNameSize)
	}
	job, layer, run, skipped := got["build"], got[`layer "app"`], got["run"], got["skipped"]
	for _, c := range []struct {
		name              string
		span              tracetest.SpanStub
		id, parent        trace.SpanID
		start, end        time.Time
		status            codes.Code
		statusDescription string
	}{
		{name: "the job", span: job, id: wantJob, start: at(0), end: at(100), status: codes.Error},
		{name: "the layer", span: layer, id: spanIDOf("i", "job-1", "s-1"), parent: wantJob, start: at(10),
			end: at(90)},
		{name: "the run", span: run, id: spanIDOf("i", "job-1", "s-2"), parent: spanIDOf("i", "job-1", "s-1"),
			start: at(20), end: at(80), status: codes.Error, statusDescription: "exit status 1"},
		{name: "the instant", span: skipped, id: spanIDOf("i", "job-1", "s-3"), parent: wantJob, start: at(50),
			end: at(50), status: codes.Error, statusDescription: `{"code":2}`},
	} {
		sc := c.span.SpanContext
		if sc.TraceID() != wantTrace || sc.SpanID() != c.id || c.span.Parent.SpanID() != c.parent {
			t.Errorf("%s is %s/%s under %s, want %s/%s under %s", c.name, sc.TraceID(), sc.SpanID(),
				c.span.Parent.SpanID(), wantTrace, c.id, c.parent)
		}
		if !c.span.StartTime.Equal(c.start) || !c.span.EndTime.Equal(c.end) {
			t.Errorf("%s ran %s to %s, want %s to %s", c.name, c.span.StartTime, c.span.EndTime, c.start, c.end)
		}
		if c.span.Status.Code != c.status || c.span.Status.Description != c.statusDescription {
			t.Errorf("%s's status = %v %q, want %v %q", c.name, c.span.Status.Code, c.span.Status.Description,
				c.status, c.statusDescription)
		}
		if ws := attrsOf(c.span)["vamos.workspace"]; ws.AsString() != "com.acme.storefront" {
			t.Errorf("%s's vamos.workspace = %q, want com.acme.storefront", c.name, ws.AsString())
		}
	}

	for _, c := range []struct {
		name string
		span tracetest.SpanStub
		want map[attribute.Key]attribute.Value
	}{
		{name: "the job", span: job, want: map[attribute.Key]attribute.Value{
			"vamos.workspace":   attribute.StringValue("com.acme.storefront"),
			"vamos.job":         attribute.StringValue("job-1"),
			"vamos.job.kind":    attribute.StringValue("build"),
			"vamos.job.outcome": attribute.StringValue("failed"),
		}},
		{name: "the layer", span: layer, want: map[attribute.Key]attribute.Value{
			"vamos.workspace":         attribute.StringValue("com.acme.storefront"),
			"vamos.process":           attribute.StringValue("vamos"),
			"vamos.track":             attribute.StringValue("main"),
			"vamos.target":            attribute.StringValue("//:app"),
			"vamos.miss.cause":        attribute.StringValue("own-inputs"),
			"vamos.miss.changed":      attribute.StringSliceValue([]string{"a.txt"}),
			"vamos.arg.changed_count": attribute.IntValue(1),
			"vamos.attempt":           attribute.StringValue("att-1"),
			"vamos.arg.hit":           attribute.BoolValue(false),
		}},
		{name: "the run", span: run, want: map[attribute.Key]attribute.Value{
			"vamos.workspace":     attribute.StringValue("com.acme.storefront"),
			"vamos.process":       attribute.StringValue("vamos"),
			"vamos.track":         attribute.StringValue("main"),
			"vamos.arg.exit_code": attribute.Int64Value(1),
			"vamos.arg.argv":      attribute.StringSliceValue([]string{"make", "all"}),
			"vamos.arg.cpu":       attribute.Float64Value(0.25),
			"vamos.arg.outputs":   attribute.StringValue(`{"bin":"app"}`),
			"vamos.arg.signal":    attribute.Int64Value(9),
			"vamos.arg.bytes":     attribute.Int64Value(1 << 40),
			"vamos.arg.load":      attribute.Float64Value(0.5),
			"vamos.arg.took":      attribute.Int64Value(1500000000),
			"vamos.arg.kind":      attribute.StringValue("layer"),
			"vamos.arg.kinds":     attribute.StringSliceValue([]string{"layer", "task"}),
		}},
	} {
		if got := attrsOf(c.span); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s's attributes = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBridgeUnnamedWorkspace checks that the spans of a job of an unnamed workspace carry no vamos.workspace.
func TestBridgeUnnamedWorkspace(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	b.Job(ctx, []tracing.Span{{ID: "s-1", Name: "step", Start: at(0), End: at(1)}}, at(0), at(2), "succeeded")
	for _, s := range exportedBy(t, b, exporter) {
		if _, ok := attrsOf(s)["vamos.workspace"]; ok {
			t.Errorf("%s has vamos.workspace", s.Name)
		}
	}
}

// TestBridgeOuterTrace checks that a job that joined an outer trace is exported in it, its span a child of the outer
// span, carrying the outer tracestate, and that one whose traceparent isn't valid starts a trace of its own.
func TestBridgeOuterTrace(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	outerTrace, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	outerSpan, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build",
		Parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", State: "ci=pipeline"})
	if e := tracing.ExportedOf(ctx); e.TraceID != outerTrace.String() || !e.Sampled {
		t.Errorf("ExportedOf the job's context = %+v, want trace %s, sampled", e, outerTrace)
	}
	b.Job(ctx, nil, at(0), at(1), "succeeded")
	job := byName(t, b, exporter)["build"]
	if job.SpanContext.TraceID() != outerTrace || job.Parent.SpanID() != outerSpan || !job.Parent.IsRemote() {
		t.Errorf("the job's span is in %s under %s, want in %s under the remote %s",
			job.SpanContext.TraceID(), job.Parent.SpanID(), outerTrace, outerSpan)
	}
	if state := job.SpanContext.TraceState().String(); state != "ci=pipeline" {
		t.Errorf("the job's tracestate = %q, want ci=pipeline", state)
	}
	// Only the outer span is remote; the job's own attempt is under the job's span, which this process made.
	b.AttemptStarted(ctx, "att-1")
	b.Attempt(ctx, "att-1", []tracing.Span{{ID: "s-1", Name: "step", Start: at(0), End: at(1)}}, at(0), at(1))
	for name, s := range byName(t, b, exporter) {
		if name != "build" && (!s.Parent.IsValid() || s.Parent.IsRemote()) {
			t.Errorf("%s's parent %s is remote or none, want the local span it is under", name, s.Parent.SpanID())
		}
	}

	exporter.Reset()
	ctx = b.StartJob(context.Background(), JobInfo{ID: "job-2", Kind: "build",
		Parent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01"})
	b.Job(ctx, nil, at(0), at(1), "succeeded")
	job = byName(t, b, exporter)["build"]
	if job.SpanContext.TraceID() != traceIDOf("i", "job-2") || job.Parent.IsValid() {
		t.Errorf("the job of an invalid traceparent is in %s under %s, want the root of %s",
			job.SpanContext.TraceID(), job.Parent.SpanID(), traceIDOf("i", "job-2"))
	}
}

// TestBridgeDroppedTrace checks that a job whose trace isn't sampled, here under an unsampled outer trace, exports no
// span at all, its attempts' none either, and says it wasn't sampled.
func TestBridgeDroppedTrace(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build",
		Parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"})
	if e := tracing.ExportedOf(ctx); e.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || e.Sampled {
		t.Errorf("ExportedOf the job's context = %+v, want the outer trace, not sampled", e)
	}
	if e := b.AttemptStarted(ctx, "att-1"); e.Sampled {
		t.Errorf("AttemptStarted = %+v, want not sampled", e)
	}
	spans := []tracing.Span{{ID: "s-1", Name: "step", Start: at(0), End: at(1)}}
	b.Attempt(ctx, "att-1", spans, at(0), at(1))
	b.Job(ctx, spans, at(0), at(2), "succeeded")
	if got := exportedBy(t, b, exporter); len(got) != 0 {
		t.Errorf("exported %d spans of a dropped trace, want none", len(got))
	}
}

// TestBridgeAttempt checks that an attempt's spans are exported into its starter's trace, under a span of the
// attempt's own, which lies under the span the attempt's work is part of, the starter's target.
func TestBridgeAttempt(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	ctx = tracing.WithRecorder(ctx, tracing.NewRecorder("vamos", nil), "")
	ctx, target := tracing.Start(ctx, `layer "app"`)
	defer target.End()
	attemptCtx := tracing.WithRecorderAt(context.WithoutCancel(ctx), tracing.NewRecorder("vamos", nil))

	exported := b.AttemptStarted(attemptCtx, "att-1")
	wantTrace, wantAttempt := traceIDOf("i", "job-1"), attemptSpanIDOf("att-1")
	if exported != (tracing.Exported{TraceID: wantTrace.String(), SpanID: wantAttempt.String(), Sampled: true}) {
		t.Errorf("AttemptStarted = %+v, want trace %s, span %s, sampled", exported, wantTrace, wantAttempt)
	}
	b.Attempt(attemptCtx, "att-1", []tracing.Span{
		{ID: "a-1", Parent: target.ID(), Process: "executor", Name: "run", Start: at(10), End: at(20)},
	}, at(5), at(25))

	got := byName(t, b, exporter)
	attempt, run := got["attempt"], got["run"]
	if attempt.SpanContext.TraceID() != wantTrace || attempt.SpanContext.SpanID() != wantAttempt ||
		attempt.Parent.SpanID() != spanIDOf("i", "job-1", target.ID()) {
		t.Errorf("the attempt's span is %s/%s under %s, want %s/%s under the target's %s",
			attempt.SpanContext.TraceID(), attempt.SpanContext.SpanID(), attempt.Parent.SpanID(), wantTrace,
			wantAttempt, spanIDOf("i", "job-1", target.ID()))
	}
	if !attempt.StartTime.Equal(at(5)) || !attempt.EndTime.Equal(at(25)) {
		t.Errorf("the attempt ran %s to %s, want %s to %s", attempt.StartTime, attempt.EndTime, at(5), at(25))
	}
	if run.Parent.SpanID() != wantAttempt || run.SpanContext.SpanID() != spanIDOf("i", "job-1", "a-1") {
		t.Errorf("the attempt's run is %s under %s, want %s under the attempt's span",
			run.SpanContext.SpanID(), run.Parent.SpanID(), spanIDOf("i", "job-1", "a-1"))
	}
	if a := attrsOf(attempt)["vamos.attempt"]; a.AsString() != "att-1" {
		t.Errorf("the attempt's span has vamos.attempt %q, want att-1", a.AsString())
	}
}

// linksOf returns the span contexts that s links to.
func linksOf(s tracetest.SpanStub) []trace.SpanContext {
	var scs []trace.SpanContext
	for _, l := range s.Links {
		scs = append(scs, l.SpanContext)
	}
	return scs
}

// TestBridgeLinks checks the links exported spans get: a joiner's target links to the attempt's span that another
// job started, but the starter's own target doesn't; a span that uses what another made links to it, in the same
// snapshot whatever their order, or in an earlier one of another job; and a span that uses what no exported span made,
// or only a dropped trace's did, gets none.
func TestBridgeLinks(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	starter := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	joiner := b.StartJob(context.Background(), JobInfo{ID: "job-2", Kind: "push"})
	dropped := b.StartJob(context.Background(), JobInfo{ID: "job-3", Kind: "build",
		Parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"})

	b.AttemptStarted(starter, "att-1")
	b.Attempt(starter, "att-1", []tracing.Span{
		{ID: "a-1", Name: "commit base", Start: at(1), End: at(2), Produces: "image:base"},
	}, at(0), at(3))
	b.Job(dropped, []tracing.Span{
		{ID: "d-1", Name: "dropped commit", Start: at(1), End: at(2), Produces: "image:dropped"},
	}, at(0), at(3), "succeeded")
	b.Job(starter, []tracing.Span{
		{ID: "s-2", Name: "start app", Start: at(4), End: at(5), Uses: "image:app-base"},
		{ID: "s-3", Name: "commit app-base", Start: at(1), End: at(2), Produces: "image:app-base"},
		{ID: "s-1", Name: `starter's layer "base"`, Start: at(0), End: at(6),
			Args: map[string]any{"attempt": "att-1", tracing.Role: tracing.RoleTarget}},
		{ID: "s-4", Name: "cache write", Start: at(0), End: at(6), Args: map[string]any{"attempt": "att-1"}},
	}, at(0), at(7), "succeeded")
	b.Job(joiner, []tracing.Span{
		{ID: "j-1", Name: `joiner's layer "base"`, Start: at(0), End: at(6),
			Args: map[string]any{"attempt": "att-1", tracing.Role: tracing.RoleTarget}},
		{ID: "j-5", Name: "joiner's cache write", Start: at(0), End: at(6), Args: map[string]any{"attempt": "att-1"}},
		{ID: "j-2", Name: "start from base", Start: at(4), End: at(5), Uses: "image:base"},
		{ID: "j-3", Name: "add sources", Start: at(4), End: at(5), Uses: "tree:client"},
		{ID: "j-4", Name: "start from dropped", Start: at(4), End: at(5), Uses: "image:dropped"},
	}, at(0), at(7), "succeeded")

	got := byName(t, b, exporter)
	starterOf := func(id trace.SpanID) trace.SpanContext {
		return trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceIDOf("i", "job-1"), SpanID: id,
			TraceFlags: trace.FlagsSampled})
	}
	for _, c := range []struct {
		span string
		want []trace.SpanContext
	}{
		{span: `joiner's layer "base"`, want: []trace.SpanContext{starterOf(attemptSpanIDOf("att-1"))}},
		{span: `starter's layer "base"`},
		{span: "joiner's cache write"},
		{span: "start from base", want: []trace.SpanContext{starterOf(spanIDOf("i", "job-1", "a-1"))}},
		{span: "start app", want: []trace.SpanContext{starterOf(spanIDOf("i", "job-1", "s-3"))}},
		{span: "add sources"},
		{span: "start from dropped"},
	} {
		span, ok := got[c.span]
		if !ok {
			t.Errorf("no span %s was exported", c.span)
			continue
		}
		if links := linksOf(span); !slices.EqualFunc(links, c.want, trace.SpanContext.Equal) {
			t.Errorf("%s links to %v, want %v", c.span, links, c.want)
		}
	}
}

// TestLRUForgetsTheLeastRecent checks that an lru past its size forgets the entry least recently put or got.
func TestLRUForgetsTheLeastRecent(t *testing.T) {
	l := newLRU[string, int](2)
	l.put("a", 1)
	l.put("b", 2)
	if _, ok := l.get("a"); !ok { // now b is the least recent
		t.Fatal("a was forgotten before the lru was full")
	}
	l.put("c", 3)
	if _, ok := l.get("b"); ok {
		t.Error("b, the least recent, is still remembered past the lru's size")
	}
	for key, want := range map[string]int{"a": 1, "c": 3} {
		if got, ok := l.get(key); !ok || got != want {
			t.Errorf("get(%s) = %d, %t; want %d", key, got, ok, want)
		}
	}
	l.put("a", 4)
	if got, _ := l.get("a"); got != 4 || len(l.entries) != 2 {
		t.Errorf("after a was put again, it is %d with %d entries; want 4 with 2", got, len(l.entries))
	}
}

// gatedExporter is an exporter that holds each export until the test opens its gate.
type gatedExporter struct {
	*tracetest.InMemoryExporter
	entered chan struct{} // receives as each export starts
	gate    chan struct{}
}

func (e gatedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.entered <- struct{}{}
	<-e.gate
	return e.InMemoryExporter.ExportSpans(ctx, spans)
}

// TestBridgeDropsWholeSnapshots checks that a bridge whose exporter falls behind, with more waiting than it holds,
// drops the next snapshot whole, reporting it, and every later snapshot of its job, while it sends the snapshots
// before it, and other jobs', whole, those of a job in the same outer trace among them,
// the builds that gave them waiting for none of it; and that a span that joined a dropped attempt links to none,
// while one that joined an attempt that was sent links to it, though its job lost a later snapshot.
func TestBridgeDropsWholeSnapshots(t *testing.T) {
	exporter := gatedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}, 64),
		gate: make(chan struct{})}
	provider := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(chosenIDs{}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())), sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	b := newBridge(provider.Tracer("test"), sampler{Sampler: sdktrace.AlwaysSample()}, "i")
	t.Cleanup(func() { _ = b.close(context.Background()) })
	var mu sync.Mutex
	var warnings []string
	HandleErrors(func(w string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, w)
	})
	t.Cleanup(func() { HandleErrors(func(string) {}) })

	two := func(prefix string) []tracing.Span {
		return []tracing.Span{{ID: prefix + "-1", Name: prefix + " one", Start: at(0), End: at(1)},
			{ID: prefix + "-2", Name: prefix + " two", Start: at(0), End: at(1)}}
	}
	b.maxBytes = estimatedSize(two("x")) // one snapshot of two spans
	// job-1 and job-2 joined one outer trace, as the steps of one pipeline do, and job-3 has its own.
	outer := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	job1 := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build", Parent: outer})
	job2 := b.StartJob(context.Background(), JobInfo{ID: "job-2", Kind: "push", Parent: outer})
	job3 := b.StartJob(context.Background(), JobInfo{ID: "job-3", Kind: "export"})
	b.AttemptStarted(job1, "att-a")
	b.AttemptStarted(job1, "att-c")
	b.AttemptStarted(job1, "att-e") // still running when job-1 is cut, so its snapshot will be dropped
	b.Attempt(job1, "att-a", two("a"), at(0), at(2))
	<-exporter.entered                               // the converter takes a, and waits in its first export
	b.Attempt(job1, "att-b", two("b"), at(0), at(2)) // waits
	b.Attempt(job1, "att-c", two("c"), at(0), at(2)) // would pass the bound: dropped, and job-1 cut
	close(exporter.gate)
	if err := b.flush(t.Context()); err != nil { // so that nothing waits
		t.Fatal(err)
	}
	b.Job(job1, two("j"), at(0), at(3), "succeeded") // job-1's: dropped, though nothing waits
	b.AttemptStarted(job1, "att-d")                  // whose snapshot will be dropped, as job-1's are
	b.AttemptStarted(job2, "att-f")                  // still running as job-3's span is converted, so not linked
	b.Job(job2, two("k"), at(0), at(3), "succeeded") // another job's of the same outer trace: sent
	if err := b.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.Job(job3, []tracing.Span{
		{ID: "l-1", Name: "joined c", Start: at(0), End: at(1),
			Args: map[string]any{"attempt": "att-c", tracing.Role: tracing.RoleTarget}},
		{ID: "l-2", Name: "joined a", Start: at(0), End: at(1),
			Args: map[string]any{"attempt": "att-a", tracing.Role: tracing.RoleTarget}},
		{ID: "l-3", Name: "joined d", Start: at(0), End: at(1),
			Args: map[string]any{"attempt": "att-d", tracing.Role: tracing.RoleTarget}},
		{ID: "l-4", Name: "joined e", Start: at(0), End: at(1),
			Args: map[string]any{"attempt": "att-e", tracing.Role: tracing.RoleTarget}},
		{ID: "l-5", Name: "joined f", Start: at(0), End: at(1),
			Args: map[string]any{"attempt": "att-f", tracing.Role: tracing.RoleTarget}},
	}, at(0), at(3), "succeeded") // links to a's span, which was sent, though its job was cut later, and no other

	var names []string
	var joinedA, joinedC, joinedD, joinedE, joinedF tracetest.SpanStub
	for _, s := range exportedBy(t, b, exporter.InMemoryExporter) {
		names = append(names, s.Name)
		switch s.Name {
		case "joined a":
			joinedA = s
		case "joined c":
			joinedC = s
		case "joined d":
			joinedD = s
		case "joined e":
			joinedE = s
		case "joined f":
			joinedF = s
		}
	}
	slices.Sort(names)
	want := []string{"a one", "a two", "attempt", "attempt", "b one", "b two", "export", "joined a", "joined c",
		"joined d", "joined e", "joined f", "k one", "k two", "push"}
	if !slices.Equal(names, want) {
		t.Errorf("exported %q, want %q: a's, b's, job-2's and job-3's whole, and none of job-1's after c", names, want)
	}
	if links := linksOf(joinedC); len(links) != 0 {
		t.Errorf("a span that joined the dropped attempt links to %v, want nothing", links)
	}
	if links := linksOf(joinedD); len(links) != 0 {
		t.Errorf("a span that joined an attempt of a cut job links to %v, want nothing", links)
	}
	if links := linksOf(joinedE); len(links) != 0 {
		t.Errorf("a span that joined an attempt still running as its job was cut links to %v, want nothing", links)
	}
	if links := linksOf(joinedF); len(links) != 0 {
		t.Errorf("a span that joined an attempt not yet queued links to %v, want nothing, as it may yet be dropped",
			links)
	}
	if links := linksOf(joinedA); len(links) != 1 || links[0].SpanID() != attemptSpanIDOf("att-a") {
		t.Errorf("a span that joined an attempt that was sent links to %v, want its span", links)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := slices.Clone(warnings)
		mu.Unlock()
		wantWarning := "exporting telemetry: dropping a job's spans, and the rest of the job's, " +
			"since the exporter is falling behind"
		if slices.Contains(got, wantWarning) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warnings = %q, want %q among them", got, wantWarning)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBridgeBoundsWhatASpanHolds checks that an exported span holds at most the bridge's budget, whatever vamos
// recorded: its name, its status's description, each key and string cut, each list cut short, and no attributes past
// maxAttributesSize but vamos.truncated.
func TestBridgeBoundsWhatASpanHolds(t *testing.T) {
	b, exporter := newTestBridge(t, nil)
	ctx := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	paths := make([]string, 50000)
	for i := range paths {
		paths[i] = strings.Repeat("p", 2000)
	}
	args := map[string]any{"error": strings.Repeat("e", 1<<20), strings.Repeat("k", 1000): "v", "changed": paths,
		"target": "//:app", "miss": "own-inputs", "a broken name": "a\xffb"}
	for i := range 100 {
		args[fmt.Sprint("output ", i)] = strings.Repeat("o", 1<<20)
	}
	b.Job(ctx, []tracing.Span{{ID: "s-1", Name: strings.Repeat("n", 1<<20), Start: at(0), End: at(1), Args: args}},
		at(0), at(2), "succeeded")
	var span tracetest.SpanStub
	for _, s := range exportedBy(t, b, exporter) {
		if s.Name != "build" {
			span = s
		}
	}
	size := len(span.Name) + len(span.Status.Description)
	truncated := false
	attrs := attrsOf(span)
	if attrs["vamos.target"].AsString() != "//:app" || attrs["vamos.miss.cause"].AsString() != "own-inputs" {
		t.Errorf("the target's attributes weren't kept first: %v", attrs)
	}
	if changed := attrs["vamos.miss.changed"].AsStringSlice(); len(changed) == 0 {
		t.Error("the list of changed files was dropped whole, rather than cut to what the budget leaves")
	}
	if name := attrs["vamos.arg.a_broken_name"].AsString(); name != "a\uFFFDb" {
		t.Errorf("a string that isn't UTF-8 is exported as %q, want it made valid", name)
	}
	for _, kv := range span.Attributes {
		size += len(kv.Key) + valueBytes(kv.Value)
		truncated = truncated || kv.Key == "vamos.truncated"
		if len(kv.Key) > maxKeySize+len("…") {
			t.Errorf("a key of %d bytes was kept", len(kv.Key))
		}
		if kv.Value.Type() == attribute.STRINGSLICE && len(kv.Value.AsStringSlice()) > maxListLength {
			t.Errorf("%s kept %d strings", kv.Key, len(kv.Value.AsStringSlice()))
		}
	}
	if len(span.Name) > maxNameSize+len("…") || len(span.Status.Description) > maxValueSize+len("…") {
		t.Errorf("the name is %d bytes and the status %d, want each cut", len(span.Name), len(span.Status.Description))
	}
	if max := maxNameSize + maxValueSize + maxAttributesSize + 64; size > max || !truncated {
		t.Errorf("the span holds %d bytes, truncated %t; want at most %d, truncated", size, truncated, max)
	}
}

// TestBridgeAbandonDropsWhatWaits checks that abandoning a bridge whose converter an exporter blocks drops the
// snapshots that wait, reporting them, and converts no more, though the exporter later takes what it was given.
func TestBridgeAbandonDropsWhatWaits(t *testing.T) {
	exporter := gatedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}, 64),
		gate: make(chan struct{})}
	provider := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(chosenIDs{}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())), sdktrace.WithSyncer(exporter))
	b := newBridge(provider.Tracer("test"), sampler{Sampler: sdktrace.AlwaysSample()}, "i")
	var mu sync.Mutex
	var warnings []string
	HandleErrors(func(w string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, w)
	})
	t.Cleanup(func() { HandleErrors(func(string) {}) })

	job := b.StartJob(context.Background(), JobInfo{ID: "job-1", Kind: "build"})
	b.Attempt(job, "att-a", []tracing.Span{{ID: "a-1", Name: "a", Start: at(0), End: at(1)}}, at(0), at(2))
	<-exporter.entered // the converter waits in a's first export
	b.Attempt(job, "att-b", []tracing.Span{{ID: "b-1", Name: "b", Start: at(0), End: at(1)}}, at(0), at(2))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.close(ctx); err == nil {
		t.Fatal("close of a blocked bridge succeeded")
	}
	b.abandon()
	close(exporter.gate)
	select {
	case <-b.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the converter didn't stop once its exporter took what it was given")
	}
	_ = provider.Shutdown(context.Background())
	for _, s := range exporter.GetSpans() {
		if s.Name == "b" {
			t.Error("b, which waited when the bridge was abandoned, was exported")
		}
	}
	want := "exporting telemetry: dropping a job's spans, and the rest of the job's, " +
		"since the exporter is falling behind"
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := slices.Clone(warnings)
		mu.Unlock()
		if slices.Contains(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warnings = %q, want %q among them", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCut checks that cut makes a string valid UTF-8 and keeps it within its bytes, the ellipsis included, cutting at
// a rune's start, whatever the string holds.
func TestCut(t *testing.T) {
	for _, c := range []struct {
		name, s, want string
		n             int
	}{
		{name: "short", s: "abc", n: 8, want: "abc"},
		{name: "long", s: "abcdefgh", n: 6, want: "abc…"},
		{name: "a rune that the cut would split", s: "aéé", n: 4, want: "a…"},
		{name: "invalid bytes, made valid before the cut", s: "\xffa\xffa\xff", n: 6, want: "�…"},
		{name: "invalid bytes that fit once valid", s: "a\xffb", n: 5, want: "a�b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := cut(c.s, c.n)
			if got != c.want || len(got) > c.n || !utf8.ValidString(got) {
				t.Errorf("cut(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
		})
	}
}

// TestBoundedMarksWhatItCuts checks that bounded marks a span vamos.truncated when its budget shortens a list or
// drops an attribute, and not when it only cuts a string to maxValueSize, which every span's strings may be.
func TestBoundedMarksWhatItCuts(t *testing.T) {
	long := make([]string, maxListLength+1)
	for i := range long {
		long[i] = "f"
	}
	for _, c := range []struct {
		name  string
		attrs []attribute.KeyValue
		want  bool
	}{
		{name: "nothing cut", attrs: []attribute.KeyValue{attribute.String("a", "b"),
			attribute.StringSlice("empty", []string{})}},
		{name: "a long string",
			attrs: []attribute.KeyValue{attribute.String("a", strings.Repeat("b", 2*maxValueSize))}},
		{name: "a list longer than it keeps",
			attrs: []attribute.KeyValue{attribute.StringSlice("a", long)}, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := attrsOf(tracetest.SpanStub{Attributes: bounded(c.attrs)})
			if _, truncated := got["vamos.truncated"]; truncated != c.want {
				t.Errorf("bounded(%s) marked truncated: %t, want %t", c.name, truncated, c.want)
			}
		})
	}
}
