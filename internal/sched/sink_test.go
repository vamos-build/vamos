package sched

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// sinkJobKey names, in a test job's context, which job it is, so that a sink can tell whose context it was given.
type sinkJobKey struct{}

// sunk is one call that a fakeSink was given.
type sunk struct {
	method     string // Attempt or Job
	job        any    // the sinkJobKey of the context it was given
	id         string // the attempt's, for Attempt
	spans      []tracing.Span
	start, end time.Time
	outcome    string
}

// fakeSink is a SpanSink that keeps what it is told.
type fakeSink struct {
	mu    sync.Mutex
	calls []sunk
}

func (f *fakeSink) record(c sunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeSink) Attempt(ctx context.Context, id string, spans []tracing.Span, start, end time.Time) {
	f.record(sunk{method: "Attempt", job: ctx.Value(sinkJobKey{}), id: id, spans: spans, start: start, end: end})
}

func (f *fakeSink) Job(ctx context.Context, spans []tracing.Span, start, end time.Time, outcome string) {
	f.record(sunk{method: "Job", job: ctx.Value(sinkJobKey{}), spans: spans, start: start, end: end,
		outcome: outcome})
}

// told returns the calls the sink was given of method.
func (f *fakeSink) told(method string) []sunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []sunk
	for _, c := range f.calls {
		if c.method == method {
			calls = append(calls, c)
		}
	}
	return calls
}

// withoutWrite returns the spans an attempt's sink was told of but its cache write, failing unless it ends with one.
func withoutWrite(t *testing.T, spans []tracing.Span) []tracing.Span {
	t.Helper()
	if len(spans) == 0 || spans[len(spans)-1].Name != "cache write" {
		t.Fatalf("the sink was told of %v; want them to end with the attempt's cache write", spanNames(spans))
	}
	return spans[:len(spans)-1]
}

// sameSpans reports whether a and b encode alike, as stored traces hold spans.
func sameSpans(t *testing.T, a, b []tracing.Span) bool {
	t.Helper()
	x, err := json.Marshal(a)
	testenv.Must(t, err)
	y, err := json.Marshal(b)
	testenv.Must(t, err)
	return string(x) == string(y)
}

// storedAttempt returns the spans of the attempt trace b in store.
func storedAttempt(t *testing.T, store cache.ContentStore, b *cache.Blob) []tracing.Span {
	t.Helper()
	if b == nil {
		t.Fatal("no attempt trace was stored")
	}
	data, err := store.Read(context.Background(), *b, tracing.MaxTraceSize)
	testenv.Must(t, err)
	a, err := tracing.DecodeAttempt(data)
	testenv.Must(t, err)
	return a.Spans
}

// sinkOrderOutput is a job's Output that notes, as it is told of the attempt's end, whether the sink was told of the
// attempt's spans already.
type sinkOrderOutput struct {
	tracedOutputs
	sink          *fakeSink
	sinkToldFirst bool
}

func (o *sinkOrderOutput) Traced(t AttemptTrace) {
	o.sinkToldFirst = len(o.sink.told("Attempt")) == 1
	o.tracedOutputs.Traced(t)
}

// TestSharingTellsSink checks that Sharing tells its sink of an attempt's spans as its runner returns, under the
// context of the job that started it, though that job has left it; and that the spans are those stored, and the sink
// is told of them before a job is told the attempt ended.
func TestSharingTellsSink(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	m := cache.NewMemory()
	sink := &fakeSink{}
	s := NewSharing(b, m, m, WithSpanSink(sink))
	starterCtx, cancel := context.WithCancel(context.WithValue(t.Context(), sinkJobKey{}, "starter"))
	starter, joiner := &tracedOutputs{}, &sinkOrderOutput{sink: sink}
	starterDone, joinerDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.attempt(starterCtx, testRequest(false), starter)
		starterDone <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	go func() {
		_, err := s.attempt(context.WithValue(t.Context(), sinkJobKey{}, "joiner"), testRequest(false), joiner)
		joinerDone <- err
	}()
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	cancel()
	if err := <-starterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the starter got %v, want %v", err, context.Canceled)
	}
	close(b.release)
	testenv.Must(t, <-joinerDone)

	whole := joiner.told(t)
	attempts := sink.told("Attempt")
	if len(attempts) != 1 || attempts[0].id != whole.ID || attempts[0].job != "starter" {
		t.Fatalf("the sink was told of %+v; want attempt %s, once, under the starter's context, though it left",
			attempts, whole.ID)
	}
	stored := storedAttempt(t, m, whole.Trace)
	if a := attempts[0]; !sameSpans(t, withoutWrite(t, a.spans), stored) || a.start.IsZero() ||
		a.end.Before(a.start) {
		t.Errorf("the sink was told of spans %v from %s to %s; want the stored trace's, %v", spanNames(a.spans),
			a.start, a.end, spanNames(stored))
	}
	if !joiner.sinkToldFirst {
		t.Error("the joiner was told the attempt ended before the sink was told of its spans")
	}
	if left := starter.told(t); !left.Left {
		t.Errorf("the starter that left was told %+v; want that it left", left)
	}
}

