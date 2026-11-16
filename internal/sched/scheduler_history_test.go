package sched

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// countingHistory is a record store that counts what reads and adds a target history, and fails them all while
// broken is set.
type countingHistory struct {
	cache.RecordStore
	mu                    sync.Mutex
	newest, history, adds int
	broken                bool
}

func (r *countingHistory) count(n *int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	*n++
	if r.broken {
		return errors.New("the history is unreadable")
	}
	return nil
}

func (r *countingHistory) Newest(ctx context.Context, name digest.Digest) (cache.HistoryHead, error) {
	if err := r.count(&r.newest); err != nil {
		return cache.HistoryHead{}, err
	}
	return r.RecordStore.Newest(ctx, name)
}

func (r *countingHistory) History(ctx context.Context, name digest.Digest) (*cache.History, error) {
	if err := r.count(&r.history); err != nil {
		return nil, err
	}
	return r.RecordStore.History(ctx, name)
}

func (r *countingHistory) AddHistory(ctx context.Context, name digest.Digest, e cache.HistoryEntry,
	trees map[digest.Digest][]cache.HistoryFile, keep int) error {
	if err := r.count(&r.adds); err != nil {
		return err
	}
	return r.RecordStore.AddHistory(ctx, name, e, trees, keep)
}

// counts returns how many times Newest, History and AddHistory were called, and resets them.
func (r *countingHistory) counts() (newest, history, adds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	newest, history, adds = r.newest, r.history, r.adds
	r.newest, r.history, r.adds = 0, 0, 0
	return newest, history, adds
}

// historyGraph returns a graph of a task that adds the glob src, whose tree is tree, and runs cmd: nodes 0 and 1,
// with Targets, so that they keep history.
func historyGraph(t *testing.T, tree cache.Blob, cmd string) *graph.Graph {
	t.Helper()
	from := graph.Ref(0)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}},
		{Label: "gen", Desc: `task "gen"`, Target: "//:gen platform=linux/amd64", Task: &graph.Task{
			Base: graph.Scratch,
			Steps: []graph.Step{
				{Op: graph.OpAdd, Display: `add("src", "/src")`, From: &from, Dest: "/src"},
				{Op: graph.OpRun, Display: `run(["` + cmd + `"])`, Argv: []string{cmd}},
			},
			Output: &graph.Output{Source: "/out", Selection: sel},
		}},
	}}
	testenv.Must(t, g.Validate())
	return g
}

// sourceTree stores a tree of the files named, each holding its name and contents, and returns its blob.
func sourceTree(t *testing.T, m *cache.Memory, files map[string]string) cache.Blob {
	t.Helper()
	tree := &cache.Tree{}
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		content, ok := files[name]
		if !ok {
			continue
		}
		b, err := m.PutBytes(t.Context(), []byte(content))
		testenv.Must(t, err)
		tree.Entries = append(tree.Entries, cache.TreeEntry{Path: name, Type: cache.EntryFile, Mode: 0o644, Blob: &b})
	}
	b, err := m.PutBytes(t.Context(), tree.Encode())
	testenv.Must(t, err)
	return b
}

// runNamed runs g in the workspace named workspace, as a job would, on the stores records and m through r,
// and returns the job's manifest entry of node 1, the span of that node, and the job's error.
func runNamed(t *testing.T, g *graph.Graph, m *cache.Memory, records cache.RecordStore, r execution,
	workspace string) (api.TraceEntry, tracing.Span, error) {
	t.Helper()
	ctx, trace := ForJob(t.Context())
	b := Backend{Content: m, Records: records, Checker: r, Runner: NewSharing(r, m, records)}
	_, err := New(g, b, &recordedEvents{}, Options{Bound: 1, Workspace: workspace}).Run(ctx)
	saved, serr := trace.Save(ctx, m)
	testenv.Must(t, serr)
	data, rerr := m.Read(t.Context(), *saved, tracing.MaxTraceSize)
	testenv.Must(t, rerr)
	manifest, derr := api.DecodeJobTrace(data)
	testenv.Must(t, derr)
	var entry api.TraceEntry
	for _, e := range manifest.Entries {
		if e.Node == 1 {
			entry = e
		}
	}
	var span tracing.Span
	for _, s := range manifest.Spans {
		if s.Name == g.Nodes[1].Desc {
			span = s
		}
	}
	return entry, span, err
}

