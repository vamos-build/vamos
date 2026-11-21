package coord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// readManifest returns the manifest that outcome names, from f's store.
func readManifest(t *testing.T, f *fixture, outcome api.Outcome) api.JobTrace {
	t.Helper()
	if outcome.Trace == nil {
		t.Fatalf("the outcome %+v names no manifest", outcome)
	}
	data, err := f.m.Read(t.Context(), *outcome.Trace, tracing.MaxTraceSize)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := api.DecodeJobTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// readAttempt returns the attempt trace that the entry of node in manifest names, from f's store.
func readAttempt(t *testing.T, f *fixture, manifest api.JobTrace, node graph.Ref) tracing.Attempt {
	t.Helper()
	for _, e := range manifest.Entries {
		if e.Node != node {
			continue
		}
		if e.Trace == nil {
			t.Fatalf("node %d's entry %+v names no attempt trace", node, e)
		}
		data, err := f.m.Read(t.Context(), *e.Trace, tracing.MaxTraceSize)
		if err != nil {
			t.Fatal(err)
		}
		a, err := tracing.DecodeAttempt(data)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	t.Fatalf("the manifest has no entry for node %d", node)
	return tracing.Attempt{}
}

// TestAttemptTraceAcrossTries checks that an attempt's trace holds its workers' spans, of a try that was lost and of
// the one that succeeded, each on the coordinator's clock, and that the result says the run that made it began with
// the successful try and took no longer, waiting for a worker and the lost try left out.
func TestAttemptTraceAcrossTries(t *testing.T) {
	f := newFixture(t, Config{})
	req := testGraph(t, f, "tries")

	// The worker takes task "one", says it started, and goes silent, as if it died; the try is lost, and retried on
	// the worker, its clock now an hour ahead.
	isOne := func(a *remote.Assignment) bool {
		return a.Request != nil && a.Request.Action.Kind == sched.KindTask && len(a.Request.Action.Steps) > 0 &&
			a.Request.Action.Steps[0].Argv[0] == "one"
	}
	var (
		mu       sync.Mutex
		firstTry string
		lostAt   time.Time
		oneKey   digest.Digest
		w        *fakeWorker
	)
	retried := make(chan struct{})
	w = newWorker(t, f, amd64, 4, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if !isOne(a) {
			return fakeRun(f, nil)(a, killed)
		}
		ctx := context.Background()
		mu.Lock()
		first := firstTry == ""
		if first {
			firstTry, lostAt = a.Attempt, time.Now()
		}
		oneKey = a.Request.Key
		mu.Unlock()
		if first {
			now := time.Now()
			span := tracing.Span{ID: "w-1", Process: "worker", Track: "t", Name: "first try", Start: now, End: now}
			if err := w.client.Events(ctx, a.Attempt, remote.AttemptEvents{Spans: []tracing.Span{span}}); err != nil {
				t.Errorf("sending the first try's span: %v", err)
			}
			select { // a silent worker never hears it was killed: it gives up once the retry is under way
			case <-retried:
			case <-time.After(5 * time.Second):
			}
			return remote.AttemptReport{Failure: sched.FailureOf(errors.New("lost"))}
		}
		close(retried)
		if _, err := w.client.Heartbeat(ctx, a.Attempt, remote.Heartbeat{
			Clock: &tracing.Offset{Offset: time.Hour}}); err != nil {
			t.Errorf("heartbeating: %v", err)
		}
		ahead := time.Now().Add(time.Hour)
		span := tracing.Span{ID: "w-2", Process: "worker", Track: "t", Name: "second try", Start: ahead, End: ahead}
		if err := w.client.Events(ctx, a.Attempt, remote.AttemptEvents{Spans: []tracing.Span{span}}); err != nil {
			t.Errorf("sending the second try's span: %v", err)
		}
		return fakeRun(f, nil)(a, killed)
	})
	w.silentOn = func(a *remote.Assignment) bool {
		mu.Lock()
		defer mu.Unlock()
		return firstTry == "" && isOne(a) || a.Attempt == firstTry
	}
	submitted := time.Now()
	id, err := f.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // within a lease, waiting for a worker to ask, which the run doesn't count
	w.start()

	var outcome api.Outcome
	if err := f.c.Watch(t.Context(), id, 0, func(e api.Event) error {
		if e.Kind == api.EventDone {
			outcome = *e.Outcome
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outcome.Failure != nil {
		t.Fatalf("the build failed: %+v", outcome.Failure)
	}
	attempt := readAttempt(t, f, readManifest(t, f, outcome), 1)
	found := map[string]tracing.Span{}
	for _, s := range attempt.Spans {
		found[s.Name] = s
	}
	for _, name := range []string{"first try", "second try"} {
		s, ok := found[name]
		if !ok {
			t.Errorf("the attempt's trace holds no %q span; it holds %v", name, attempt.Spans)
			continue
		}
		if d := time.Until(s.Start); d > time.Minute || d < -time.Minute {
			t.Errorf("the %q span starts %v from now; want it on the coordinator's clock", name, d)
		}
	}

	mu.Lock()
	key, lost := oneKey, lostAt
	mu.Unlock()
	result, err := f.m.Result(t.Context(), key)
	if err != nil || result == nil || result.Provenance == nil {
		t.Fatalf("task one's result = %+v, %v", result, err)
	}
	p := result.Provenance
	if !p.Started.After(lost) || p.Took <= 0 || p.Took > f.c.cfg.Lease {
		t.Errorf("the result's run began at %v and took %v; want after the lost try at %v, and less than its lease",
			p.Started, p.Took, lost)
	}
	if p.Started.Sub(submitted) < 200*time.Millisecond {
		t.Errorf("the result's run began %v after the build was submitted; want the wait for a worker left out",
			p.Started.Sub(submitted))
	}
}

// manifestRefusing is a content store that refuses to store a job's manifest, and stores everything else.
type manifestRefusing struct{ cache.ContentStore }

func (s manifestRefusing) PutBytes(ctx context.Context, data []byte) (cache.Blob, error) {
	if h, err := tracing.ReadHeader(data); err == nil && h.Kind == api.KindJob {
		return cache.Blob{}, errors.New("the store is full")
	}
	return s.ContentStore.PutBytes(ctx, data)
}

// TestJobManifests checks that a cancelled build's outcome names its stored manifest, that a build whose manifest
// can't be stored succeeds all the same, naming none, and that a released build's trace is still found by its ID once
// it is evicted, on a coordinator without Config.State.
func TestJobManifests(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		f := newFixture(t, Config{})
		run, ended := blockedRun(t, f)
		startWorker(t, f, amd64, 4, run)
		t.Cleanup(ended)
		req := testGraph(t, f, "cancelled")
		id, err := f.c.Submit(t.Context(), api.Request{Graph: &req})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return f.c.sharing.Attached() > 0 })
		if err := f.c.Cancel(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		var outcome api.Outcome
		if err := f.c.Watch(t.Context(), id, 0, func(e api.Event) error {
			if e.Kind == api.EventDone {
				outcome = *e.Outcome
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !outcome.Cancelled {
			t.Fatalf("the build's outcome = %+v; want it cancelled", outcome)
		}
		readManifest(t, f, outcome)
	})

	t.Run("not stored", func(t *testing.T) {
		m := cache.NewMemory()
		f := newFixtureOver(t, manifestRefusing{m}, m, Config{Lease: 300 * time.Millisecond,
			Heartbeat: 50 * time.Millisecond, Warn: func(error) {}})
		startWorker(t, f, amd64, 4, fakeRun(f, nil))
		outcome, _ := runJob(t, f.c, testGraph(t, f, "not stored"))
		if outcome.Failure != nil || len(outcome.Results) != 1 || outcome.Trace != nil {
			t.Errorf("the build's outcome = %+v; want it succeeded, naming no manifest", outcome)
		}
	})

	t.Run("evicted without state", func(t *testing.T) {
		f := newFixture(t, Config{Grace: 100 * time.Millisecond})
		startWorker(t, f, amd64, 4, fakeRun(f, nil))
		ctx := t.Context()
		req := testGraph(t, f, "evicted")
		id, err := f.c.Submit(ctx, api.Request{Graph: &req})
		if err != nil {
			t.Fatal(err)
		}
		var outcome api.Outcome
		if err := f.c.Watch(ctx, id, 0, func(e api.Event) error {
			if e.Kind == api.EventDone {
				outcome = *e.Outcome
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		readManifest(t, f, outcome)
		if err := f.c.Release(ctx, id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			_, err := f.c.Result(ctx, id)
			return errors.Is(err, api.ErrExpired)
		})
		if got, err := f.c.Trace(ctx, id); err != nil || got != *outcome.Trace {
			t.Errorf("the evicted build's trace = %v, %v; want %v", got, err, *outcome.Trace)
		}
	})
}

// TestAttemptSpansKeepTheirOrder checks that a worker's spans, sent in batches that each carry the clock's offset it
// estimated before the attempt started, keep the order they ran in on the coordinator's clock, though its heartbeats
// refine the estimate between the batches by more than the gap between two steps.
func TestAttemptSpansKeepTheirOrder(t *testing.T) {
	f := newFixture(t, Config{})
	req := testGraph(t, f, "order")
	isOne := func(a *remote.Assignment) bool {
		return a.Request != nil && a.Request.Action.Kind == sched.KindTask && len(a.Request.Action.Steps) > 0 &&
			a.Request.Action.Steps[0].Argv[0] == "one"
	}
	var w *fakeWorker
	w = newWorker(t, f, amd64, 4, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if !isOne(a) {
			return fakeRun(f, nil)(a, killed)
		}
		ctx := context.Background()
		before := &tracing.Offset{Offset: time.Hour, Uncertainty: 10 * time.Millisecond} // the worker an hour ahead
		start := time.Now().Add(time.Hour)
		first := tracing.Span{ID: "w-1", Process: "worker", Track: "t", Name: "first step", Start: start,
			End: start.Add(time.Millisecond)}
		if err := w.client.Events(ctx, a.Attempt, remote.AttemptEvents{Spans: []tracing.Span{first},
			Clock: before}); err != nil {
			t.Errorf("sending the first step: %v", err)
		}
		// A better estimate, 50ms off the first, far more than the 10µs between the steps.
		better := &tracing.Offset{Offset: time.Hour + 50*time.Millisecond, Uncertainty: time.Millisecond}
		if _, err := w.client.Heartbeat(ctx, a.Attempt, remote.Heartbeat{Clock: better}); err != nil {
			t.Errorf("heartbeating: %v", err)
		}
		then := first.End.Add(10 * time.Microsecond)
		second := tracing.Span{ID: "w-2", Process: "worker", Track: "t", Name: "second step", Start: then,
			End: then.Add(time.Millisecond)}
		if err := w.client.Events(ctx, a.Attempt, remote.AttemptEvents{Spans: []tracing.Span{second},
			Clock: before}); err != nil {
			t.Errorf("sending the second step: %v", err)
		}
		return fakeRun(f, nil)(a, killed)
	})
	w.start()
	id, err := f.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	var outcome api.Outcome
	if err := f.c.Watch(t.Context(), id, 0, func(e api.Event) error {
		if e.Kind == api.EventDone {
			outcome = *e.Outcome
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outcome.Failure != nil {
		t.Fatalf("the build failed: %+v", outcome.Failure)
	}
	found := map[string]tracing.Span{}
	for _, s := range readAttempt(t, f, readManifest(t, f, outcome), 1).Spans {
		found[s.Name] = s
	}
	first, second := found["first step"], found["second step"]
	if first.ID == "" || second.ID == "" {
		t.Fatalf("the attempt's trace holds %v; want both steps", found)
	}
	if !first.End.Before(second.Start) {
		t.Errorf("the first step ends at %v, after the second starts at %v; want them in the order they ran",
			first.End, second.Start)
	}
	if d := time.Until(first.Start); d > time.Minute || d < -time.Minute {
		t.Errorf("the first step starts %v from now; want it on the coordinator's clock", d)
	}
}
