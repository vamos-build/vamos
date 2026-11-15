package apitest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// finishTraced runs req as a job on ex to its end, and returns its outcome and the manifest it names, checking that
// the outcome names one the executor's content store holds, which Trace gives by the job's ID too.
// A job that fails is fine if fails is set.
func finishTraced(t *testing.T, f Fixture, ex api.Executor, req api.Request, fails bool) (api.Outcome, api.JobTrace) {
	t.Helper()
	ctx := t.Context()
	id, err := ex.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil && !fails {
		t.Fatalf("Result = %+v, %v; want success", outcome, err)
	}
	return outcome, manifestOf(t, f, ex, id, outcome)
}

// manifestOf returns the manifest that outcome, the job id's, names, checking that the executor's content store
// holds it and that Trace gives it.
func manifestOf(t *testing.T, f Fixture, ex api.Executor, id api.JobID, outcome api.Outcome) api.JobTrace {
	t.Helper()
	if outcome.Trace == nil {
		t.Fatalf("the outcome %+v names no manifest", outcome)
	}
	if missing := f.Missing(t, ex, []cache.Blob{*outcome.Trace}); len(missing) != 0 {
		t.Fatalf("the manifest %v isn't in the content store", *outcome.Trace)
	}
	if b, err := ex.Trace(t.Context(), id); err != nil || b != *outcome.Trace {
		t.Errorf("Trace = %v, %v; want the outcome's manifest, %v", b, err, *outcome.Trace)
	}
	manifest, err := api.DecodeJobTrace(f.Read(t, ex, *outcome.Trace))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// entryOf returns the entry of node in manifest, failing the test if it has none.
func entryOf(t *testing.T, manifest api.JobTrace, node graph.Ref) api.TraceEntry {
	t.Helper()
	for _, e := range manifest.Entries {
		if e.Node == node {
			return e
		}
	}
	t.Fatalf("the manifest has no entry for node %d: %+v", node, manifest.Entries)
	return api.TraceEntry{}
}

// attemptOf returns the attempt trace that e names, failing the test if it names none or it can't be read.
func attemptOf(t *testing.T, f Fixture, ex api.Executor, e api.TraceEntry) tracing.Attempt {
	t.Helper()
	if e.Trace == nil {
		t.Fatalf("the entry %+v names no attempt trace", e)
	}
	a, err := tracing.DecodeAttempt(f.Read(t, ex, *e.Trace))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != e.Attempt {
		t.Errorf("the attempt trace is of attempt %q; want %q, which the entry names", a.ID, e.Attempt)
	}
	return a
}

// testTraces checks that every graph, push, export and publish job's outcome names its manifest, stored, which
// Trace gives too: a built target's entry names its attempt's trace, and so does a cold pull's.
func testTraces(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	server := httptest.NewServer(f.Registry)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)

	built, manifest := finishTraced(t, f, ex, api.Request{Graph: new(layerGraph(t, f, ex, "traced"))}, false)
	layer := built.Results[0]
	if e := entryOf(t, manifest, layer.Node); e.Outcome != api.NodeMiss || !e.Started {
		t.Errorf("the built layer's entry = %+v; want a miss in an attempt it started", e)
	} else {
		attemptOf(t, f, ex, e)
	}

	push := api.PushRequest{Result: layer, Destination: host + "/base:v1"}
	finishTraced(t, f, ex, api.Request{Push: &push}, false)
	publish := api.PublishRequest{
		Images: []api.PublishImage{{Result: layer, Repository: host + "/team/app"}},
		Tags:   []string{"v1"},
	}
	finishTraced(t, f, ex, api.Request{Publish: &publish}, false)
	if !f.NoExport {
		export := api.ExportRequest{Result: layer}
		finishTraced(t, f, ex, api.Request{Export: &export}, false)
	}

	// A pull of what was pushed, which this executor hasn't pulled, resolves its tag in an attempt.
	_, manifest = finishTraced(t, f, ex, api.Request{Graph: new(pullGraph(t, f, ex, host+"/base:v1"))}, false)
	if e := entryOf(t, manifest, 0); e.Outcome != api.NodeMiss || !e.Started {
		t.Errorf("the cold pull's entry = %+v; want a miss in an attempt it started", e)
	} else {
		attemptOf(t, f, ex, e)
	}
}

// testSharedAttemptTrace checks that two jobs that share one attempt, a pull of a registry that answers only once
// both wait for it, name the same attempt and its trace, though the attempt fails.
func testSharedAttemptTrace(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	release := make(chan struct{})
	var once bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		if !once {
			close(release)
		}
	})
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	req := pullGraph(t, f, ex, host+"/shared:v1")
	var ids []api.JobID
	for range 2 {
		id, err := ex.Submit(ctx, api.Request{Graph: &req})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	time.Sleep(500 * time.Millisecond) // for both to attach to the one attempt
	close(release)
	once = true
	var entries []api.TraceEntry
	for _, id := range ids {
		watchAll(t, ex, id, 0)
		outcome, err := ex.Result(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Failure == nil {
			t.Fatalf("the pull of an image the registry lacks succeeded: %+v", outcome)
		}
		entries = append(entries, entryOf(t, manifestOf(t, f, ex, id, outcome), 0))
	}
	a, b := entries[0], entries[1]
	if a.Attempt == "" || a.Attempt != b.Attempt || a.Trace == nil || b.Trace == nil || *a.Trace != *b.Trace {
		t.Errorf("the jobs' entries = %+v and %+v; want the same attempt and trace", a, b)
	}
	if a.Started == b.Started {
		t.Errorf("both jobs say they started the attempt, or neither does: %+v and %+v", a, b)
	}
	if _, err := ex.Trace(ctx, "no such job"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Trace of an unknown job = %v, want %v", err, api.ErrNoJob)
	}
}
