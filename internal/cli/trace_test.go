package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// TestFinishTrace checks which error a traced command returns when its trace can't be written:
// that failure if the command succeeded, or else the command's own, with the trace's failure only warned about.
func TestFinishTrace(t *testing.T) {
	failed := errors.New("the build failed")
	for _, tt := range []struct {
		name    string
		err     error
		want    string
		warning string
	}{
		{
			name: "command succeeded",
			err:  nil,
			want: "writing trace t.json: ",
		},
		{
			name:    "command failed",
			err:     failed,
			want:    failed.Error(),
			warning: "vamos: warning: writing trace t.json: ",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The folder is gone by the time the trace is written.
			session := &traceSession{
				path:     filepath.Join(t.TempDir(), "gone", "t.json"),
				recorder: tracing.NewRecorder(clientProcess, nil),
			}
			var stderr strings.Builder
			opts := defaultOptions()
			opts.trace = "t.json"
			inv := &invocation{ctx: t.Context(), stderr: &stderr, opts: opts, session: session}
			err := inv.finishTrace(tt.err)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if got := stderr.String(); tt.warning == "" && got != "" || !strings.HasPrefix(got, tt.warning) {
				t.Errorf("stderr = %q, want %q", got, tt.warning)
			}
		})
	}
}

// assembly is the fixture of an assembly test: a memory store holding traces, which put adds to.
type assembly struct {
	t   *testing.T
	m   *cache.Memory
	now time.Time
}

func newAssembly(t *testing.T) *assembly {
	return &assembly{t: t, m: cache.NewMemory(), now: time.Now()}
}

// span returns a span of the executor, named name, on track, from ms to end milliseconds after the fixture's now.
func (a *assembly) span(id, track, name string, ms, end int, args ...any) tracing.Span {
	s := tracing.Span{ID: id, Process: "executor", Track: track, Name: name,
		Start: a.now.Add(time.Duration(ms) * time.Millisecond), End: a.now.Add(time.Duration(end) * time.Millisecond)}
	if len(args) > 0 {
		s.Args = map[string]any{}
		for i := 0; i+1 < len(args); i += 2 {
			s.Args[args[i].(string)] = args[i+1]
		}
	}
	return s
}

// attempt stores the trace of the attempt id, holding spans, and returns its blob.
func (a *assembly) attempt(id string, spans ...tracing.Span) cache.Blob {
	data, err := tracing.EncodeAttempt(id, spans, tracing.Lost{})
	must(a.t, err)
	b, err := a.m.PutBytes(a.t.Context(), data)
	must(a.t, err)
	return b
}

// job stores a job's manifest of spans and entries, and returns the command's job naming it.
func (a *assembly) job(id api.JobID, spans []tracing.Span, entries ...api.TraceEntry) api.CommandJob {
	data, err := api.JobTrace{Spans: spans, Entries: entries}.Encode(tracing.Lost{})
	must(a.t, err)
	b, err := a.m.PutBytes(a.t.Context(), data)
	must(a.t, err)
	return api.CommandJob{ID: id, Manifest: &b}
}

// placed returns, for each span assembled by name, its track, failing the test if a name appears twice.
func placed(t *testing.T, spans []tracing.Span) map[string]tracing.Span {
	t.Helper()
	byName := map[string]tracing.Span{}
	for _, s := range spans {
		if _, ok := byName[s.Name]; ok && s.Name != "trace incomplete" {
			t.Errorf("%q is placed twice", s.Name)
		}
		byName[s.Name] = s
	}
	return byName
}

