package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/telemetry"
	"github.com/vamos-build/vamos/internal/tracing"
)

// outerTraceparent is a W3C traceparent, sampled, that the tests' jobs join.
const (
	outerTrace       = "4bf92f3577b34da6a3ce929d0e0e4736"
	outerSpan        = "00f067aa0ba902b7"
	outerTraceparent = "00-" + outerTrace + "-" + outerSpan + "-01"
)

// exporting is what a coordinator's Config.Telemetry makes in a test: a bridge for each coordinator started, which
// exports to the console exporter, written to a buffer of its own that the test reads once every one is shut down.
type exporting struct {
	t        *testing.T
	mu       sync.Mutex
	consoles []*lockedBuffer // one for each coordinator started, in order
	shutdown []func(context.Context) error
}

// lockedBuffer is a buffer that a console exporter writes to while others may.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// newExporting returns an exporting that the test's cleanup shuts down, if it hasn't.
func newExporting(t *testing.T) *exporting {
	e := &exporting{t: t}
	t.Cleanup(e.shutdownAll)
	return e
}

// telemetry is a Config.Telemetry.
func (e *exporting) telemetry(instance string) (*telemetry.Telemetry, error) {
	console := &lockedBuffer{}
	tel, err := telemetry.Setup(telemetry.Config{Role: telemetry.Coordinator, Instance: instance,
		Getenv: func(name string) string {
			return map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_METRICS_EXPORTER": "none"}[name]
		},
		Console: console})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consoles = append(e.consoles, console)
	e.shutdown = append(e.shutdown, tel.Shutdown)
	return tel, nil
}

// shutdownAll shuts down every bridge made, so that what they hold is written.
func (e *exporting) shutdownAll() {
	e.mu.Lock()
	shutdown := e.shutdown
	e.shutdown = nil
	e.mu.Unlock()
	for _, s := range shutdown {
		if err := s(context.Background()); err != nil {
			e.t.Error(err)
		}
	}
}

// exportedSpan is what the tests read of a span the console exporter wrote.
type exportedSpan struct {
	Name        string
	SpanContext struct{ TraceID, SpanID, TraceState string }
	Parent      struct{ TraceID, SpanID string }
	Links       []struct {
		SpanContext struct{ TraceID, SpanID string }
	}
	Attributes []struct {
		Key   string
		Value struct{ Value any }
	}
}

// attr returns the value of the attribute key of s, or nil.
func (s exportedSpan) attr(key string) any {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value.Value
		}
	}
	return nil
}

// spans shuts down every bridge made and returns the spans that of the coordinator started nth exported.
func (e *exporting) spans(nth int) []exportedSpan {
	e.shutdownAll()
	e.mu.Lock()
	defer e.mu.Unlock()
	if nth >= len(e.consoles) {
		e.t.Fatalf("%d coordinators exported; want the %dth's spans", len(e.consoles), nth+1)
	}
	var spans []exportedSpan
	console := e.consoles[nth]
	console.mu.Lock() // as a shutdown that timed out may still be writing
	data := bytes.Clone(console.buf.Bytes())
	console.mu.Unlock()
	d := json.NewDecoder(bytes.NewReader(data))
	for {
		var s exportedSpan
		if err := d.Decode(&s); errors.Is(err, io.EOF) {
			return spans
		} else if err != nil {
			e.t.Fatalf("decoding the console's spans: %v", err)
		}
		spans = append(spans, s)
	}
}

