package sched

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// refusingCancelled is a content store that refuses a put whose context has ended, as a remote one would.
type refusingCancelled struct{ cache.ContentStore }

func (r refusingCancelled) PutBytes(ctx context.Context, data []byte) (cache.Blob, error) {
	if err := ctx.Err(); err != nil {
		return cache.Blob{}, err
	}
	return r.ContentStore.PutBytes(ctx, data)
}

// refusingAll is a content store that refuses every put.
type refusingAll struct{ cache.ContentStore }

func (refusingAll) PutBytes(context.Context, []byte) (cache.Blob, error) {
	return cache.Blob{}, errors.New("the store is full")
}

// TestForJobSaves checks that a job always records its spans, whether or not its client asked for them, and saves
// them with its nodes' entries as its manifest, even once the job is cancelled, and that a manifest that can't be
// stored is reported, naming none.
func TestForJobSaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx, trace := ForJob(ctx)
	_, span := tracing.Start(ctx, "schedule")
	span.End()
	blob := &cache.Blob{Digest: digest.FromString("attempt"), Size: 3}
	trace.Record(api.TraceEntry{Node: 1, Outcome: api.NodeHit})
	trace.Record(api.TraceEntry{Node: 2, Outcome: api.NodeMiss, Started: true, Attempt: "a1", Trace: blob})
	trace.Record(api.TraceEntry{Node: 1, Outcome: api.NodeMiss, Cause: "lost", Attempt: "a2"}) // a regeneration
	cancel()

	store := cache.NewMemory()
	b, err := trace.Save(ctx, refusingCancelled{store})
	if err != nil || b == nil {
		t.Fatalf("saving a cancelled job's manifest = %v, %v; want it stored", b, err)
	}
	data, err := store.Read(context.Background(), *b, tracing.MaxTraceSize)
	testenv.Must(t, err)
	manifest, err := api.DecodeJobTrace(data)
	testenv.Must(t, err)
	if len(manifest.Spans) != 1 || manifest.Spans[0].Name != "schedule" {
		t.Errorf("the manifest's spans = %+v; want the job's schedule span", manifest.Spans)
	}
	want := []api.TraceEntry{
		{Node: 1, Outcome: api.NodeMiss, Cause: "lost", Attempt: "a2"},
		{Node: 2, Outcome: api.NodeMiss, Started: true, Attempt: "a1", Trace: blob},
	}
	if !reflect.DeepEqual(manifest.Entries, want) {
		t.Errorf("the manifest's entries = %+v; want %+v, the regeneration in place of the hit", manifest.Entries, want)
	}

	if b, err := trace.Save(context.Background(), refusingAll{store}); err == nil || b != nil {
		t.Errorf("saving to a store that refuses = %v, %v; want none, and why", b, err)
	}
}

// TestJobsRefusedDuringPrune checks that no job starts while an exclusive one, a prune, runs, nor a prune while
// another job runs, and that both are accepted again once it has ended; and that none starts after stop.
func TestJobsRefusedDuringPrune(t *testing.T) {
	var jobs Jobs
	release := make(chan struct{})
	block := func(context.Context, Events) api.Outcome {
		<-release
		return api.Outcome{}
	}
	id, err := jobs.Start(true, block)
	testenv.Must(t, err)
	if _, err := jobs.Start(false, block); !errors.Is(err, ErrPruning) {
		t.Errorf("a job during a prune = %v, want %v", err, ErrPruning)
	}
	if _, err := jobs.Start(true, block); !errors.Is(err, ErrPruning) {
		t.Errorf("a prune during a prune = %v, want %v", err, ErrPruning)
	}
	close(release)
	j, err := jobs.Lookup(id)
	testenv.Must(t, err)
	testenv.Must(t, j.Watch(t.Context(), 0, func(api.Event) error { return nil }))

	running := make(chan struct{})
	_, err = jobs.Start(false, func(ctx context.Context, _ Events) api.Outcome {
		<-ctx.Done()
		<-running
		return api.Outcome{}
	})
	testenv.Must(t, err)
	if _, err := jobs.Start(true, block); err == nil || !strings.Contains(err.Error(), "1 other jobs are running") {
		t.Errorf("a prune while a job runs = %v, want it refused, counting the job", err)
	}
	close(running)
	jobs.Stop()
	if _, err := jobs.Start(false, block); !errors.Is(err, ErrClosed) {
		t.Errorf("a job after stop = %v, want %v", err, ErrClosed)
	}
}