// TestSchedulerHistory checks that a miss is attributed from the target's history, and adds to it, that a fully
// cached build reads the head of each target's history once and adds nothing, that a hit on a history not used for
// a week refreshes it, that a failed target adds nothing, and that a history that can't be read fails no target.
func TestSchedulerHistory(t *testing.T) {
	m, f := fakeFixture(t)
	records := &countingHistory{RecordStore: m}
	one := sourceTree(t, m, map[string]string{"a.txt": "a", "b.txt": "b"})
	two := sourceTree(t, m, map[string]string{"a.txt": "a, edited", "c.txt": "c"})

	e, span, err := runNamed(t, historyGraph(t, one, "gen"), m, records, f, "com.acme")
	testenv.Must(t, err)
	if e.Cause != CauseNew || span.Args["miss"] != CauseNew {
		t.Errorf("the first build's entry %+v, span %v; want the cause new", e, span.Args)
	}
	if _, _, adds := records.counts(); adds != 1 {
		t.Errorf("the first build added %d entries; want 1", adds)
	}

	e, span, err = runNamed(t, historyGraph(t, two, "gen"), m, records, f, "com.acme")
	testenv.Must(t, err)
	if e.Cause != CauseOwnInputs || span.Args["miss"] != CauseOwnInputs {
		t.Fatalf("a build with changed sources: entry %+v, span %v; want own-inputs", e, span.Args)
	}
	for arg, want := range map[string]string{"added": "c.txt", "removed": "b.txt", "changed": "a.txt"} {
		if got, _ := span.Args[arg].([]any); len(got) != 1 || got[0] != want {
			t.Errorf("the span's %s = %v; want [%s]", arg, span.Args[arg], want)
		}
	}
	if _, history, _ := records.counts(); history != 1 {
		t.Errorf("a miss read the history %d times; want once", history)
	}

	// A fully cached build reads each target's head once, and adds nothing.
	e, span, err = runNamed(t, historyGraph(t, two, "gen"), m, records, f, "com.acme")
	testenv.Must(t, err)
	if e.Outcome != api.NodeHit || span.Args["miss"] != nil {
		t.Errorf("a cached build's entry %+v, span %v; want a hit, saying no cause", e, span.Args)
	}
	if newest, history, adds := records.counts(); newest != 1 || history != 0 || adds != 0 {
		t.Errorf("a cached build read %d heads and %d histories, and added %d; want 1, 0, 0", newest, history, adds)
	}

	// A hit on a history not used for over a week adds its entry again, so that a prune keeps it.
	m.SetHistoryUsed(HistoryName("com.acme", "//:gen platform=linux/amd64"), time.Now().Add(-8*24*time.Hour))
	if _, _, err := runNamed(t, historyGraph(t, two, "gen"), m, records, f, "com.acme"); err != nil {
		t.Fatal(err)
	}
	if _, _, adds := records.counts(); adds != 1 {
		t.Errorf("a hit on a history unused for 8 days added %d entries; want 1", adds)
	}

	// A failed target adds nothing.
	f.fail = func(Request) error { return errors.New("the step failed") }
	if _, _, err := runNamed(t, historyGraph(t, one, "fails"), m, records, f, "com.acme"); err == nil {
		t.Fatal("the failing target succeeded")
	}
	if _, _, adds := records.counts(); adds != 0 {
		t.Errorf("a failed target added %d entries; want none", adds)
	}
	f.fail = nil

	// A history that can't be read fails no target, and the span says why.
	records.broken = true
	_, span, err = runNamed(t, historyGraph(t, one, "broken"), m, records, f, "com.acme")
	if err != nil {
		t.Fatalf("a build with an unreadable history failed: %v", err)
	}
	if span.Args["history error"] == nil || span.Args["miss"] != CauseNew {
		t.Errorf("the span says %v; want a history error, and the cause new", span.Args)
	}
}

// TestSchedulerHistoryWorkspaces checks that two workspaces whose builds share one attempt each find their own cause,
// from their own history, and name the same attempt trace.
func TestSchedulerHistoryWorkspaces(t *testing.T) {
	m, f := fakeFixture(t)
	one := sourceTree(t, m, map[string]string{"a.txt": "a"})
	two := sourceTree(t, m, map[string]string{"a.txt": "a, edited"})
	// The first workspace has built the target before, with other sources; the second never has.
	if _, _, err := runNamed(t, historyGraph(t, one, "gen"), m, m, f, "com.acme.first"); err != nil {
		t.Fatal(err)
	}
	b := &blockingSharedRunner{fakeRunner: f, release: make(chan struct{})}
	s := NewSharing(b, m, m)
	type result struct {
		entry api.TraceEntry
		span  tracing.Span
	}
	results := make(chan result, 2)
	for _, workspace := range []string{"com.acme.first", "com.acme.second"} {
		go func() {
			ctx, trace := ForJob(t.Context())
			backend := Backend{Content: m, Records: m, Checker: b, Runner: s}
			g := historyGraph(t, two, "gen")
			_, err := New(g, backend, &recordedEvents{}, Options{Bound: 1, Workspace: workspace}).Run(ctx)
			testenv.Must(t, err)
			saved, err := trace.Save(ctx, m)
			testenv.Must(t, err)
			data, err := m.Read(t.Context(), *saved, tracing.MaxTraceSize)
			testenv.Must(t, err)
			manifest, err := api.DecodeJobTrace(data)
			testenv.Must(t, err)
			var r result
			for _, e := range manifest.Entries {
				if e.Node == 1 {
					r.entry = e
				}
			}
			for _, sp := range manifest.Spans {
				if sp.Name == g.Nodes[1].Desc {
					r.span = sp
				}
			}
			r.span.Args["workspace"] = workspace
			results <- r
		}()
	}
	waitFor(t, func() bool { return s.Attached() == 2 })
	close(b.release)
	got := map[string]result{}
	for range 2 {
		r := <-results
		got[r.span.Args["workspace"].(string)] = r
	}
	first, second := got["com.acme.first"], got["com.acme.second"]
	if first.entry.Cause != CauseOwnInputs || second.entry.Cause != CauseNew {
		t.Errorf("the causes are %q and %q; want own-inputs for the workspace that built it before, new for the other",
			first.entry.Cause, second.entry.Cause)
	}
	if first.entry.Trace == nil || second.entry.Trace == nil || *first.entry.Trace != *second.entry.Trace {
		t.Errorf("the entries name traces %v and %v; want the same", first.entry.Trace, second.entry.Trace)
	}
}

