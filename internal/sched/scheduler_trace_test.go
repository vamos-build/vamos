package sched

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
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
		b, _, err := trace.Save(ctx, m)
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

// targetSpans runs g once, as a job, and returns the spans of its targets, by name, as its manifest holds them.
func targetSpans(t *testing.T, g *graph.Graph, b Backend, bound int) (map[string]tracing.Span, error) {
	t.Helper()
	ctx, trace := ForJob(t.Context())
	_, runErr := New(g, b, &recordedEvents{}, Options{Bound: bound}).Run(ctx)
	_, spans, err := trace.Save(ctx, b.Content)
	testenv.Must(t, err)
	byName := map[string]tracing.Span{}
	for _, s := range spans {
		if s.Args[tracing.Role] == tracing.RoleTarget {
			byName[s.Name] = s
		}
	}
	return byName, runErr
}

// wantTarget checks that the span of the target name says it is a target, of kind, configured as target, ended as
// outcome says, served by an attempt if attempt, and lost for lost, if it isn't "".
func wantTarget(t *testing.T, spans map[string]tracing.Span, name, kind, target, outcome string, attempt bool,
	lost string) {
	t.Helper()
	s, ok := spans[name]
	if !ok {
		t.Errorf("no target span is named %s", name)
		return
	}
	id, _ := s.Args["attempt"].(string)
	if s.Args["kind"] != kind || s.Args["target"] != target || s.Args["outcome"] != outcome || (id != "") != attempt ||
		s.Args["lost"] != map[bool]any{true: lost, false: nil}[lost != ""] {
		t.Errorf("%s says %v; want kind %s, target %s, outcome %s, an attempt %v, lost %q", name, s.Args, kind,
			target, outcome, attempt, lost)
	}
}

// TestSchedulerTargetSpans checks that the span of each pull, layer and task says it is a target, of what kind and
// configured target, and how it ended: a miss and the attempt that served it, a hit, a result lost and why, and a
// failure.
func TestSchedulerTargetSpans(t *testing.T) {
	m, f := fakeFixture(t)
	g := traceGraph(t)
	for i, target := range []string{"//:base", "//:gen", "//:app"} {
		g.Nodes[i].Target = target
	}
	b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(f, m, m)}

	cold, err := targetSpans(t, g, b, 1)
	testenv.Must(t, err)
	wantTarget(t, cold, `pull "base"`, kindPull, "//:base", api.NodeMiss, true, "")
	wantTarget(t, cold, `task "gen"`, KindTask, "//:gen", api.NodeMiss, true, "")
	wantTarget(t, cold, `layer "app"`, KindLayer, "//:app", api.NodeMiss, true, "")

	warm, err := targetSpans(t, g, b, 1)
	testenv.Must(t, err)
	wantTarget(t, warm, `pull "base"`, kindPull, "//:base", api.NodeHit, false, "")
	wantTarget(t, warm, `task "gen"`, KindTask, "//:gen", api.NodeHit, false, "")
	wantTarget(t, warm, `layer "app"`, KindLayer, "//:app", api.NodeHit, false, "")

	clear(f.images) // the pull's and the layer's results are unusable now
	lost, err := targetSpans(t, g, b, 1)
	testenv.Must(t, err)
	wantTarget(t, lost, `pull "base"`, kindPull, "//:base", api.NodeMiss, true, lostUnusable)
	wantTarget(t, lost, `layer "app"`, KindLayer, "//:app", api.NodeMiss, true, lostUnusable)

	// The task's tree is corrupt, then missing.
	gen, err := m.Result(t.Context(), f.runs[0]) // the task ran first, on the cold build
	testenv.Must(t, err)
	m.Replace(*gen.Output, []byte("not a tree"))
	corrupt, err := targetSpans(t, g, b, 1)
	testenv.Must(t, err)
	wantTarget(t, corrupt, `task "gen"`, KindTask, "//:gen", api.NodeMiss, true, lostCorrupt)
	_, err = m.ClearContent(t.Context())
	testenv.Must(t, err)
	missing, err := targetSpans(t, g, b, 1)
	testenv.Must(t, err)
	wantTarget(t, missing, `task "gen"`, KindTask, "//:gen", api.NodeMiss, true, lostMissing)

	f.fail = func(req Request) error {
		if req.Action.Kind == KindLayer {
			return errors.New("make failed")
		}
		return nil
	}
	b.Runner = NewSharing(f, m, m)
	clear(f.images)
	failed, err := targetSpans(t, g, b, 1)
	if err == nil {
		t.Fatal("the build whose layer fails succeeded")
	}
	wantTarget(t, failed, `layer "app"`, KindLayer, "//:app", outcomeFailed, true, lostUnusable)

	// A timeout of the target's own, while its job goes on, is a failure.
	f.fail = func(req Request) error {
		if req.Action.Kind == KindLayer {
			return fmt.Errorf("checking the registry: %w", context.DeadlineExceeded)
		}
		return nil
	}
	b.Runner = NewSharing(f, m, m)
	clear(f.images)
	timedOut, err := targetSpans(t, g, b, 1)
	if err == nil {
		t.Fatal("the build whose layer timed out succeeded")
	}
	wantTarget(t, timedOut, `layer "app"`, KindLayer, "//:app", outcomeFailed, true, lostUnusable)

	// A job cancelled while its layer runs is cancelled, whatever error the step fails with: the Runner, which no
	// Sharing wraps, so that the step's own error reaches the Scheduler, fails it with "signal: killed".
	ctx, cancel := context.WithCancel(t.Context())
	f.fail = func(req Request) error {
		if req.Action.Kind == KindLayer {
			cancel()
			return errors.New("signal: killed")
		}
		return nil
	}
	b.Runner = f
	clear(f.images)
	jctx, trace := ForJob(ctx)
	if _, err := New(g, b, &recordedEvents{}, Options{Bound: 1}).Run(jctx); err == nil {
		t.Fatal("the cancelled build succeeded")
	}
	_, spans, err := trace.Save(jctx, m)
	testenv.Must(t, err)
	cancelled := map[string]tracing.Span{}
	for _, s := range spans {
		if s.Args[tracing.Role] == tracing.RoleTarget {
			cancelled[s.Name] = s
		}
	}
	wantTarget(t, cancelled, `layer "app"`, KindLayer, "//:app", outcomeCancelled, false, lostUnusable)
}