// TestJobsReservation checks that a reservation holds its place until it is started or cancelled: a reserved prune
// excludes other jobs, a reserved job counts against a prune and the limit, and a cancelled one frees its place;
// that the terminal event Ending is given is the one watchers see; and that a reservation started after stop is
// refused.
func TestJobsReservation(t *testing.T) {
	var jobs Jobs
	jobs.Limit(1)
	prune, err := jobs.Reserve(Spec{Exclusive: true})
	testenv.Must(t, err)
	if _, err := jobs.Reserve(Spec{}); !errors.Is(err, ErrPruning) {
		t.Errorf("a job while a prune is reserved = %v, want %v", err, ErrPruning)
	}
	prune.Cancel()
	build, err := jobs.Reserve(Spec{Build: true})
	testenv.Must(t, err)
	if _, err := jobs.Reserve(Spec{Exclusive: true}); err == nil || !strings.Contains(err.Error(), "1 other jobs") {
		t.Errorf("a prune while a job is reserved = %v, want it refused, counting the reservation", err)
	}
	if _, err := jobs.Reserve(Spec{}); !errors.Is(err, ErrBusy) {
		t.Errorf("a job beyond the limit while one is reserved = %v, want %v", err, ErrBusy)
	}
	build.Cancel()

	var ending api.Event
	spec := Spec{ID: "0123456789abcdef", First: 100, Ending: func(done api.Event) { ending = done }}
	r, err := jobs.Reserve(spec)
	testenv.Must(t, err)
	if r.ID() != spec.ID {
		t.Errorf("the reservation's ID = %s, want %s", r.ID(), spec.ID)
	}
	id, err := r.Start(func(_ context.Context, ev Events) api.Outcome {
		ev.Status(0, "running")
		return api.Outcome{Cancelled: true}
	})
	testenv.Must(t, err)
	j, err := jobs.Lookup(id)
	testenv.Must(t, err)
	var done api.Event
	testenv.Must(t, j.Watch(t.Context(), 100, func(e api.Event) error {
		done = e
		return nil
	}))
	if ending.Position != 101 || done.Position != 101 || done.Kind != api.EventDone || !ending.Outcome.Cancelled {
		t.Errorf("Ending was given %+v, and watchers saw %+v; want the terminal event at 101 in both", ending, done)
	}

	late, err := jobs.Reserve(Spec{})
	testenv.Must(t, err)
	jobs.Stop()
	if _, err := late.Start(func(context.Context, Events) api.Outcome { return api.Outcome{} }); !errors.Is(err,
		ErrClosed) {
		t.Errorf("a reservation started after stop = %v, want %v", err, ErrClosed)
	}
}