// blockingSharedRunner runs a fake runner's actions once release is closed.
type blockingSharedRunner struct {
	*fakeRunner
	release chan struct{}
}

func (b *blockingSharedRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeRunner.Run(ctx, req, ev)
}

// TestSchedulerHistoryCancelled checks that a target cancelled while it runs adds nothing to its history, and that a
// job that left a shared attempt, which then succeeded for another job, adds nothing either, while the other adds
// its entry.
func TestSchedulerHistoryCancelled(t *testing.T) {
	m, f := fakeFixture(t)
	records := &countingHistory{RecordStore: m}
	b := &blockingSharedRunner{fakeRunner: f, release: make(chan struct{})}
	s := NewSharing(b, m, records)
	g := historyGraph(t, sourceTree(t, m, map[string]string{"a.txt": "a"}), "gen")
	run := func(ctx context.Context) error {
		ctx, _ = ForJob(ctx)
		backend := Backend{Content: m, Records: records, Checker: b, Runner: s}
		_, err := New(g, backend, &recordedEvents{}, Options{Bound: 1, Workspace: "com.acme"}).Run(ctx)
		return err
	}
	leaving, leave := context.WithCancel(t.Context())
	left := make(chan error, 1)
	go func() { left <- run(leaving) }()
	waitFor(t, func() bool { return s.Attached() == 1 })
	stayed := make(chan error, 1)
	go func() { stayed <- run(t.Context()) }()
	waitFor(t, func() bool { return s.Attached() == 2 })
	leave()
	if err := <-left; err == nil {
		t.Fatal("the job that was cancelled succeeded")
	}
	if _, _, adds := records.counts(); adds != 0 {
		t.Errorf("the job that left the attempt added %d entries; want none", adds)
	}
	close(b.release)
	testenv.Must(t, <-stayed)
	if _, _, adds := records.counts(); adds != 1 {
		t.Errorf("the job that stayed added %d entries; want 1", adds)
	}

	// A lone target cancelled while it runs adds nothing.
	b.release = make(chan struct{})
	g = historyGraph(t, sourceTree(t, m, map[string]string{"a.txt": "other"}), "gen")
	cancelled, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(cancelled) }()
	waitFor(t, func() bool { return s.Attached() == 1 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled job succeeded")
	}
	if _, _, adds := records.counts(); adds != 0 {
		t.Errorf("a cancelled target added %d entries; want none", adds)
	}
}

// TestSchedulerHistoryParallel checks that targets that keep history run at once, on a pull and on each other, as a
// coordinator runs them, missing and then hitting, without reading what the walk writes meanwhile; run it with -race,
// which scripts/check doesn't, since the race detector is what finds such a read.
func TestSchedulerHistoryParallel(t *testing.T) {
	m, f := fakeFixture(t)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Target: "//:base platform=linux/amd64",
			Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
	}}
	for i := range 8 {
		base := graph.Ref(0)
		if i >= 4 {
			base = graph.Ref(i - 3) // on one of the first four, so that the walk records images while others run
		}
		name := string(rune('a' + i))
		g.Nodes = append(g.Nodes, graph.Node{Label: name, Desc: `layer "` + name + `"`,
			Target: "//:" + name + " platform=linux/amd64",
			Layer: &graph.Layer{Base: base, Platform: "linux/amd64", Steps: []graph.Step{
				{Op: graph.OpRun, Display: `run(["` + name + `"])`, Argv: []string{name}},
			}}})
		g.Roots = append(g.Roots, graph.Ref(i+1))
	}
	testenv.Must(t, g.Validate())
	for range 2 { // misses, then hits
		ctx, _ := ForJob(t.Context())
		b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(&lockedRunner{f: f}, m, m)}
		if _, err := New(g, b, &recordedEvents{}, Options{Bound: 8, Workspace: "com.acme"}).Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// lockedRunner runs a fake runner's actions one at a time, since the fake isn't safe for concurrent use.
type lockedRunner struct {
	mu sync.Mutex
	f  *fakeRunner
}

func (r *lockedRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Run(ctx, req, ev)
}