// TestRestartJoinsOuterTrace checks that a build submitted with a traceparent, running when the coordinator
// restarts, is resumed in the outer trace, as the journal kept the traceparent with its request: the resumed run's
// job span is a child of the outer span, in its trace.
func TestRestartJoinsOuterTrace(t *testing.T) {
	e := newExporting(t)
	r := newRestartable(t, Config{Grace: 10 * time.Second, Telemetry: e.telemetry})
	wait, ended := untilKilled(t)
	blocked := startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(a *remote.Assignment,
		killed func() bool) error {
		wait(killed)
		return errors.New("killed")
	}))
	t.Cleanup(ended)
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := testGraph(t, r.fixture, "outer trace")
	req.Parent, req.State = outerTraceparent, "ci=pipeline"
	id, err := client.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return blocked.takenCount() >= 2 })

	r.restart()
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	if outcome := finish(t, client, id); outcome.Failure != nil {
		t.Fatalf("the resumed build failed: %+v", outcome.Failure)
	}
	r.stop()
	// The run the restart cancelled exports its job span too, of an ID of its own, in the same trace.
	var cancelled []exportedSpan
	for _, s := range e.spans(0) {
		if s.Name == "build" && s.attr("vamos.job") != nil {
			cancelled = append(cancelled, s)
		}
	}
	if len(cancelled) != 1 || cancelled[0].SpanContext.TraceID != outerTrace ||
		cancelled[0].Parent.SpanID != outerSpan || cancelled[0].attr("vamos.job.outcome") != "cancelled" ||
		cancelled[0].attr("vamos.job.run") != float64(0) {
		t.Errorf("the cancelled run exported the job spans %+v; want one, in %s under %s, cancelled, of run 0",
			cancelled, outerTrace, outerSpan)
	}
	builds := 0
	for _, s := range e.spans(1) { // the resumed run's, exported by the coordinator started again
		if s.Name != "build" {
			continue
		}
		builds++
		if len(cancelled) == 1 && s.SpanContext.SpanID == cancelled[0].SpanContext.SpanID {
			t.Errorf("the resumed run's job span has the cancelled run's ID, %s; want one of its own",
				s.SpanContext.SpanID)
		}
		if s.SpanContext.TraceID != outerTrace || s.Parent.SpanID != outerSpan ||
			s.SpanContext.TraceState != "ci=pipeline" || s.attr("vamos.job.run") != float64(1) {
			t.Errorf("the job's span is in %s under %s, with tracestate %q, of run %v; want in %s under %s, with "+
				"ci=pipeline, of run 1", s.SpanContext.TraceID, s.Parent.SpanID, s.SpanContext.TraceState,
				s.attr("vamos.job.run"), outerTrace, outerSpan)
		}
	}
	if builds != 1 {
		t.Errorf("the resumed run exported %d job spans; want 1", builds)
	}
}

// TestMalformedTraceparentIgnored checks that a request whose traceparent isn't one, as an older client or another
// program may send, is reported and ignored: the job runs, in a trace of its own.
func TestMalformedTraceparentIgnored(t *testing.T) {
	e := newExporting(t)
	var mu sync.Mutex
	var warnings []string
	r := newRestartable(t, Config{Telemetry: e.telemetry, Warn: func(err error) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, err.Error())
	}})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	req := testGraph(t, r.fixture, "malformed trace")
	req.Parent = "00-not-a-trace"
	id, status := postSubmission(t, r, "malformed", api.Request{Graph: &req})
	if status/100 != 2 {
		t.Fatalf("the submission was answered %d; want it accepted", status)
	}
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if outcome := finish(t, client, id); outcome.Failure != nil {
		t.Fatalf("the job failed: %+v", outcome.Failure)
	}
	r.stop()
	mu.Lock()
	want := "the job " + string(id) + `: its traceparent "00-not-a-trace" is not a W3C traceparent, and is ignored`
	matching := len(slices.DeleteFunc(slices.Clone(warnings), func(w string) bool { return w != want }))
	mu.Unlock()
	if matching != 1 {
		t.Errorf("warnings = %q; want %q once", warnings, want)
	}
	builds := 0
	for _, s := range e.spans(0) {
		if s.Name != "build" {
			continue
		}
		builds++
		if s.SpanContext.TraceID == outerTrace || s.Parent.SpanID != "0000000000000000" {
			t.Errorf("the job's span is in %s under %s; want the root of a trace of its own", s.SpanContext.TraceID,
				s.Parent.SpanID)
		}
	}
	if builds != 1 {
		t.Errorf("%d job spans were exported; want the build's", builds)
	}
}

// TestTelemetryFailureFreesState checks that a coordinator whose telemetry can't be set up fails to start, leaving
// its state for one started again.
func TestTelemetryFailureFreesState(t *testing.T) {
	state := t.TempDir()
	m := cache.NewMemory()
	failing := func(string) (*telemetry.Telemetry, error) { return nil, errors.New("OTEL_TRACES_EXPORTER=zipkin") }
	if _, err := New(m, m, Config{State: state, Telemetry: failing}); err == nil {
		t.Fatal("a coordinator whose telemetry failed started")
	}
	c, err := New(m, m, Config{State: state})
	if err != nil {
		t.Fatalf("starting again on the same state: %v", err)
	}
	c.Close()
}

// finish waits for the job id to end, and returns its outcome.
func finish(t *testing.T, client *remote.Client, id api.JobID) api.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := client.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatalf("watching the job: %v", err)
	}
	outcome, err := client.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// journaledParent returns the traceparent that the journal in state keeps with the build id.