// TestSchedulerJoinedTargetSpan checks that a target that joined an attempt another target started says so, naming
// that attempt, and is marked joined, which outlasts its outcome.
func TestSchedulerJoinedTargetSpan(t *testing.T) {
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1, 2}}
	step := []graph.Step{{Op: graph.OpRun, Display: `run(["make"])`, Argv: []string{"make"}}}
	g.Nodes = []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "a", Desc: `layer "a"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: step}},
		{Label: "b", Desc: `layer "b"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: step}},
	}
	testenv.Must(t, g.Validate())
	m, f := fakeFixture(t)
	gate := &attachGate{fakeRunner: f}
	gate.sharing = NewSharing(gate, m, m)
	spans, err := targetSpans(t, g, Backend{Content: m, Records: m, Checker: f, Runner: gate.sharing}, 2)
	testenv.Must(t, err)
	a, b := spans[`layer "a"`], spans[`layer "b"`]
	outcomes := []any{a.Args["outcome"], b.Args["outcome"]}
	if !slices.Contains(outcomes, api.NodeMiss) || !slices.Contains(outcomes, api.NodeJoined) ||
		a.Args["attempt"] == nil || a.Args["attempt"] != b.Args["attempt"] {
		t.Errorf("the layers say %v and %v; want one a miss, the other joined, both naming its attempt", a.Args,
			b.Args)
	}
	for _, span := range []tracing.Span{a, b} {
		if joined := span.Args["outcome"] == api.NodeJoined; (span.Args["joined"] == true) != joined {
			t.Errorf("%s says %v; want it marked joined only if it joined", span.Name, span.Args)
		}
	}
}

// TestOnSharedTrackMarksCopies checks that spans moved to a target's shared track are marked as copies, without
// changing the spans they were copied from.
func TestOnSharedTrackMarksCopies(t *testing.T) {
	spans := []tracing.Span{{ID: "w", Parent: "t", Track: "main", Name: "cache write",
		Args: map[string]any{"attempt": "a"}}}
	moved := onSharedTrack(spans, `layer "app"`)
	if s := moved[0]; s.Args[tracing.Shared] != true || s.Args["attempt"] != "a" || s.Parent != "" ||
		s.Track != sharedTrack(`layer "app"`) {
		t.Errorf("the moved span is %+v; want it on the shared track, part of nothing, marked shared", s)
	}
	if spans[0].Args[tracing.Shared] != nil {
		t.Error("marking the copy changed the span it was copied from")
	}
}

// TestSchedulerPullOutcomeWithoutSharing checks that a pull that a Runner without Sharing resolves, which says
// nothing of whose attempt ran it, is a miss.
func TestSchedulerPullOutcomeWithoutSharing(t *testing.T) {
	m, f := fakeFixture(t)
	spans, err := targetSpans(t, traceGraph(t), Backend{Content: m, Records: m, Checker: f, Runner: f}, 1)
	testenv.Must(t, err)
	if s := spans[`pull "base"`]; s.Args["outcome"] != api.NodeMiss {
		t.Errorf("the pull says %v; want a miss", s.Args)
	}
}