// TestSharingTellsSinkTrimmed checks that the sink is told of an attempt's spans as its stored trace, cut short by
// LimitTraces, holds them, the instant that says so included.
func TestSharingTellsSinkTrimmed(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	m := cache.NewMemory()
	sink := &fakeSink{}
	s := NewSharing(b, m, m, WithSpanSink(sink))
	s.LimitTraces(300) // room for the document and one instant, not both of the runner's
	close(b.release)
	out := &tracedOutputs{}
	_, err := s.attempt(t.Context(), testRequest(false), out)
	testenv.Must(t, err)
	stored := storedAttempt(t, m, out.told(t).Trace)
	attempts := sink.told("Attempt")
	if len(attempts) != 1 || !sameSpans(t, withoutWrite(t, attempts[0].spans), stored) {
		t.Fatalf("the sink was told of %v; want the stored trace's %v", attempts, spanNames(stored))
	}
	if len(stored) == 0 || stored[len(stored)-1].Name != tracing.Truncated {
		t.Errorf("the stored trace holds %v; want it cut short, saying so", spanNames(stored))
	}
}

// TestRunJobTellsSink checks that RunJob tells its sink of the job's spans as its manifest holds them, with how it
// ended, under the job's context, and that the manifest holds how the context says the job's trace is exported;
// and that a nil sink is told nothing.
func TestRunJobTellsSink(t *testing.T) {
	m := cache.NewMemory()
	exported := tracing.Exported{TraceID: "t", SpanID: "s", Sampled: true}
	ctx := tracing.WithExported(context.WithValue(t.Context(), sinkJobKey{}, "job"), exported)
	failure := &api.Failure{Message: "signal: killed"}
	for _, c := range []struct {
		outcome api.Outcome
		cancel  bool // the job's context ends as it works
		want    string
	}{
		{outcome: api.Outcome{}, want: JobSucceeded},
		{outcome: api.Outcome{Failure: failure}, want: JobFailed},
		// Cancelled as its context ends, whatever its failure says, as its target spans are.
		{outcome: api.Outcome{Failure: failure}, cancel: true, want: JobCancelled},
		{outcome: api.Outcome{Failure: failure, Cancelled: true}, cancel: true, want: JobCancelled},
		// Failed while its context lives, though an error of its own wrapped context.Canceled.
		{outcome: api.Outcome{Failure: failure, Cancelled: true}, want: JobFailed},
	} {
		sink := &fakeSink{}
		jctx, cancel := context.WithCancel(ctx)
		outcome := RunJob(jctx, func() (cache.ContentStore, error) { return m, nil }, nil, sink,
			func(ctx context.Context) api.Outcome {
				_, span := tracing.Start(ctx, "work")
				span.End()
				if c.cancel {
					cancel()
				}
				return c.outcome
			})
		cancel()
		data, err := m.Read(t.Context(), *outcome.Trace, tracing.MaxTraceSize)
		testenv.Must(t, err)
		manifest, err := api.DecodeJobTrace(data)
		testenv.Must(t, err)
		jobs := sink.told("Job")
		if len(jobs) != 1 || jobs[0].job != "job" || jobs[0].outcome != c.want ||
			!sameSpans(t, jobs[0].spans, manifest.Spans) || len(manifest.Spans) != 1 ||
			jobs[0].end.Before(jobs[0].start) {
			t.Errorf("the sink was told of %+v; want the manifest's spans %v, ended %s, under the job's context",
				jobs, spanNames(manifest.Spans), c.want)
		}
		if manifest.Exported != exported {
			t.Errorf("the manifest says the trace was exported as %+v; want %+v", manifest.Exported, exported)
		}
	}
	outcome := RunJob(t.Context(), func() (cache.ContentStore, error) { return m, nil }, nil, nil,
		func(context.Context) api.Outcome { return api.Outcome{} })
	data, err := m.Read(t.Context(), *outcome.Trace, tracing.MaxTraceSize)
	testenv.Must(t, err)
	if manifest, err := api.DecodeJobTrace(data); err != nil || manifest.Exported != (tracing.Exported{}) {
		t.Errorf("a job not exported stored %+v, %v; want no Exported", manifest.Exported, err)
	}
}