// TestJobsLimit checks that the table refuses a job while as many are open as its limit allows, under its lock,
// and accepts one again once a job has ended.
func TestJobsLimit(t *testing.T) {
	var jobs Jobs
	jobs.Limit(2)
	release := make(chan struct{})
	work := func(ctx context.Context, ev Events) api.Outcome {
		<-release
		return api.Outcome{}
	}
	first, err := jobs.Start(false, work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Start(false, work); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Start(false, work); !errors.Is(err, ErrBusy) {
		t.Errorf("a third job = %v, want %v", err, ErrBusy)
	}
	close(release)
	j, err := jobs.Lookup(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Watch(t.Context(), 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); jobs.Active() > 0; {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the jobs didn't end")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := jobs.Start(false, work); err != nil {
		t.Errorf("a job after the others ended = %v, want it accepted", err)
	}
	jobs.Stop()
}

// TestJobsExpire checks that a build no one watches has its demand withdrawn after the grace period, while what
// runs finishes, that a finished job is evicted after the window, or at once when released, and is then reported
// expired rather than unknown, and that a push never expires.
func TestJobsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	jobs := &Jobs{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock(); jobs.Sweep() }
	jobs.Expire(time.Minute, 2*time.Minute)
	withdrawn := make(chan struct{})
	work := func(ctx context.Context, ev Events) api.Outcome {
		<-Withdrawn(ctx)
		close(withdrawn)
		return Failed(ErrWithdrawn)
	}
	build, err := jobs.StartBuild(work)
	if err != nil {
		t.Fatal(err)
	}
	push, err := jobs.Start(false, func(ctx context.Context, ev Events) api.Outcome {
		<-ctx.Done()
		return api.Outcome{Cancelled: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Within the grace period nothing happens; a watcher present at its end holds the build.
	advance(59 * time.Second)
	select {
	case <-withdrawn:
		t.Fatal("the build's demand was withdrawn within the grace period")
	default:
	}
	wctx, stopWatch := context.WithCancel(t.Context())
	watching := make(chan error, 1)
	j, err := jobs.Lookup(build)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		watching <- j.Watch(wctx, 0, func(api.Event) error { return nil })
	}()
	for start := time.Now(); ; {
		j.mu.Lock()
		watchers := j.watchers
		j.mu.Unlock()
		if watchers == 1 {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("the watcher never attached")
		}
		time.Sleep(time.Millisecond)
	}
	advance(2 * time.Minute)
	select {
	case <-withdrawn:
		t.Fatal("the build's demand was withdrawn while it was watched")
	default:
	}
	stopWatch()
	if err := <-watching; !errors.Is(err, context.Canceled) {
		t.Fatalf("the watch ended with %v", err)
	}
	// The grace period counts from the watcher leaving.
	advance(59 * time.Second)
	select {
	case <-withdrawn:
		t.Fatal("the build's demand was withdrawn within the grace period after its watcher left")
	default:
	}
	advance(2 * time.Second)
	select {
	case <-withdrawn:
	case <-time.After(5 * time.Second):
		t.Fatal("the build's demand wasn't withdrawn after the grace period")
	}
	if err := j.Watch(t.Context(), 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if outcome, err := j.Result(); err != nil || outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message, "expired") {
		t.Errorf("the expired build's outcome = %+v, %v; want a failure saying it expired", outcome, err)
	}
	// The finished build stays readable for the window, then is evicted and remembered as expired; the push,
	// unwatched all along, is still running. A watch within the window, as a client holding the build through its
	// delivery makes, starts the window over.
	advance(time.Minute)
	if _, err := jobs.Lookup(build); err != nil {
		t.Errorf("Lookup within the window = %v, want the job", err)
	}
	if err := j.Watch(t.Context(), 1000, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	advance(90 * time.Second)
	if _, err := jobs.Lookup(build); err != nil {
		t.Errorf("Lookup within the window of the last watch = %v, want the job still held", err)
	}
	advance(time.Minute)
	if _, err := jobs.Lookup(build); !errors.Is(err, api.ErrExpired) {
		t.Errorf("Lookup after the window = %v, want %v", err, api.ErrExpired)
	}
	if _, err := jobs.Lookup("never"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Lookup of an unknown job = %v, want %v", err, api.ErrNoJob)
	}
	p, err := jobs.Lookup(push)
	if err != nil {
		t.Fatalf("the push = %v, want it still there", err)
	}
	if _, err := p.Result(); !errors.Is(err, api.ErrRunning) {
		t.Errorf("the push's result = %v, want it running", err)
	}
	// A released job is evicted at the next sweep.
	p.Cancel()
	if err := p.Watch(t.Context(), 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p.Release()
	advance(0)
	if _, err := jobs.Lookup(push); !errors.Is(err, api.ErrExpired) {
		t.Errorf("Lookup of the released push = %v, want %v", err, api.ErrExpired)
	}
	jobs.Stop()
}

// TestJobsRetainBytes checks that the output a job's retained events hold is bounded by bytes as well as by count.
func TestJobsRetainBytes(t *testing.T) {
	var jobs Jobs
	jobs.RetainBytes(100)
	release := make(chan struct{})
	id, err := jobs.Start(false, func(ctx context.Context, ev Events) api.Outcome {
		for range 10 {
			ev.Output(0, 1, make([]byte, 30))
		}
		<-release
		return api.Outcome{}
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := jobs.Lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); ; {
		j.mu.Lock()
		first, n := j.first, len(j.events)
		j.mu.Unlock()
		if first+int64(n) == 10 {
			if n != 3 || first != 7 {
				t.Errorf("retained %d events from %d, want the last 3, which fit in 100 bytes", n, first)
			}
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("the events never arrived")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	jobs.Stop()
}

// TestJobForgetsDroppedEvents checks that an event the Job no longer retains isn't kept reachable by the array
// behind its events, so that the bound on retained output bounds the memory too.
func TestJobForgetsDroppedEvents(t *testing.T) {
	j := &Job{retention: 10, retentionBytes: 100, changed: make(chan struct{}), events: make([]api.Event, 0, 10)}
	backing := j.events[:cap(j.events)]
	for range 5 {
		j.append(api.Event{Data: make([]byte, 30)})
	}
	if j.first != 2 || len(j.events) != 3 {
		t.Fatalf("retained %d events from %d, want the last 3, which fit in 100 bytes", len(j.events), j.first)
	}
	for i, e := range backing[:j.first] {
		if e.Data != nil {
			t.Errorf("the dropped event at %d still holds %d bytes", i, len(e.Data))
		}
	}
}

// TestRunJobWithoutStore checks that a job whose content store can't be opened returns its outcome all the same,
// naming no manifest, and reports why.
func TestRunJobWithoutStore(t *testing.T) {
	var warned []error
	outcome := RunJob(context.Background(),
		func() (cache.ContentStore, error) { return nil, errors.New("the build cache is locked") },
		func(err error) { warned = append(warned, err) },
		func(ctx context.Context) api.Outcome { return api.Outcome{Results: []api.Result{{}}} })
	if outcome.Trace != nil || len(outcome.Results) != 1 {
		t.Errorf("outcome = %+v; want its results, naming no manifest", outcome)
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "the build cache is locked") {
		t.Errorf("warned %v; want why the manifest wasn't stored", warned)
	}
}
