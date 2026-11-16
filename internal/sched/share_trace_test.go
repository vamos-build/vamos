package sched

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// tracedOutputs collects what an Action prints, and what its job is told of the attempt's trace.
type tracedOutputs struct {
	outputs
	mu     sync.Mutex
	traces []AttemptTrace
}

// Traced keeps t.
func (o *tracedOutputs) Traced(t AttemptTrace) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.traces = append(o.traces, t)
}

// told returns what the job was told of the attempt's trace, failing the test unless it was told once.
func (o *tracedOutputs) told(t *testing.T) AttemptTrace {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.traces) != 1 {
		t.Fatalf("the job was told of %d attempt traces; want 1", len(o.traces))
	}
	return o.traces[0]
}

// attemptSpans returns the names of the spans of the attempt trace b in store, failing the test if it can't be read.
func attemptSpans(t *testing.T, store cache.ContentStore, id string, b *cache.Blob) []string {
	t.Helper()
	if b == nil {
		t.Fatal("no attempt trace was stored")
	}
	data, err := store.Read(context.Background(), *b, tracing.MaxTraceSize)
	testenv.Must(t, err)
	a, err := tracing.DecodeAttempt(data)
	testenv.Must(t, err)
	if a.ID != id {
		t.Errorf("the stored trace is of attempt %q; want %q", a.ID, id)
	}
	var names []string
	for _, s := range a.Spans {
		names = append(names, s.Name)
	}
	return names
}

// TestSharingTracesAttempts checks that the job that starts an attempt and one that joins it are told of the same
// stored trace, which holds the attempt's spans, and the same cache write span, which names the attempt and its
// trace; and that the result names the trace and when the run that made it began and how long it took.
func TestSharingTracesAttempts(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, m := testSharing(b)
	starter, joiner := &tracedOutputs{}, &tracedOutputs{}
	results := make(chan *cache.ActionResult, 2)
	go func() {
		r, err := s.attempt(t.Context(), testRequest(false), starter)
		testenv.Must(t, err)
		results <- r
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	go func() {
		r, err := s.attempt(t.Context(), testRequest(false), joiner)
		testenv.Must(t, err)
		results <- r
	}()
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	close(b.release)
	r := <-results
	<-results

	a, j := starter.told(t), joiner.told(t)
	if a.ID == "" || a.ID != j.ID || a.Trace == nil || j.Trace == nil || *a.Trace != *j.Trace || a.Left || j.Left {
		t.Fatalf("the starter was told %+v and the joiner %+v; want the same attempt and trace", a, j)
	}
	if names := attemptSpans(t, m, a.ID, a.Trace); !slices.Equal(names, []string{"started", "released"}) {
		t.Errorf("the attempt's trace holds %v; want [started released]", names)
	}
	if len(a.Write) != 1 || a.Write[0].Name != "cache write" || !slices.Equal(spanNames(a.Write), spanNames(j.Write)) {
		t.Fatalf("the starter was told of the write %+v and the joiner of %+v; want the one cache write",
			a.Write, j.Write)
	}
	if a.Write[0].Args["attempt"] != a.ID || a.Write[0].Args["attempt trace"] != a.Trace.Digest.String() {
		t.Errorf("the cache write says %v; want the attempt %s and its trace", a.Write[0].Args, a.ID)
	}
	if r.Trace == nil || *r.Trace != *a.Trace || r.Provenance == nil || r.Provenance.Started.IsZero() ||
		r.Provenance.Took <= 0 {
		t.Errorf("the result names trace %v with provenance %+v; want the attempt's, and when and how long it ran",
			r.Trace, r.Provenance)
	}
	recorded, err := m.Result(t.Context(), testRequest(false).Key)
	if err != nil || recorded == nil || recorded.Trace == nil || *recorded.Trace != *a.Trace {
		t.Errorf("the result recorded = %+v, %v; want it naming the attempt's trace", recorded, err)
	}
}

// spanNames returns the names of spans.
func spanNames(spans []tracing.Span) []string {
	var names []string
	for _, s := range spans {
		names = append(names, s.Name)
	}
	return names
}

// TestSharingTracesFailure checks that a failed attempt's trace is stored and named to its job.
func TestSharingTracesFailure(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{}), fail: map[int32]bool{1: true}}
	s, m := testSharing(b)
	out := &tracedOutputs{}
	done := make(chan error, 1)
	go func() {
		_, err := s.attempt(t.Context(), testRequest(false), out)
		done <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	close(b.gate(1))
	if err := <-done; err == nil {
		t.Fatal("the attempt succeeded; want it failed")
	}
	a := out.told(t)
	if names := attemptSpans(t, m, a.ID, a.Trace); !slices.Equal(names, []string{"started", "released"}) {
		t.Errorf("the failed attempt's trace holds %v; want [started released]", names)
	}
}