// TestAssembleSharedAttempt checks that an attempt one job of a command started and another joined is placed once:
// under the starter's target if the starter took its trace, with its own cache write alone, and otherwise on the
// target's shared track of the first job that took it; and that one no job took says the job left it running.
func TestAssembleSharedAttempt(t *testing.T) {
	a := newAssembly(t)
	const app = `layer "app"`
	step := a.span("s1", app, `run(["make"])`, 10, 50)
	trace := a.attempt("a1", step)
	write := func(id string) tracing.Span {
		return a.span(id, app, "cache write", 51, 52, "attempt", "a1")
	}
	target := a.span("t1", app, app, 0, 60)
	joinedWrite := write("w2")
	joinedWrite.Track = app + " (shared)"
	starter := api.TraceEntry{Node: 1, Desc: app, Outcome: api.NodeMiss, Started: true, Attempt: "a1", Trace: &trace}
	joiner := api.TraceEntry{Node: 1, Desc: app, Outcome: api.NodeJoined, Attempt: "a1", Trace: &trace}

	doc := api.CommandTrace{Jobs: []api.CommandJob{
		a.job("j1", []tracing.Span{target, write("w1")}, starter),
		a.job("j2", []tracing.Span{a.span("t2", app, "joined "+app, 30, 60), joinedWrite}, joiner),
	}}
	got := placed(t, assemble(t.Context(), a.m, doc, tracing.Lost{}))
	if s := got[`run(["make"])`]; s.Track != app {
		t.Errorf("the attempt's step lies on %q; want the starter's target track, %q", s.Track, app)
	}
	var writes int
	for _, s := range assemble(t.Context(), a.m, doc, tracing.Lost{}) {
		if s.Name == "cache write" {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("%d cache writes are placed; want the starter's alone", writes)
	}

	// The starter left the attempt running; the job that joined it placed it on its shared track.
	left := starter
	left.Trace, left.Left = nil, true
	doc = api.CommandTrace{Jobs: []api.CommandJob{
		a.job("j1", []tracing.Span{target}, left),
		a.job("j2", []tracing.Span{a.span("t2", app, "joined "+app, 30, 60), joinedWrite}, joiner),
	}}
	got = placed(t, assemble(t.Context(), a.m, doc, tracing.Lost{}))
	if s := got[`run(["make"])`]; s.Track != app+" (shared)" || s.Parent != "" {
		t.Errorf("the attempt's step lies on %q, part of %q; want the shared track, part of nothing", s.Track, s.Parent)
	}

	// No job took the attempt's trace: one left it running, or its trace couldn't be stored.
	for why, entry := range map[string]api.TraceEntry{
		"the job left the attempt running": left,
		"the attempt's trace couldn't be stored": {Node: 1, Desc: app, Outcome: api.NodeMiss, Started: true,
			Attempt: "a1"},
	} {
		doc = api.CommandTrace{Jobs: []api.CommandJob{a.job("j1", []tracing.Span{target}, entry)}}
		got = placed(t, assemble(t.Context(), a.m, doc, tracing.Lost{}))
		if s := got["trace incomplete"]; s.Track != app || s.Args["reason"] != why || !s.Start.Equal(target.Start) {
			t.Errorf("the gap is %+v; want an instant on %q at the trace's start saying %s", s, app, why)
		}
	}
}

// TestAssembleTwoNodesOneAttempt checks that an attempt two nodes of one job share is placed once, under the
// starter's target, though each node's entry names it.
func TestAssembleTwoNodesOneAttempt(t *testing.T) {
	a := newAssembly(t)
	step := a.span("s1", `layer "app" (linux/amd64)`, `run(["make"])`, 10, 50)
	trace := a.attempt("a1", step)
	doc := api.CommandTrace{Jobs: []api.CommandJob{a.job("j1", nil,
		api.TraceEntry{Node: 1, Outcome: api.NodeMiss, Started: true, Cause: "new", Attempt: "a1", Trace: &trace},
		api.TraceEntry{Node: 2, Outcome: api.NodeJoined, Cause: "new", Attempt: "a1", Trace: &trace},
	)}}
	got := placed(t, assemble(t.Context(), a.m, doc, tracing.Lost{}))
	if s := got[`run(["make"])`]; s.Track != `layer "app" (linux/amd64)` {
		t.Errorf("the step lies on %q; want the starter's track", s.Track)
	}
}

// TestAssembleGaps checks that what can't be read, in a batch with what can, costs only its own spans, each an
// instant saying so where it would have been: a manifest gone, an attempt trace gone or corrupt, a job with no
// manifest, and a clock offset not measured.
func TestAssembleGaps(t *testing.T) {
	a := newAssembly(t)
	const app, lib = `layer "app"`, `layer "lib"`
	present := a.attempt("a1", a.span("s1", app, "kept step", 10, 20))
	gone := a.attempt("a2", a.span("s2", lib, "gone step", 10, 20))
	corrupt := a.attempt("a3", a.span("s3", lib, "corrupt step", 10, 20))
	a.m.Replace(corrupt, []byte("not the trace"))
	doc := api.CommandTrace{OffsetUnknown: true, Jobs: []api.CommandJob{
		a.job("j1", []tracing.Span{a.span("t1", app, app, 0, 30)},
			api.TraceEntry{Node: 1, Desc: app, Outcome: api.NodeMiss, Started: true, Attempt: "a1", Trace: &present},
			api.TraceEntry{Node: 2, Desc: lib, Outcome: api.NodeMiss, Started: true, Attempt: "a2", Trace: &gone},
			api.TraceEntry{Node: 3, Desc: lib, Outcome: api.NodeMiss, Started: true, Attempt: "a3", Trace: &corrupt}),
		{ID: "j2"},
		{ID: "j3", Manifest: &cache.Blob{Digest: digest.FromString("gone manifest"), Size: 9}},
	}}
	a.m.Replace(gone, nil)
	spans := assemble(t.Context(), a.m, doc, tracing.Lost{})
	reasons := map[string]bool{}
	var kept bool
	for _, s := range spans {
		if s.Name == "trace incomplete" {
			reasons[s.Args["reason"].(string)+" on "+s.Track] = true
		}
		kept = kept || s.Name == "kept step"
	}
	if !kept {
		t.Error("the attempt trace that can be read isn't placed")
	}
	for _, want := range []string{
		"the attempt's trace is gone from the build cache on " + lib,
		"the executor stored no trace of a job on vamos",
		"a job's trace can't be read on vamos",
		"the executor's clock couldn't be measured, so this process's spans may be misplaced on vamos",
	} {
		if !reasons[want] {
			t.Errorf("the trace doesn't say %q; it says %v", want, reasons)
		}
	}
	var buf bytes.Buffer
	if err := tracing.WriteChrome(&buf, spans); err != nil {
		t.Errorf("the assembled trace can't be written: %v", err)
	}
}

// TestMeasureLocalClock checks that the local executor's clock, which is this process's, is at no offset, exactly,
// rather than at one measured, which a sample's round trip makes uncertain.
func TestMeasureLocalClock(t *testing.T) {
	local, err := run.NewLocal(testenv.PrivateStores(t).Options)
	must(t, err)
	t.Cleanup(func() { local.Close() })
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil), path: "trace.json"}
	session.measure(t.Context(), local, func(err error) { t.Errorf("warned: %v", err) })
	doc, _ := session.document()
	if doc.OffsetUnknown || doc.Offset != (tracing.Offset{}) {
		t.Errorf("the offset is %+v, unknown %t; want none, exactly", doc.Offset, doc.OffsetUnknown)
	}
}