// TestSchedulerForcedRegenerationIsLost checks that a task that --rebuild forced, whose files a later target then
// finds corrupt, is run again marked lost, corrupt, though it was forced.
func TestSchedulerForcedRegenerationIsLost(t *testing.T) {
	m, f := fakeFixture(t)
	g := traceGraph(t)
	g.Roots = []graph.Ref{1, 2} // so that --rebuild forces the task too
	corrupted := false
	f.fail = func(req Request) error {
		if req.Action.Kind != KindLayer || corrupted {
			return nil
		}
		corrupted = true
		files := f.files[f.runs[0]] // the task's, which ran first
		return &StepError{Step: 0, Err: fmt.Errorf("copying sources from the build cache: f: %w",
			cache.Corrupt(files[0], "it was changed"))}
	}
	ctx, trace := ForJob(t.Context())
	b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(f, m, m)}
	_, err := New(g, b, &recordedEvents{}, Options{Rebuild: true, Bound: 1}).Run(ctx)
	testenv.Must(t, err)
	_, spans, err := trace.Save(ctx, m)
	testenv.Must(t, err)
	var lost int
	for _, s := range spans {
		if s.Name == `task "gen"` && s.Args["lost"] == lostCorrupt {
			lost++
			if s.Args["miss"] != CauseLost {
				t.Errorf("the regenerated task's miss is attributed to %v; want %s, as its span says", s.Args["miss"],
					CauseLost)
			}
		}
	}
	if !corrupted || lost != 1 {
		t.Errorf("the task's spans were marked lost %d times, the layer failing on its files %v; want once",
			lost, corrupted)
	}
}

// gatedLayers is a Runner whose layers fail as fail says, run apart from the fake, which keeps unlocked books, and
// whose other actions the fake runs one at a time.
type gatedLayers struct {
	*fakeRunner
	mu   sync.Mutex
	fail func(req Request) error
}

func (g *gatedLayers) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	if req.Action.Kind == KindLayer {
		return nil, g.fail(req)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeRunner.Run(ctx, req, ev)
}

// TestSchedulerNotRetriedAfterAFailure checks that a target that finds an input corrupt once another target has
// failed the job, which starts nothing more, is failed rather than retried.
func TestSchedulerNotRetriedAfterAFailure(t *testing.T) {
	m, f := fakeFixture(t)
	g := traceGraph(t) // the layer adds the task's files
	makeStep := []graph.Step{{Op: graph.OpRun, Display: `run(["make"])`, Argv: []string{"make"}}}
	g.Nodes = append(g.Nodes, graph.Node{Label: "other", Desc: `layer "other"`,
		Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: makeStep}})
	g.Roots = []graph.Ref{2, 3}
	testenv.Must(t, g.Validate())
	_, err := New(g, Backend{Content: m, Records: m, Checker: f, Runner: f}, &recordedEvents{},
		Options{Bound: 1}).Run(t.Context())
	testenv.Must(t, err)

	tree := f.files[f.runs[0]][0] // of the task, which ran first
	appStarted, otherFailed := make(chan struct{}), make(chan struct{})
	gated := &gatedLayers{fakeRunner: f, fail: func(req Request) error {
		if req.Action.Steps[0].Files == nil { // layer "other"
			<-appStarted // so that the layer runs, though the job is about to fail
			return errors.New("make failed")
		}
		close(appStarted)
		<-otherFailed // so that the walk has the other layer's failure first
		return &StepError{Step: 0, Err: fmt.Errorf("copying sources from the build cache: f: %w",
			cache.Corrupt(tree, "it was changed"))}
	}}
	ctx, trace := ForJob(t.Context())
	s := New(g, Backend{Content: m, Records: m, Checker: f, Runner: gated}, &recordedEvents{},
		Options{Rebuild: true, Bound: 2})
	s.handled = func(c completion) {
		if c.ref == 3 {
			close(otherFailed)
		}
	}
	if _, err := s.Run(ctx); err == nil {
		t.Fatal("the build whose other layer failed succeeded")
	}
	_, spans, err := trace.Save(ctx, m)
	testenv.Must(t, err)
	apps := 0
	for _, sp := range spans {
		if sp.Name == `layer "app"` {
			apps++
			if sp.Args["outcome"] != outcomeFailed {
				t.Errorf("the layer that found its input corrupt after the job failed says %v; want failed", sp.Args)
			}
		}
	}
	if apps != 1 {
		t.Errorf("the layer has %d spans; want the one that found its input corrupt", apps)
	}
}