func journaledParent(t *testing.T, state string, id api.JobID) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "jobs", string(id)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Build == nil {
		t.Fatalf("the journal's record of %s holds no build", id)
	}
	return r.Build.Parent
}

// TestJournalKeepsOuterTraceBounded checks that a traceparent of a later version, with fields of any size past those
// a job joins, is journaled as version 00, at a traceparent's length, with its flags.
func TestJournalKeepsOuterTraceBounded(t *testing.T) {
	state := t.TempDir()
	r := newRestartable(t, Config{State: state})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	req := testGraph(t, r.fixture, "later version")
	req.Parent = "01-" + outerTrace + "-" + outerSpan + "-03-" + strings.Repeat("later", 1<<14)
	id, status := postSubmission(t, r, "later", api.Request{Graph: &req})
	if status/100 != 2 {
		t.Fatalf("the submission was answered %d; want it accepted", status)
	}
	if got, want := journaledParent(t, state, id), "00-"+outerTrace+"-"+outerSpan+"-03"; got != want {
		t.Errorf("the journal keeps the traceparent %.80q; want %q", got, want)
	}
}

// TestResumedMalformedTraceparentReported checks that a build whose record, journaled by an older coordinator, holds
// a malformed traceparent is reported, naming the job, as it is resumed, once, though it is resumed twice, and runs
// in a trace of its own.
func TestResumedMalformedTraceparentReported(t *testing.T) {
	e := newExporting(t)
	var mu sync.Mutex
	var warnings []string
	state := t.TempDir()
	r := newRestartable(t, Config{State: state, Grace: 10 * time.Second, Telemetry: e.telemetry,
		Warn: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			warnings = append(warnings, err.Error())
		}})
	wait, ended := untilKilled(t)
	blocked := startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(a *remote.Assignment,
		killed func() bool) error {
		wait(killed)
		return errors.New("killed")
	}))
	t.Cleanup(ended)
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := testGraph(t, r.fixture, "older record")
	id, err := client.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return blocked.takenCount() >= 2 })

	r.stop()
	path := filepath.Join(state, "jobs", string(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Build.Parent = "00-not-a-trace" // as a coordinator that didn't check it journaled it
	if data, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(strings.TrimPrefix(r.url, "http://"))
	// Resumed, it blocks again, so that a second restart resumes the record the first repaired.
	wait2, ended2 := untilKilled(t)
	blocked2 := startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(a *remote.Assignment,
		killed func() bool) error {
		wait2(killed)
		return errors.New("killed")
	}))
	t.Cleanup(ended2)
	waitFor(t, func() bool { return blocked2.takenCount() >= 1 })
	r.restart()
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	if outcome := finish(t, client, id); outcome.Failure != nil {
		t.Fatalf("the resumed build failed: %+v", outcome.Failure)
	}
	r.stop()
	mu.Lock()
	defer mu.Unlock()
	want := "the job " + string(id) + `: its traceparent "00-not-a-trace" is not a W3C traceparent, and is ignored`
	if n := len(slices.DeleteFunc(slices.Clone(warnings), func(w string) bool { return w != want })); n != 1 {
		t.Errorf("warnings = %q; want %q once", warnings, want)
	}
	if parent := journaledParent(t, state, id); parent != "" {
		t.Errorf("the journal still keeps the traceparent %q; want it repaired as the build resumed", parent)
	}
	// Each resumption ran from the repaired request, its job's span the root of the job's one trace, of an ID of its
	// own: the first's, as its coordinator stopped, and the second's, as the build ended.
	var traces, ids []string
	for nth := 1; nth <= 2; nth++ {
		roots := 0
		for _, s := range e.spans(nth) {
			if s.Name != "build" {
				continue
			}
			traces, ids = append(traces, s.SpanContext.TraceID), append(ids, s.SpanContext.SpanID)
			if s.Parent.SpanID != "0000000000000000" {
				t.Errorf("resumption %d exported a job span under %s; want none under an outer span", nth,
					s.Parent.SpanID)
			}
			roots++
		}
		if roots != 1 {
			t.Errorf("resumption %d exported %d job spans; want 1", nth, roots)
		}
	}
	if len(traces) == 2 && (traces[0] != traces[1] || ids[0] == ids[1]) {
		t.Errorf("the resumptions' job spans are %s/%s and %s/%s; want one trace, and spans of their own", traces[0],
			ids[0], traces[1], ids[1])
	}
}