// TestMeasureClockBoundedAfterLateFailure checks that a measurement taken again after a failure stays within the
// first's bound, so that a failure before it, and then a stall, cost no more than the bound.
func TestMeasureClockBoundedAfterLateFailure(t *testing.T) {
	old := clockTimeout
	clockTimeout = 500 * time.Millisecond
	t.Cleanup(func() { clockTimeout = old })
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
	clock := &resetThenStallClock{t: t, after: 50 * time.Millisecond}
	session.measure(t.Context(), clock, func(error) {})
	if len(clock.deadlines) != 2 {
		t.Fatalf("the clock was asked %d times, want twice: once more after the failure", len(clock.deadlines))
	}
	if first, second := clock.deadlines[0], clock.deadlines[1]; second.After(first.Add(5 * time.Millisecond)) {
		t.Errorf("the second measurement's deadline is %v after the first's; want it within the first's bound",
			second.Sub(first))
	}
	if doc, _ := session.document(); !doc.OffsetUnknown {
		t.Error("the offset is known; want it unknown, the executor having never answered")
	}
}

// resetThenStallClock is an executor whose first answer about the time fails after after, and whose later ones
// never come, until the call's context ends. It keeps each call's deadline.
type resetThenStallClock struct {
	api.Executor
	t         *testing.T
	after     time.Duration
	deadlines []time.Time
}

func (c *resetThenStallClock) Capabilities(ctx context.Context) (api.Capabilities, error) {
	deadline, ok := ctx.Deadline()
	if !ok { // which a stall would never end
		c.t.Error("the clock was asked for the time with no deadline")
		return api.Capabilities{}, errors.New("asked for the time with no deadline")
	}
	c.deadlines = append(c.deadlines, deadline)
	if len(c.deadlines) == 1 {
		time.Sleep(c.after)
		return api.Capabilities{}, syscall.ECONNRESET
	}
	return stalledClock{}.Capabilities(ctx)
}

