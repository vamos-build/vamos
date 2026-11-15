package sched

import (
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"
)

// traceGraph returns a graph that pulls a base, and builds a task on it and a layer that adds the task's files:
// nodes 0, 1 and 2.
func traceGraph(t *testing.T) *graph.Graph {
	t.Helper()
	from := graph.Ref(1)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{2}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "gen", Desc: `task "gen"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["gen"])`, Argv: []string{"gen"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("gen", "/data")`, From: &from, Dest: "/data"},
		}}},
	}}
	testenv.Must(t, g.Validate())
	return g
}

// TestSchedulerManifest checks that a job's manifest says what became of each node: a cold build's resolve and
// targets ran in attempts it started, each naming its attempt's trace; a cached build's targets were hits naming
// the trace of the attempt that made their result, whose spans say when that was, how long it took and what reusing
// it saved; and that a result whose trace is gone is reused all the same, naming it still.
func TestSchedulerManifest(t *testing.T) {
	m, f := fakeFixture(t)
	g := traceGraph(t)
	run := func() api.JobTrace {
		t.Helper()
		ctx, trace := ForJob(t.Context())
		if _, err := schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(ctx); err != nil {
			t.Fatal(err)
		}
		b, err := trace.Save(ctx, m)
		testenv.Must(t, err)
		data, err := m.Read(t.Context(), *b, tracing.MaxTraceSize)
		testenv.Must(t, err)
		manifest, err := api.DecodeJobTrace(data)
		testenv.Must(t, err)
		return manifest
	}
	byNode := func(manifest api.JobTrace) map[graph.Ref]api.TraceEntry {
		entries := map[graph.Ref]api.TraceEntry{}
		for _, e := range manifest.Entries {
			entries[e.Node] = e
		}
		return entries
	}

	cold := byNode(run())
	for ref := range graph.Ref(3) {
		e, ok := cold[ref]
		if !ok || e.Outcome != api.NodeMiss || !e.Started || e.Attempt == "" || e.Trace == nil {
			t.Errorf("node %d's entry on a cold build = %+v; want a miss in an attempt it started, naming its trace",
				ref, e)
		}
	}

	warm := run()
	entries := byNode(warm)
	for _, ref := range []graph.Ref{1, 2} {
		e := entries[ref]
		if e.Outcome != api.NodeHit || e.Trace == nil || *e.Trace != *cold[ref].Trace {
			t.Errorf("node %d's entry on a cached build = %+v; want a hit naming the trace that made it, %v",
				ref, e, cold[ref].Trace)
		}
	}
	if e := entries[0]; e.Outcome != api.NodeHit {
		t.Errorf("the pull's entry on a cached build = %+v; want a hit", e)
	}
	var app *tracing.Span
	for i, s := range warm.Spans {
		if s.Name == `layer "app"` {
			app = &warm.Spans[i]
		}
	}
	if app == nil {
		t.Fatal("the cached build's manifest has no span of the layer")
	}
	for _, arg := range []string{"made", "took", "saved", "attempt trace"} {
		if app.Args[arg] == nil {
			t.Errorf("the hit's span says %v; want %q among them", app.Args, arg)
		}
	}
	if app.Args["attempt trace"] != cold[2].Trace.Digest.String() {
		t.Errorf("the hit's span names the attempt trace %v; want %s", app.Args["attempt trace"], cold[2].Trace.Digest)
	}

	// A result whose trace is gone is reused: nothing checks it.
	m.Replace(*cold[2].Trace, []byte("gone"))
	runs := len(f.runs)
	entries = byNode(run())
	if len(f.runs) != runs || entries[2].Outcome != api.NodeHit {
		t.Errorf("with its trace gone, the layer ran again (%d runs, %d before), or wasn't a hit: %+v",
			len(f.runs), runs, entries[2])
	}
}