// TestMalformedTracestateDropped checks that a request with a valid traceparent and a tracestate that isn't one joins
// the outer trace without the state, which is reported.
func TestMalformedTracestateDropped(t *testing.T) {
	e := newExporting(t)
	var mu sync.Mutex
	var warnings []string
	r := newRestartable(t, Config{Telemetry: e.telemetry, Warn: func(err error) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, err.Error())
	}})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	req := testGraph(t, r.fixture, "malformed state")
	req.Parent, req.State = outerTraceparent, "not a state"
	id, status := postSubmission(t, r, "state", api.Request{Graph: &req})
	if status/100 != 2 {
		t.Fatalf("the submission was answered %d; want it accepted", status)
	}
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if outcome := finish(t, client, id); outcome.Failure != nil {
		t.Fatalf("the build failed: %+v", outcome.Failure)
	}
	r.stop()
	mu.Lock()
	want := "the job " + string(id) + `: its tracestate "not a state" is not a W3C tracestate, and is ignored`
	matching := len(slices.DeleteFunc(slices.Clone(warnings), func(w string) bool { return w != want }))
	mu.Unlock()
	if matching != 1 {
		t.Errorf("warnings = %q; want %q once", warnings, want)
	}
	builds := 0
	for _, s := range e.spans(0) {
		if s.Name != "build" {
			continue
		}
		builds++
		if s.SpanContext.TraceID != outerTrace || s.Parent.SpanID != outerSpan || s.SpanContext.TraceState != "" {
			t.Errorf("the job's span is in %s under %s, with tracestate %q; want in %s under %s, with none",
				s.SpanContext.TraceID, s.Parent.SpanID, s.SpanContext.TraceState, outerTrace, outerSpan)
		}
	}
	if builds != 1 {
		t.Errorf("%d job spans were exported; want the build's", builds)
	}
}

// TestMetricsOnlyIgnoresOuterTrace checks that a coordinator that exports metrics alone says nothing of a malformed
// traceparent, which it ignores, as it exports no trace.
func TestMetricsOnlyIgnoresOuterTrace(t *testing.T) {
	var mu sync.Mutex
	var warnings []string
	metricsOnly := func(instance string) (*telemetry.Telemetry, error) {
		tel, err := telemetry.Setup(telemetry.Config{Role: telemetry.Coordinator, Instance: instance,
			Getenv: func(name string) string {
				return map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "console"}[name]
			},
			Console: io.Discard})
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
		return tel, nil
	}
	r := newRestartable(t, Config{Telemetry: metricsOnly, Warn: func(err error) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, err.Error())
	}})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	req := testGraph(t, r.fixture, "metrics only")
	req.Parent = "00-not-a-trace"
	id, status := postSubmission(t, r, "metrics", api.Request{Graph: &req})
	if status/100 != 2 {
		t.Fatalf("the submission was answered %d; want it accepted", status)
	}
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	finish(t, client, id)
	r.stop()
	mu.Lock()
	defer mu.Unlock()
	if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "traceparent") }) {
		t.Errorf("warnings = %q; want none of the traceparent", warnings)
	}
}

// exportingHandler runs attempts as fakeRun does, and a publication's and a push's as their tests' workers do,
// relaying a worker's span for task "one" through w, once w is set.
func exportingHandler(f *fixture, w **fakeWorker) func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
	return func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push != nil {
			switch a.Push.Mode {
			case remote.ModeUpload:
				return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
			case remote.ModeTag:
				return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
			}
		}
		r := a.Request
		if r != nil && r.Action.Kind == sched.KindTask && r.Action.Steps[0].Argv[0] == "one" && *w != nil {
			now := time.Now()
			span := tracing.Span{ID: "w-1", Process: "worker", Track: "t", Name: "worker step", Start: now, End: now}
			if err := (*w).client.Events(context.Background(), a.Attempt,
				remote.AttemptEvents{Spans: []tracing.Span{span}}); err != nil {
				return remote.AttemptReport{Failure: failure(err.Error())}
			}
		}
		return fakeRun(f, nil)(a, killed)
	}
}