// TestMeasureClockFailingTwice checks that a measurement that fails at once, and fails again, otherwise, warns of
// both causes, in one warning.
func TestMeasureClockFailingTwice(t *testing.T) {
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
	causes := []error{syscall.ECONNRESET, syscall.ECONNREFUSED}
	clock := failingClock{causes: &causes}
	var warned []error
	session.measure(t.Context(), clock, func(err error) { warned = append(warned, err) })
	if len(warned) != 1 || !errors.Is(warned[0], syscall.ECONNRESET) || !errors.Is(warned[0], syscall.ECONNREFUSED) ||
		strings.Contains(warned[0].Error(), "\n") {
		t.Errorf("warned %q; want one warning, on one line, of both the reset and the refusal", warned)
	}
}

// failingClock is an executor whose answers about the time fail, each with the next of causes.
type failingClock struct {
	api.Executor
	causes *[]error
}

func (c failingClock) Capabilities(context.Context) (api.Capabilities, error) {
	err := (*c.causes)[0]
	*c.causes = (*c.causes)[1:]
	return api.Capabilities{}, err
}

// stalledClock is an executor that never answers what time it is, until the call's context ends.
type stalledClock struct{ api.Executor }

func (stalledClock) Capabilities(ctx context.Context) (api.Capabilities, error) {
	<-ctx.Done()
	return api.Capabilities{}, ctx.Err()
}

// TestMeasureClockBounded checks that measuring the clock of an executor that stops answering ends at its bound, and
// leaves the command's trace saying the offset is unknown, with a warning.
func TestMeasureClockBounded(t *testing.T) {
	old := clockTimeout
	clockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { clockTimeout = old })
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
	var warned []error
	start := time.Now()
	session.addJob("j1", api.Outcome{}, func(err error) { warned = append(warned, err) })
	session.measure(t.Context(), stalledClock{}, func(err error) { warned = append(warned, err) })
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("measuring a stalled clock took %v; want it bounded", took)
	}
	if doc, _ := session.document(); !doc.OffsetUnknown || len(doc.Jobs) != 1 {
		t.Errorf("the trace's offset unknown = %v, jobs %v; want it unknown, and the job", doc.OffsetUnknown, doc.Jobs)
	}
	if len(warned) != 2 { // the job named no manifest, and the clock wasn't measured
		t.Errorf("warned %v; want the missing manifest and the clock", warned)
	}
}

// TestMeasureClockAfterInterrupt checks that a command interrupted as its job ends still measures the executor's
// clock, which the interrupt doesn't end.
func TestMeasureClockAfterInterrupt(t *testing.T) {
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := cache.Blob{Digest: digest.FromString("manifest"), Size: 1}
	session.addJob("j1", api.Outcome{Trace: &b}, func(err error) {
		t.Errorf("warned %v; want the job recorded", err)
	})
	session.measure(ctx, answeringClock{}, func(err error) {
		t.Errorf("warned %v; want the clock measured", err)
	})
	if doc, _ := session.document(); doc.OffsetUnknown {
		t.Error("the offset is unknown, as if the interrupt ended its measurement")
	}
}

// TestMeasureClockAfterQuickFailure checks that a clock measurement that fails before its bound, as on a reused
// connection that was reset, is taken again, timed afresh, so that the failure neither leaves the offset unknown nor
// skews it by the time the failure took.
func TestMeasureClockAfterQuickFailure(t *testing.T) {
	const ahead = 10 * time.Second
	session := &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}
	session.measure(t.Context(), &resetOnceClock{ahead: ahead, after: 200 * time.Millisecond}, func(err error) {
		t.Errorf("warned %v; want the clock measured", err)
	})
	doc, _ := session.document()
	if doc.OffsetUnknown {
		t.Fatal("the offset is unknown, as if one quick failure ended its measurement")
	}
	if skew := doc.Offset.Offset - ahead; skew < -90*time.Millisecond || skew > 90*time.Millisecond {
		t.Errorf("the offset is %v, %v off the executor's %v; want the failed attempt's time left out",
			doc.Offset.Offset, skew, ahead)
	}
}

// resetOnceClock is an executor whose clock is ahead by ahead, and whose first answer about it fails after after, as
// a connection reset does.
type resetOnceClock struct {
	api.Executor
	ahead, after time.Duration
	reset        bool
}

func (c *resetOnceClock) Capabilities(ctx context.Context) (api.Capabilities, error) {
	if !c.reset {
		c.reset = true
		time.Sleep(c.after)
		return api.Capabilities{}, syscall.ECONNRESET
	}
	return api.Capabilities{Clock: time.Now().Add(c.ahead)}, nil
}

