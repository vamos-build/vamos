package sched

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// executorProcess names the executor's spans' process in a trace.
const executorProcess = "executor"

// traceSaveTimeout bounds storing a job's manifest or an attempt's trace, which goes ahead after the job or attempt
// is cancelled, so that a store that doesn't answer delays its end by no more.
const traceSaveTimeout = 10 * time.Second

// JobTrace is what a job records of its trace: its own spans, always, and what became of each node that went
// through the cache, which Save stores as the job's manifest, an api.JobTrace.
// Its methods may be called from several goroutines.
type JobTrace struct {
	recorder *tracing.Recorder

	mu      sync.Mutex
	entries []api.TraceEntry
	index   map[graph.Ref]int // of each node's entry in entries
}

type jobTraceKey struct{}

// ForJob returns a context whose spans the job records, always, and the JobTrace that holds them,
// which the job saves before it returns its outcome.
func ForJob(ctx context.Context) (context.Context, *JobTrace) {
	t := &JobTrace{recorder: tracing.NewRecorder(executorProcess, nil)}
	ctx = tracing.WithRecorder(ctx, t.recorder, "")
	return context.WithValue(ctx, jobTraceKey{}, t), t
}

// jobTraceOf returns the JobTrace of the job whose context ctx is, or nil outside a job, as in tests that run a
// Scheduler alone.
func jobTraceOf(ctx context.Context) *JobTrace {
	t, _ := ctx.Value(jobTraceKey{}).(*JobTrace)
	return t
}

// Record records e, what became of a node, in place of any entry for the same node,
// as a regeneration's miss replaces the hit recorded first.
func (t *JobTrace) Record(e api.TraceEntry) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if i, ok := t.index[e.Node]; ok {
		t.entries[i] = e
		return
	}
	if t.index == nil {
		t.index = map[graph.Ref]int{}
	}
	t.index[e.Node] = len(t.entries)
	t.entries = append(t.entries, e)
}

// Save stores the job's manifest, its spans with each node's entry, in store, under ctx without its cancellation and
// within traceSaveTimeout, so that a cancelled job's manifest is stored too, and returns its blob;
// nil, with why, if it couldn't be stored, which leaves the job's outcome as it is.
// The job's spans must have ended: it is called last, before the job returns its outcome.
func (t *JobTrace) Save(ctx context.Context, store api.BlobPutter) (*cache.Blob, error) {
	spans, lost := t.recorder.Snapshot()
	t.mu.Lock()
	entries := slices.Clone(t.entries)
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceSaveTimeout)
	defer cancel()
	b, err := api.JobTrace{Spans: spans, Entries: entries}.Save(ctx, store, lost)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// RunJob runs work, a job's, recording its trace, always, and stores the job's manifest once work returns, before
// the job does, naming it in the outcome; a manifest that can't be stored, or a content store that can't be opened,
// leaves the outcome naming none, and otherwise as it is, and is reported to warn, if it isn't nil.
func RunJob(ctx context.Context, content func() (cache.ContentStore, error), warn func(error),
	work func(context.Context) api.Outcome) api.Outcome {
	ctx, trace := ForJob(ctx)
	outcome := work(ctx)
	var store api.BlobPutter
	if opened, err := content(); err != nil {
		store = refusing{err}
	} else {
		store = opened
	}
	var err error
	outcome.Trace, err = trace.Save(ctx, store)
	if err != nil && warn != nil {
		warn(fmt.Errorf("storing the job's trace: %w", err))
	}
	return outcome
}

// refusing is a content store that can't be opened, whose puts fail with why.
type refusing struct{ err error }

func (r refusing) PutBytes(context.Context, []byte) (cache.Blob, error) { return cache.Blob{}, r.err }

// Traced is an Output that a Runner tells of the attempt that served it, once the job is done with the attempt:
// as Joiner is told whose attempt runs it, before.
type Traced interface {
	Traced(t AttemptTrace)
}

// AttemptTrace is what a job learns of the trace of an attempt that served it.
type AttemptTrace struct {
	ID string // the attempt's
	// Trace is the blob of the attempt's stored trace, or nil if the job left the attempt running, or the trace
	// couldn't be stored.
	Trace *cache.Blob
	Left  bool // the job left the attempt running, and learned only its ID
	// Write is the span of the attempt's cache write, which isn't in the trace, since it names the trace's blob:
	// the job that started the attempt records it as its own, and a job that joined it beside the attempt's spans.
	Write []tracing.Span
}

// traced tells ev, if it is Traced, of the attempt that served it.
func traced(ev Output, t AttemptTrace) {
	if tr, ok := ev.(Traced); ok {
		tr.Traced(t)
	}
}

// sharedTrack returns the track that a job that joined the attempt serving the target desc puts the attempt's spans
// on, apart from the target's own, since the job that started it has the spans that hold them.
func sharedTrack(desc string) string {
	return fmt.Sprintf("%s (shared)", desc)
}

// entry returns the manifest entry of node, served by the attempt t, which the job started if starter is set.
func (t AttemptTrace) entry(node graph.Ref, desc string, starter bool) api.TraceEntry {
	e := api.TraceEntry{Node: node, Desc: desc, Outcome: api.NodeJoined, Attempt: t.ID, Left: t.Left}
	if starter {
		e.Outcome, e.Started = api.NodeMiss, true
	}
	e.Trace = cloneBlob(t.Trace)
	return e
}