// TestExportsEachJob checks that a coordinator exports each job, a build, a push and a publication, as a trace of its
// own under the outer span it joined, with its attempts' spans, a worker's among them, under their attempts' spans.
func TestExportsEachJob(t *testing.T) {
	e := newExporting(t)
	f := newFixture(t, Config{Telemetry: e.telemetry})
	var w *fakeWorker
	w = newWorker(t, f, amd64, 4, exportingHandler(f, &w))
	w.start()
	ctx := t.Context()

	req := testGraph(t, f, "each job")
	req.Parent = outerTraceparent
	built, _ := runJob(t, f.c, req)
	if built.Failure != nil {
		t.Fatalf("the build failed: %+v", built.Failure)
	}
	push := api.PushRequest{Result: built.Results[0], Destination: "example.com/team/app:v1", Parent: outerTraceparent}
	id, err := f.c.Submit(ctx, api.Request{Push: &push})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	publication := aPublication
	publication.Parent = outerTraceparent
	if published := publishJob(t, f.c, publication); published.Failure != nil {
		t.Fatalf("the publication failed: %+v", published.Failure)
	}

	spans := e.spans(0)
	byID := map[string]exportedSpan{}
	for _, s := range spans {
		byID[s.SpanContext.SpanID] = s
	}
	jobs := map[string]string{} // each job's span's ID, by kind
	for _, s := range spans {
		if s.attr("vamos.job") == nil { // not a job's span, as the publication's own "publish" span within it isn't
			continue
		}
		switch s.Name {
		case "build", "push", "publish":
			jobs[s.Name] = s.SpanContext.SpanID
			if s.SpanContext.TraceID != outerTrace || s.Parent.SpanID != outerSpan {
				t.Errorf("the %s job's span is in %s under %s; want in %s under %s", s.Name, s.SpanContext.TraceID,
					s.Parent.SpanID, outerTrace, outerSpan)
			}
		}
	}
	if len(jobs) != 3 {
		t.Fatalf("exported the job spans of %v; want a build's, a push's and a publication's", jobs)
	}
	var step *exportedSpan
	for i, s := range spans {
		if s.Name == "worker step" {
			step = &spans[i]
		}
	}
	if step == nil {
		t.Fatal("the worker's span wasn't exported")
	}
	attempt, ok := byID[step.Parent.SpanID]
	if !ok || attempt.Name != "attempt" || step.SpanContext.TraceID != outerTrace {
		t.Errorf("the worker's span is in %s under %q; want in %s under its attempt's span", step.SpanContext.TraceID,
			attempt.Name, outerTrace)
	}
}

// TestExportsSharedAttemptOnce checks that an attempt two builds share is exported once, in the trace of the build
// that started it, and that the other build's target span links to it, in the other trace.
func TestExportsSharedAttemptOnce(t *testing.T) {
	e := newExporting(t)
	f := newFixture(t, Config{Telemetry: e.telemetry})
	startWorker(t, f, amd64, 4, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		if a.Request.Action.Kind == sched.KindLayer { // until the other build joins it
			waitFor(t, func() bool { return f.c.sharing.AttachedTo(a.Request.Action.Key()) >= 2 })
		}
		return nil
	}))
	ctx := t.Context()
	req := testGraph(t, f, "shared")
	var ids []api.JobID
	for range 2 {
		id, err := f.c.Submit(ctx, api.Request{Graph: &req})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	spans := e.spans(0)
	attempts := map[any]int{}
	byID := map[string]exportedSpan{}
	for _, s := range spans {
		byID[s.SpanContext.SpanID] = s
		if s.Name == "attempt" {
			attempts[s.attr("vamos.attempt")]++
		}
	}
	for id, n := range attempts {
		if n != 1 {
			t.Errorf("the attempt %v was exported %d times; want once", id, n)
		}
	}
	joined := 0
	for _, s := range spans {
		for _, l := range s.Links {
			if a, ok := byID[l.SpanContext.SpanID]; ok && a.Name == "attempt" &&
				l.SpanContext.TraceID == a.SpanContext.TraceID && l.SpanContext.TraceID != s.SpanContext.TraceID {
				joined++
			}
		}
	}
	if joined == 0 {
		t.Error("no span links to an attempt in another trace; want the joining build's target span to")
	}
}

// TestPoolShutdownCancelsACancelledJob checks that an attempt of a job that Close cancelled, which reaches the pool
// once it has closed, ends as the job's cancellation, not as the pool's shutdown, which would fail the job.
func TestPoolShutdownCancelsACancelledJob(t *testing.T) {
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.close()
	_, err := p.attempt(ctx, nil, &remote.PushWork{Mode: remote.ModePush}, sched.OutputFunc(func(byte, []byte) {}))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the attempt of a cancelled job, at a closed pool, = %v; want %v", err, context.Canceled)
	}
	if _, err := p.attempt(t.Context(), nil, &remote.PushWork{Mode: remote.ModePush},
		sched.OutputFunc(func(byte, []byte) {})); !errors.Is(err, errPoolClosed) {
		t.Errorf("the attempt of a live job, at a closed pool, = %v; want %v", err, errPoolClosed)
	}
}