// answeringClock is an executor that says what time it is, unless the call's context has ended.
type answeringClock struct{ api.Executor }

func (answeringClock) Capabilities(ctx context.Context) (api.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	return api.Capabilities{Clock: time.Now()}, nil
}

// TestAssembleNamesExportedTrace checks that a job's top-level spans name the OpenTelemetry trace the job was exported
// in, and whether it was sampled, when its manifest says it was, and that spans within them, the spans of a job not
// exported, and those of a manifest stored before Vamos exported, name none.
func TestAssembleNamesExportedTrace(t *testing.T) {
	a := newAssembly(t)
	top := a.span("t1", "main", "build", 0, 60)
	top.Parent = "client-1" // the client's span, which the job's top-level span is part of
	inner := a.span("t2", "main", "load", 10, 20)
	inner.Parent = "t1"
	exported := a.job("j1", []tracing.Span{top, inner})
	data, err := api.JobTrace{Spans: []tracing.Span{top, inner},
		Exported: tracing.Exported{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
			Sampled: true}}.Encode(tracing.Lost{})
	must(t, err)
	b, err := a.m.PutBytes(t.Context(), data)
	must(t, err)
	exported.Manifest = &b

	got := placed(t, assemble(t.Context(), a.m, api.CommandTrace{Jobs: []api.CommandJob{exported}}, tracing.Lost{}))
	if s := got["build"]; s.Args["otel trace"] != "4bf92f3577b34da6a3ce929d0e0e4736" || s.Args["otel sampled"] != true {
		t.Errorf("the job's top-level span says %v; want its OpenTelemetry trace, sampled", s.Args)
	}
	if s := got["load"]; s.Args["otel trace"] != nil {
		t.Errorf("a span within the job's top-level one says %v; want no OpenTelemetry trace", s.Args)
	}

	// Another job's cache write, on the shared track, isn't the job's, and an unsampled trace says so.
	joinedWrite := a.span("w2", `layer "app" (shared)`, "cache write", 30, 31, "attempt", "a9", tracing.Shared, true)
	data, err = api.JobTrace{Spans: []tracing.Span{top, joinedWrite},
		Exported: tracing.Exported{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7"}}.Encode(
		tracing.Lost{})
	must(t, err)
	b, err = a.m.PutBytes(t.Context(), data)
	must(t, err)
	got = placed(t, assemble(t.Context(), a.m, api.CommandTrace{Jobs: []api.CommandJob{{ID: "j4", Manifest: &b}}},
		tracing.Lost{}))
	if s := got["build"]; s.Args["otel sampled"] != false {
		t.Errorf("the unsampled job's top-level span says %v; want otel sampled false", s.Args)
	}
	if s, ok := got["cache write"]; !ok || s.Args["otel trace"] != nil || s.Args["otel sampled"] != nil ||
		s.Args[tracing.Shared] != nil {
		t.Errorf("another job's cache write on the shared track says %v; want no OpenTelemetry trace", s.Args)
	}

	older := fmt.Sprintf(`{"version":%d,"kind":%q,"spans":[{"id":"t1","process":"executor","track":"main",`+
		`"name":"build","start":%q,"end":%q}],"entries":[]}`, tracing.Version, api.KindJob,
		top.Start.Format(time.RFC3339Nano), top.End.Format(time.RFC3339Nano))
	b, err = a.m.PutBytes(t.Context(), []byte(older))
	must(t, err)
	data, err = api.JobTrace{Spans: []tracing.Span{top},
		Exported: tracing.Exported{TraceID: strings.Repeat("Z", 1<<10), Sampled: true}}.Encode(tracing.Lost{})
	must(t, err)
	malformed, err := a.m.PutBytes(t.Context(), data)
	must(t, err)
	for name, job := range map[string]api.CommandJob{
		"a job not exported":                      a.job("j2", []tracing.Span{top}),
		"a manifest stored before vamos exported": {ID: "j3", Manifest: &b},
		"a manifest whose trace ID isn't one":     {ID: "j5", Manifest: &malformed},
	} {
		got := placed(t, assemble(t.Context(), a.m, api.CommandTrace{Jobs: []api.CommandJob{job}}, tracing.Lost{}))
		if s, ok := got["build"]; !ok || s.Args["otel trace"] != nil || s.Args["otel sampled"] != nil {
			t.Errorf("%s: the top-level span says %v; want no OpenTelemetry trace", name, s.Args)
		}
	}
}
