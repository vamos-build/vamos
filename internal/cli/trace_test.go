package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
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

// answeringClock is an executor that says what time it is, unless the call's context has ended.
type answeringClock struct{ api.Executor }

func (answeringClock) Capabilities(ctx context.Context) (api.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	return api.Capabilities{Clock: time.Now()}, nil
}