// TestSharingStarterLeaves checks that a starter that is cancelled while another job remains attached learns the
// attempt's ID alone, and records none of its spans, while the job that waits for the end gets the whole trace.
func TestSharingStarterLeaves(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, m := testSharing(b)
	recorder := tracing.NewRecorder("executor", nil)
	starterCtx, cancel := context.WithCancel(tracing.WithRecorder(t.Context(), recorder, ""))
	starter, other := &tracedOutputs{}, &tracedOutputs{}
	starterDone := make(chan error, 1)
	go func() {
		_, err := s.attempt(starterCtx, testRequest(false), starter)
		starterDone <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	otherDone := make(chan error, 1)
	go func() {
		_, err := s.attempt(t.Context(), testRequest(false), other)
		otherDone <- err
	}()
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	cancel()
	if err := <-starterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the starter got %v, want %v", err, context.Canceled)
	}
	close(b.release)
	testenv.Must(t, <-otherDone)

	left, whole := starter.told(t), other.told(t)
	if !left.Left || left.Trace != nil || left.ID != whole.ID || left.Write != nil {
		t.Errorf("the starter that left was told %+v; want the attempt's ID alone", left)
	}
	if names := attemptSpans(t, m, whole.ID, whole.Trace); !slices.Equal(names, []string{"started", "released"}) {
		t.Errorf("the attempt's trace holds %v; want [started released]", names)
	}
	if spans := recorder.Spans(); len(spans) != 0 {
		t.Errorf("the starter's own trace recorded %v; want none of the attempt's spans", spanNames(spans))
	}
}

// TestSharingLoneCancelledTraced checks that a job alone on an attempt that it cancels waits for the attempt to end
// and gets its final trace, stored though the content store refuses a cancelled context.
func TestSharingLoneCancelledTraced(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	m := cache.NewMemory()
	s := NewSharing(b, refusingCancelled{m}, m)
	ctx, cancel := context.WithCancel(t.Context())
	out := &tracedOutputs{}
	done := make(chan error, 1)
	go func() {
		_, err := s.attempt(ctx, testRequest(false), out)
		done <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the job got %v, want %v", err, context.Canceled)
	}
	a := out.told(t)
	if a.Left {
		t.Fatalf("the last job to leave was told it left the attempt running")
	}
	if names := attemptSpans(t, m, a.ID, a.Trace); !slices.Equal(names, []string{"started"}) {
		t.Errorf("the cancelled attempt's trace holds %v; want [started]", names)
	}
}

// heldRecords is a record store whose SetResult waits until release is closed, once entered is.
type heldRecords struct {
	cache.RecordStore
	entered chan struct{}
	release chan struct{}
}

func (r *heldRecords) SetResult(ctx context.Context, key digest.Digest, result *cache.ActionResult) error {
	close(r.entered)
	<-r.release
	return r.RecordStore.SetResult(ctx, key, result)
}

// TestSharingJoinWhilePublishing checks that a job that attaches while the attempt's result is being recorded gets
// the same trace as the job that started it.
func TestSharingJoinWhilePublishing(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	m := cache.NewMemory()
	records := &heldRecords{RecordStore: m, entered: make(chan struct{}), release: make(chan struct{})}
	s := NewSharing(b, m, records)
	starter, late := &tracedOutputs{}, &tracedOutputs{}
	done := make(chan error, 2)
	go func() {
		_, err := s.attempt(t.Context(), testRequest(false), starter)
		done <- err
	}()
	close(b.release)
	<-records.entered
	go func() {
		_, err := s.attempt(t.Context(), testRequest(false), late)
		done <- err
	}()
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	close(records.release)
	testenv.Must(t, <-done)
	testenv.Must(t, <-done)
	a, l := starter.told(t), late.told(t)
	if a.Trace == nil || l.Trace == nil || *a.Trace != *l.Trace || a.ID != l.ID {
		t.Errorf("the starter was told %+v and the job that joined while it published %+v; want the same trace", a, l)
	}
}

// TestSharingTraceWriteFails checks that an attempt whose trace can't be stored still records its result, naming no
// trace, and says why in its cache write span.
func TestSharingTraceWriteFails(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	m := cache.NewMemory()
	s := NewSharing(b, refusingAll{m}, m)
	out := &tracedOutputs{}
	close(b.release)
	r, err := s.attempt(t.Context(), testRequest(false), out)
	testenv.Must(t, err)
	if r.Trace != nil {
		t.Errorf("the result names the trace %v, which wasn't stored", r.Trace)
	}
	recorded, err := m.Result(t.Context(), testRequest(false).Key)
	if err != nil || recorded == nil {
		t.Fatalf("the result recorded = %+v, %v; want it recorded", recorded, err)
	}
	a := out.told(t)
	if a.Trace != nil || len(a.Write) != 1 || a.Write[0].Args["trace error"] == nil {
		t.Errorf("the job was told %+v; want no trace, and a cache write saying why", a)
	}
}