// TestSinkToldWhenStoringFails checks that the sink is told of a job's and an attempt's spans though their traces
// can't be stored, and that the job's outcome and the attempt's result are what they would have been.
func TestSinkToldWhenStoringFails(t *testing.T) {
	m := cache.NewMemory()
	sink := &fakeSink{}
	var warned []error
	outcome := RunJob(t.Context(), func() (cache.ContentStore, error) { return refusingAll{m}, nil },
		func(err error) { warned = append(warned, err) }, sink,
		func(ctx context.Context) api.Outcome {
			_, span := tracing.Start(ctx, "work")
			span.End()
			return api.Outcome{Results: []api.Result{{}}}
		})
	jobs := sink.told("Job")
	if outcome.Trace != nil || len(outcome.Results) != 1 || len(warned) != 1 || len(jobs) != 1 ||
		!slices.Equal(spanNames(jobs[0].spans), []string{"work"}) || jobs[0].outcome != JobSucceeded {
		t.Errorf("a job whose manifest can't be stored returned %+v, warned %v, and told the sink %+v; want its "+
			"results, a warning, and its spans", outcome, warned, jobs)
	}

	b := &blockingRunner{release: make(chan struct{})}
	close(b.release)
	s := NewSharing(b, refusingAll{m}, m, WithSpanSink(sink))
	out := &tracedOutputs{}
	if _, err := s.attempt(t.Context(), testRequest(false), out); err != nil {
		t.Fatal(err)
	}
	attempts := sink.told("Attempt")
	want := []string{"started", "released", "cache write"}
	if len(attempts) != 1 || !slices.Equal(spanNames(attempts[0].spans), want) ||
		out.told(t).Trace != nil {
		t.Errorf("an attempt whose trace can't be stored told the sink %+v; want its spans", attempts)
	}
}

// failingResults is a record store whose SetResult fails, as one that is full does.
type failingResults struct{ cache.RecordStore }

func (failingResults) SetResult(context.Context, digest.Digest, *cache.ActionResult) error {
	return errors.New("the records are full")
}

// TestSinkToldOfCacheWrite checks that the sink is told of an attempt's cache write within the attempt, which ends
// once its result is recorded, and of one that failed, saying so, while the job is told of the failure.
func TestSinkToldOfCacheWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		b := &blockingRunner{release: make(chan struct{})}
		close(b.release)
		m := cache.NewMemory()
		var records cache.RecordStore = m
		if fail {
			records = failingResults{m}
		}
		sink := &fakeSink{}
		s := NewSharing(b, m, records, WithSpanSink(sink))
		_, err := s.attempt(t.Context(), testRequest(false), &tracedOutputs{})
		if (err != nil) != fail {
			t.Fatalf("recording failing %v: the job got %v", fail, err)
		}
		attempts := sink.told("Attempt")
		if len(attempts) != 1 {
			t.Fatalf("the sink was told of %d attempts; want 1", len(attempts))
		}
		a := attempts[0]
		write := withoutWrite(t, a.spans) // fails unless it ends with the write
		w := a.spans[len(write)]
		if w.Start.Before(a.start) || w.End.After(a.end) {
			t.Errorf("the cache write ran from %s to %s, outside the attempt's %s to %s", w.Start, w.End, a.start,
				a.end)
		}
		if failed := w.Args["error"] != nil; failed != fail {
			t.Errorf("recording failing %v: the cache write says %v", fail, w.Args)
		}
	}
}

// cancellingStore is a content store that cancels a job's context as its manifest is stored.
type cancellingStore struct {
	cache.ContentStore
	cancel context.CancelFunc
}

func (c cancellingStore) PutBytes(ctx context.Context, data []byte) (cache.Blob, error) {
	c.cancel()
	return c.ContentStore.PutBytes(ctx, data)
}

// TestRunJobOutcomeAsWorkReturns checks that a job that failed is told to the sink as failed though its context is
// cancelled as its manifest is stored, after the work returned.
func TestRunJobOutcomeAsWorkReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &fakeSink{}
	store := cancellingStore{ContentStore: cache.NewMemory(), cancel: cancel}
	RunJob(ctx, func() (cache.ContentStore, error) { return store, nil }, nil, sink,
		func(context.Context) api.Outcome { return api.Outcome{Failure: &api.Failure{Message: "make failed"}} })
	if jobs := sink.told("Job"); len(jobs) != 1 || jobs[0].outcome != JobFailed {
		t.Errorf("the sink was told %+v; want the job failed, as it was when its work returned", jobs)
	}
}
